//go:build windows

package urnettools

import (
	"errors"
	"os"
	"testing"
)

func TestWindowsHotSwapPreflightAndTriggerDecline(t *testing.T) {
	p := Provider{Version: "v3.23.0-fix.31.0"}
	if err := hotSwapPreflight(p); !errors.Is(err, ErrHotSwapWindows) {
		t.Fatalf("native preflight = %v, want ErrHotSwapWindows", err)
	}
	if supportsHotSwap(p) {
		t.Fatal("Windows supportsHotSwap must decline")
	}
	if err := triggerHotSwap(p); !errors.Is(err, ErrHotSwapWindows) {
		t.Fatalf("triggerHotSwap = %v, want ErrHotSwapWindows", err)
	}
}

// TestWindowsPidLiveness verifies that pidIsAlive correctly reports
// process liveness for the current process and a dead PID.
func TestWindowsPidLiveness(t *testing.T) {
	// The current process is alive.
	if !pidIsAlive(os.Getpid()) {
		t.Fatal("pidIsAlive(currentPID) = false, want true")
	}

	// PID 0 is invalid.
	if pidIsAlive(0) {
		t.Fatal("pidIsAlive(0) = true, want false")
	}

	// Negative PID is invalid.
	if pidIsAlive(-1) {
		t.Fatal("pidIsAlive(-1) = true, want false")
	}

	// A very large PID that doesn't exist.
	if pidIsAlive(999999) {
		t.Fatal("pidIsAlive(999999) = true, want false")
	}
}
