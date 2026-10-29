package miner

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"testing"

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
