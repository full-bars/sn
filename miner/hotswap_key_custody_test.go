package miner

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

type fakeKeyCustody struct {
	held         bool
	releaseErr   error
	retakeFails  int
	retakeCalls  int
	releaseCalls int
}

func (f *fakeKeyCustody) ReleaseForHotSwap() error {
	f.releaseCalls++
	if f.releaseErr != nil {
		return f.releaseErr
	}
	f.held = false
	return nil
}

func (f *fakeKeyCustody) ReacquireAfterAbortedHotSwap() error {
	f.retakeCalls++
	if f.retakeCalls <= f.retakeFails {
		return errors.New("lock still held by the candidate")
	}
	f.held = true
	return nil
}

func useFakeKeyCustody(t *testing.T, fake *fakeKeyCustody) {
	t.Helper()
	t.Setenv("URNETWORK_STATE_DIR", shortStateDir(t))
	closeProviderStore()
	t.Cleanup(closeProviderStore)
	previousWait, previousPoll := keyCustodyRetakeWait, keyCustodyRetakePoll
	keyCustodyRetakeWait, keyCustodyRetakePoll = 200*time.Millisecond, time.Millisecond
	setHotSwapKeyCustody(fake)
	t.Cleanup(func() {
		setHotSwapKeyCustody(nil)
		keyCustodyRetakeWait, keyCustodyRetakePoll = previousWait, previousPoll
	})
}

// The candidate opens the provider key right after TAKEOVER and takes its
// owner lock without waiting, so the lock must be free before TAKEOVER leaves.
func TestHandoffBatonReleasesTheKeyLockBeforeTakeover(t *testing.T) {
	fake := &fakeKeyCustody{held: true}
	useFakeKeyCustody(t, fake)

	parentConn, candidateConn := net.Pipe()
	t.Cleanup(func() { _ = candidateConn.Close() })
	session := &HotswapParentSession{Reader: bufio.NewReader(parentConn), Writer: parentConn}

	heldAtTakeover := make(chan bool, 1)
	go func() {
		msg, err := readHotswapMessage(bufio.NewReader(candidateConn))
		if err != nil || msg.Type != HotswapMsgTakeover {
			heldAtTakeover <- true
			return
		}
		heldAtTakeover <- fake.held
		_, _ = io.WriteString(candidateConn, `{"type":"ACK"}`+"\n")
	}()

	if err := handoffBatonSendTakeover(session, os.Getpid(), 4301); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if <-heldAtTakeover {
		t.Fatal("the parent still held the key lock when TAKEOVER was sent")
	}
	if fake.retakeCalls != 0 {
		t.Fatalf("the parent took the key lock back after a confirmed takeover (%d calls)", fake.retakeCalls)
	}
}

// An aborted handoff leaves the parent serving, and it must own the key lock
// again once the killed candidate has exited.
func TestHandoffBatonTakesTheKeyLockBackOnAbort(t *testing.T) {
	fake := &fakeKeyCustody{held: true, retakeFails: 3}
	useFakeKeyCustody(t, fake)

	session := &HotswapParentSession{Writer: brokenIPCEWriter{errors.New("broken pipe")}}
	if err := handoffBatonSendTakeover(session, os.Getpid(), 4302); err == nil {
		t.Fatal("the handoff was expected to abort when the TAKEOVER write fails")
	}
	if !fake.held {
		t.Fatalf("the parent did not take the key lock back after the abort (%d tries)", fake.retakeCalls)
	}
	if fake.retakeCalls != 4 {
		t.Fatalf("retake calls = %d, want 4 (three refusals and one success)", fake.retakeCalls)
	}
}

// A lock that cannot be released stops the handoff before the candidate is told
// to take over.
func TestHandoffBatonAbortsWhenTheKeyLockCannotBeReleased(t *testing.T) {
	fake := &fakeKeyCustody{held: true, releaseErr: errors.New("unlock failed")}
	useFakeKeyCustody(t, fake)

	session := &HotswapParentSession{Writer: failTestWriter{t}}
	if err := handoffBatonSendTakeover(session, os.Getpid(), 4303); err == nil {
		t.Fatal("the handoff was expected to abort when the key lock stays held")
	}
	if fake.retakeCalls != 0 {
		t.Fatalf("retake calls = %d, want 0: the lock was never released", fake.retakeCalls)
	}
}

type failTestWriter struct{ t *testing.T }

func (w failTestWriter) Write([]byte) (int, error) {
	w.t.Fatal("TAKEOVER was sent although the key lock was not released")
	return 0, nil
}
