//go:build unix

package urnettools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestWriteStateFileDoesNotCloseReusedFd: writeStateFile once closed its fd
// directly and also through the *os.File wrapping it. The File's finalizer
// then closed whichever descriptor had reused the number, which surfaced as
// "bad file descriptor" in unrelated tests sharing the process.
func TestWriteStateFileDoesNotCloseReusedFd(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 50; i++ {
		if err := writeStateFile(dir, "x", []byte("hi"), 0o600); err != nil {
			t.Fatal(err)
		}
		victim, err := os.Open(filepath.Join(dir, "x"))
		if err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		runtime.GC()
		buf := make([]byte, 2)
		if _, err := victim.ReadAt(buf, 0); err != nil {
			victim.Close()
			t.Fatalf("iteration %d: descriptor opened after writeStateFile was closed underneath: %v", i, err)
		}
		victim.Close()
	}
}
