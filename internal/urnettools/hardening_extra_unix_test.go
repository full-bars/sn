//go:build unix

package urnettools

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A FIFO with no writer must be rejected promptly: os.Stat accepted it and
// the later os.ReadFile blocked forever waiting for input.
func TestReadSessionLoadFileRejectsFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "bundle.enc")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create fifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readSessionLoadFile(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO: got %v, want a not-a-regular-file error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readSessionLoadFile blocked on a FIFO")
	}
}
