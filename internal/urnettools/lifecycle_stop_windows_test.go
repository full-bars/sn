//go:build windows

package urnettools

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCmdStopWindows_PIDZero polls controlSocketReachable to confirm
// the provider actually shut down before reporting success, instead of
// assuming socket unreachable == stopped.
func TestCmdStopWindows_PIDZero(t *testing.T) {
	// Create a temp state dir and a mock socket that responds to
	// shutdown and then closes itself after a short delay.
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "provider.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Background goroutine: accept one connection, read the shutdown
	// request, respond ok, then close the listener after a short delay
	// (simulating the provider shutting down).
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read and discard the request.
		buf := make([]byte, 4096)
		conn.Read(buf)
		resp := controlResponse{OK: true, Value: "shutting down"}
		json.NewEncoder(conn).Encode(resp)
		// Wait a bit then close the listener so the socket disappears.
		time.Sleep(100 * time.Millisecond)
		ln.Close()
	}()

	p := Provider{
		StateDir: tmpDir,
		PID:      0, // Zero PID forces socket-poll path
	}

	err = cmdStopWindows(p, false, false)
	if err != nil {
		t.Fatalf("cmdStopWindows: %v", err)
	}

	// The socket should no longer exist (listener was closed).
	if _, statErr := os.Stat(sockPath); statErr == nil {
		t.Fatal("socket still exists after cmdStopWindows with PID=0")
	}
}
