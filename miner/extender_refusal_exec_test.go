package miner

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
)

// The container path replaces the process image, so a refusal only memory
// holds is lost there. It must stop at the same gate as the baton handoff and
// leave the live provider running.
func TestContainerExecRefusesWhileARefusalIsOnlyInMemory(t *testing.T) {
	testExtenderState(t)
	ResetHotSwapStateForTest()
	t.Cleanup(ResetHotSwapStateForTest)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	failJournalRename(t)
	releaseProviderStore()
	holdExtenderRefusal(extenderTestAccountA, "scope.1601")

	origPid, origSpawn, origExec, origRestore := getpidFunc, spawnCandidateFunc, execInPlaceFunc, restoreStdioBeforeExecFunc
	t.Cleanup(func() {
		getpidFunc, spawnCandidateFunc, execInPlaceFunc, restoreStdioBeforeExecFunc = origPid, origSpawn, origExec, origRestore
	})
	getpidFunc = func() int { return 1 }
	var ready bytes.Buffer
	if err := writeHotswapMessage(&ready, HotswapMessage{Type: HotswapMsgReady, PID: 4400, Version: RequireVersion()}); err != nil {
		t.Fatal(err)
	}
	spawnCandidateFunc = func(string, []string) (*HotswapParentSession, error) {
		return &HotswapParentSession{Reader: bufio.NewReader(bytes.NewReader(ready.Bytes())), Writer: io.Discard}, nil
	}
	execed := false
	execInPlaceFunc = func(string, []string, []string) error { execed = true; return nil }
	restoreStdioBeforeExecFunc = func() {}

	err := runHotSwapParentHandoff(context.Background(), func() {}, docopt.Opts{})
	if err == nil {
		t.Fatal("the container handoff went ahead with a refusal only memory holds")
	}
	if execed {
		t.Fatal("execve ran with a decided refusal only in memory")
	}
}

// containerHandoffForTest stubs a container handoff whose canary is ready at
// once. onCanaryDone runs when the parent tells the canary to stop, which is
// after the gate; exec is what execve would have run.
func containerHandoffForTest(t *testing.T, onCanaryDone func(), exec func() error) {
	t.Helper()
	ResetHotSwapStateForTest()
	t.Cleanup(ResetHotSwapStateForTest)
	origPid, origSpawn, origExec, origRestore := getpidFunc, spawnCandidateFunc, execInPlaceFunc, restoreStdioBeforeExecFunc
	t.Cleanup(func() {
		getpidFunc, spawnCandidateFunc, execInPlaceFunc, restoreStdioBeforeExecFunc = origPid, origSpawn, origExec, origRestore
	})
	getpidFunc = func() int { return 1 }
	var ready bytes.Buffer
	if err := writeHotswapMessage(&ready, HotswapMessage{Type: HotswapMsgReady, PID: 4400, Version: RequireVersion()}); err != nil {
		t.Fatal(err)
	}
	spawnCandidateFunc = func(string, []string) (*HotswapParentSession, error) {
		return &HotswapParentSession{Reader: bufio.NewReader(bytes.NewReader(ready.Bytes())), Writer: writerFunc(func([]byte) { onCanaryDone() })}, nil
	}
	execInPlaceFunc = func(string, []string, []string) error { return exec() }
	restoreStdioBeforeExecFunc = func() {}
}

type writerFunc func([]byte)

func (write writerFunc) Write(data []byte) (int, error) { write(data); return len(data), nil }

// A refusal decided after the gate that no file takes is still only in memory
// at the last check. The exec must stop there, before anything is torn down,
// and leave the live provider serving.
func TestContainerExecStopsBeforeTeardownWhenALateRefusalIsUnjournaled(t *testing.T) {
	testExtenderState(t)
	shortenRefusalGate(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	failJournalRename(t)
	var yielded, execed atomic.Bool
	containerHandoffForTest(t, func() {
		releaseProviderStore()
		holdExtenderRefusal(extenderTestAccountA, "scope.2301")
	}, func() error { execed.Store(true); return nil })
	t.Cleanup(RegisterCoordinatorCloser(func() { yielded.Store(true) }))

	err := runHotSwapParentHandoff(context.Background(), func() {}, docopt.Opts{})
	if err == nil || execed.Load() {
		t.Fatalf("exec ran (%t, err %v) with a late refusal only in memory", execed.Load(), err)
	}
	if yielded.Load() {
		t.Fatal("the coordinator was yielded before the exec was abandoned: the provider is not retained")
	}
}

// The barrier is held from the last check to execve, so a callback that
// arrives in between cannot queue a refusal the exec then discards.
func TestContainerExecHoldsCallbacksFromTheLastCheckToExec(t *testing.T) {
	testExtenderState(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	var queuedBeforeExec atomic.Int64
	queuedBeforeExec.Store(-1)
	late := make(chan struct{})
	containerHandoffForTest(t, func() {}, func() error {
		go func() {
			holdExtenderRefusal(extenderTestAccountA, "scope.2401")
			close(late)
		}()
		time.Sleep(100 * time.Millisecond)
		queuedBeforeExec.Store(int64(extenderRefusalsHeldCount()))
		return nil
	})
	if err := runHotSwapParentHandoff(context.Background(), func() {}, docopt.Opts{}); err != nil {
		t.Fatal(err)
	}
	<-late
	if got := queuedBeforeExec.Load(); got != 0 {
		t.Fatalf("a callback queued %d refusals between the last check and exec", got)
	}
}

// A refusal decided by coordinator teardown must prevent exec if it cannot be saved.
func TestContainerExecAbortsWhenTeardownRefusalCannotBeJournaled(t *testing.T) {
	testExtenderState(t)
	shortenRefusalGate(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	failJournalRename(t)
	var yielded, execed atomic.Bool
	containerHandoffForTest(t, func() {}, func() error { execed.Store(true); return nil })
	t.Cleanup(RegisterCoordinatorCloser(func() {
		yielded.Store(true)
		releaseProviderStore()
		holdExtenderRefusal(extenderTestAccountA, "scope.2501")
	}))
	err := runHotSwapParentHandoff(context.Background(), func() {}, docopt.Opts{})
	if !yielded.Load() {
		t.Fatal("coordinator closer was not called")
	}
	if err == nil || execed.Load() {
		t.Fatalf("exec proceeded with unsaved teardown refusal: exec=%t err=%v", execed.Load(), err)
	}
}
