package miner

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urfoundation/sn/internal/durablesync"
	"github.com/urnetwork/sdk"
)

// failJournalRename makes the journal's publishing rename fail until the
// returned function is called, the failure a full or faulty state directory
// gives. It is switched by an atomic so a worker goroutine can run through it.
func failJournalRename(t *testing.T) (heal func()) {
	t.Helper()
	original := providerStoreDurableRename
	var failing atomic.Bool
	failing.Store(true)
	providerStoreDurableRename = func(from, to string) error {
		if failing.Load() {
			return os.ErrPermission
		}
		return original(from, to)
	}
	t.Cleanup(func() { providerStoreDurableRename = original })
	return func() { failing.Store(false) }
}

// A refusal that the store cannot take and the journal cannot hold is only in
// this process. The handoff must not release the store and send TAKEOVER on
// top of it, or the candidate admits into a wait it cannot see; once the
// journal works the same handoff goes through and the candidate's process
// finds the refusal.
func TestHandoffWaitsForARefusalOnlyMemoryHolds(t *testing.T) {
	testExtenderState(t)
	dir, err := providerStateDir()
	if err != nil {
		t.Fatal(err)
	}
	// The store cannot open: its name is taken by a directory.
	if err := os.Mkdir(filepath.Join(dir, providerStoreFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	heal := failJournalRename(t)
	holdExtenderRefusal(extenderTestAccountA, "scope.1101")

	var sent bytes.Buffer
	session := &HotswapParentSession{Reader: bufio.NewReader(bytes.NewReader(nil)), Writer: &sent}
	if err := handoffBatonSendTakeover(session, os.Getpid(), 4300); err == nil {
		t.Fatal("handoff went ahead with a refusal neither committed nor journaled")
	}
	if sent.Len() != 0 {
		t.Fatalf("TAKEOVER was sent (%q) while a refusal was only in memory", sent.String())
	}

	// The journal recovers; the store is still down, so the file is what the
	// candidate has.
	heal()
	parentConn, candidateConn := net.Pipe()
	t.Cleanup(func() { _ = candidateConn.Close() })
	session = &HotswapParentSession{Reader: bufio.NewReader(parentConn), Writer: parentConn}
	go func() {
		if _, err := readHotswapMessage(bufio.NewReader(candidateConn)); err == nil {
			_, _ = io.WriteString(candidateConn, `{"type":"ACK"}`+"\n")
		}
	}()
	if err := handoffBatonSendTakeover(session, os.Getpid(), 4301); err != nil {
		t.Fatalf("handoff with a journaled refusal: %v", err)
	}

	// The parent's memory goes with the parent. The candidate's process opens
	// the store and its first admission is held by the file.
	resetExtenderRefusals()
	closeProviderStore()
	if err := os.Remove(filepath.Join(dir, providerStoreFileName)); err != nil {
		t.Fatal(err)
	}
	if allow, wait, _ := extenderActivationAllow(time.Now(), 1, extenderTestAccountA); allow || wait <= 0 {
		t.Fatalf("the successor's admission = %t (wait %s), want held by the transferred refusal", allow, wait)
	}
}

// A callback can decide a refusal after the store went to the candidate, when
// only a file can carry it. The drain must not end the parent while that file
// cannot be written.
func TestDrainWaitsForALateRefusalToBeJournaled(t *testing.T) {
	testExtenderState(t)
	ResetHotSwapStateForTest()
	t.Cleanup(ResetHotSwapStateForTest)
	gate := extenderRefusalGateRetry
	extenderRefusalGateRetry = 10 * time.Millisecond
	t.Cleanup(func() { extenderRefusalGateRetry = gate })
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	heal := failJournalRename(t)
	releaseProviderStore()
	listener := newProviderExtenderStatusListener(nil, 0, extenderTestAccountA)
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, LastActivationTime: 1201, LastActivationError: "refused", LastActivationRefused: true})

	origExit := exitFunc
	t.Cleanup(func() { exitFunc = origExit })
	exited := make(chan struct{})
	exitFunc = func(int) { close(exited) }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handoffDrainParent(ctx, func() {}, &HotswapParentSession{}, os.Getpid())

	if !blockedFor(exited, 300*time.Millisecond) {
		t.Fatal("the parent exited with a decided refusal only in its memory")
	}
	heal()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the parent never exited after the journal recovered")
	}
	if onDisk, _, err := readExtenderRefusalJournal(time.Now()); err != nil || len(onDisk) != 1 {
		t.Fatalf("journal at the parent's exit = %d refusals (%v), want the late refusal", len(onDisk), err)
	}
}

// Where the platform cannot make a rename crash-durable (windows, for a rename
// inside one volume) the journal cannot certify a file, so a refusal is never
// reported durable by it. Only the store's commit counts there, and the
// handoff gate holds until the store takes it.
func TestRefusalIsNotJournaledWhereTheRenameIsNotCrashDurable(t *testing.T) {
	testExtenderState(t)
	original := providerStoreDurableRename
	t.Cleanup(func() { providerStoreDurableRename = original })
	providerStoreDurableRename = func(string, string) error { return durablesync.ErrNotCrashDurable }
	holdExtenderRefusal(extenderTestAccountA, "scope.1301")

	if at := extenderRefusalsAtRisk(); at != 1 {
		t.Fatalf("refusals at risk = %d, want 1: the journal certified a rename the platform cannot", at)
	}
	dir, err := providerStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*extender-refusal-*.json*")); len(left) != 0 {
		t.Fatalf("files left behind by the refused rename: %v", left)
	}
	// The store takes it, which is the one durable place left.
	if err := ensureExtenderRefusalsDurable(); err != nil {
		t.Fatalf("a committed refusal should satisfy the gate: %v", err)
	}
	if got := readRefusals(t, extenderTestAccountA); got != 1 {
		t.Fatalf("refusals = %d, want the refusal committed", got)
	}
}
