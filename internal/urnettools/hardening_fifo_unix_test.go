//go:build unix

package urnettools

// TestWriteStateFileRejectsFIFO: writeStateFile must not block forever on a
// planted FIFO (a plain O_WRONLY open of a FIFO with no reader blocks).
// The helper opens with O_NONBLOCK to detect FIFOs, so this must return an
// error quickly instead of hanging the caller (root) indefinitely. Unix-only
// because FIFOs require mkfifo(2).

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWriteStateFileRejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "target")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create fifo: %v", err)
	}
	if err := writeStateFile(dir, "target", []byte("x"), 0o600); err == nil {
		t.Fatal("writeStateFile on a FIFO returned nil; must refuse (O_NONBLOCK open fails) instead of blocking forever")
	}
}
