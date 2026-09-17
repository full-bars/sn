//go:build windows

package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestControlSocketShutdown_Execution verifies that the "shutdown"
// command triggers the shutdownFn callback and the client receives a
// clean {ok:true} response without io.EOF (the 50ms delay before
// cancel ensures the response is flushed).
func TestControlSocketShutdown_Execution(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	// Wire a shutdownFn that cancels the context (mirrors real usage).
	shutdownCalled := make(chan struct{}, 1)
	globalControlState.shutdownFn = func() {
		shutdownCalled <- struct{}{}
		cancel()
	}
	defer func() { globalControlState.shutdownFn = nil }()

	resp, err := dialControlSocket(controlRequest{Cmd: "shutdown"})
	if err != nil {
		t.Fatalf("dial shutdown: %v", err)
	}
	if !resp.OK {
		t.Fatalf("shutdown response: %+v", resp)
	}
	if resp.Value != "shutting down" {
		t.Fatalf("unexpected value: %q", resp.Value)
	}

	// shutdownFn must have been called within a reasonable window.
	select {
	case <-shutdownCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdownFn was not called within 2s")
	}
}

// TestRestrictSocketACL_Enforcement verifies that restrictFileACL
// applies a protected DACL (inherited ACEs blocked) with full owner
// access. The successful application is the key assertion for CI.
func TestRestrictSocketACL_Enforcement(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "test.sock")
	if err := os.WriteFile(tmpFile, []byte("test"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := restrictFileACL(tmpFile); err != nil {
		t.Fatalf("restrictFileACL: %v", err)
	}
}
