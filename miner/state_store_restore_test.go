package miner

import (
	"errors"
	"os"
	"testing"
)

func TestHandoffAbortReportsStoreReopenFailure(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	want := errors.New("injected reopen failure")
	previous := reopenProviderStoreAfterAbort
	reopenProviderStoreAfterAbort = func() (*providerStore, error) { return nil, want }
	t.Cleanup(func() { reopenProviderStoreAfterAbort = previous })
	closeProviderStore()
	t.Cleanup(closeProviderStore)
	previousOwner := getActiveHotSwapControlSocket()
	setActiveHotSwapControlSocket(nil)
	t.Cleanup(func() { setActiveHotSwapControlSocket(previousOwner) })
	session := &HotswapParentSession{Writer: brokenIPCEWriter{errors.New("broken pipe")}}
	err := handoffBatonSendTakeover(session, os.Getpid(), 0)
	if !errors.Is(err, want) {
		t.Fatalf("handoff error = %v, want store reopen error %v", err, want)
	}
}
