package miner

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
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
// only a file can carry it. If that file cannot be written the drain waits a
// bounded time and then ends the parent anyway: waiting forever left no
// recovery short of killing it.
func TestDrainEndsAfterTheDeadlineWhenALateRefusalCannotBeJournaled(t *testing.T) {
	testExtenderState(t)
	ResetHotSwapStateForTest()
	t.Cleanup(ResetHotSwapStateForTest)
	shortenRefusalGate(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	failJournalRename(t)
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

	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the parent never exited: the drain waits for a journal that will not come")
	}
	if at := extenderRefusalsAtRisk(); at != 1 {
		t.Fatalf("refusals at risk = %d, want the unwritten refusal still counted", at)
	}
}

// shortenRefusalGate makes the wait for durability finish in milliseconds.
func shortenRefusalGate(t *testing.T) {
	t.Helper()
	retry, deadline := extenderRefusalGateRetry, extenderRefusalGateDeadline
	extenderRefusalGateRetry, extenderRefusalGateDeadline = 5*time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() { extenderRefusalGateRetry, extenderRefusalGateDeadline = retry, deadline })
}

// Shutdown with a store that is gone and a journal that cannot be written
// returns once the deadline passes, says what was lost in the log, and records
// it in events.log where the next start can find it.
func TestShutdownIsBoundedWhenNothingCanHoldTheRefusal(t *testing.T) {
	testExtenderState(t)
	shortenRefusalGate(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	failJournalRename(t)
	releaseProviderStore()
	holdExtenderRefusal(extenderTestAccountA, "scope.1401")

	done := make(chan bool, 1)
	go func() { done <- waitExtenderRefusalsDurable("shutdown") }()
	select {
	case durable := <-done:
		if durable {
			t.Fatal("reported durable with the store gone and the journal failing")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown never finished: the wait is unbounded")
	}
	path, err := critLogPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "EXTENDER REFUSALS LOST at shutdown") {
		t.Fatalf("events.log = %q (%v), want the lost refusal recorded", data, err)
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

// queueAtRiskRefusal holds a refusal in memory only, without the barrier, the
// state a callback leaves when neither the store nor the journal took it.
func queueAtRiskRefusal(attempt string) {
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	queueExtenderOpLocked(extenderPendingOp{kind: extenderOpRefusal, account: extenderTestAccountA, attempt: attempt, at: time.Now()})
}

// A callback that holds the barrier through slow I/O must not hold shutdown
// past its deadline: the wait ends at the deadline, not when the barrier is
// released.
func TestShutdownDeadlineHoldsWhileTheBarrierIsHeld(t *testing.T) {
	testExtenderState(t)
	shortenRefusalGate(t)
	queueAtRiskRefusal("scope.1701")
	release, err := lockExtenderRefusalBarrier()
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	time.AfterFunc(500*time.Millisecond, func() { release(); close(released) })
	t.Cleanup(func() { <-released; extenderDurableAttempts.Wait() })

	started := time.Now()
	durable := waitExtenderRefusalsDurable("shutdown")
	elapsed := time.Since(started)
	if durable {
		t.Fatal("reported durable while a refusal was only in memory")
	}
	if elapsed >= 400*time.Millisecond {
		t.Fatalf("wait took %s with a 150ms deadline: it waited for the barrier, not the clock", elapsed)
	}
}
