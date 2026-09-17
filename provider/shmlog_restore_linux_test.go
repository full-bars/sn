//go:build linux

package provider

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestRestoreFDRedirectsWrites: after restoreFD, writes to the target land in
// the saved file, the way stdout returns from the ramlog pipe to its original
// destination before an execve.
func TestRestoreFDRedirectsWrites(t *testing.T) {
	orig, err := os.Create(filepath.Join(t.TempDir(), "orig"))
	if err != nil {
		t.Fatal(err)
	}
	defer orig.Close()
	saved, err := dupCloseOnExec(int(orig.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(saved)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	target, err := unix.Dup(int(w.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(target)

	restoreFD(saved, target)
	if _, err := unix.Write(target, []byte("after restore")); err != nil {
		t.Fatalf("write after restore: %v", err)
	}
	got, err := os.ReadFile(orig.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after restore" {
		t.Fatalf("restored target wrote %q to the original file, want %q", got, "after restore")
	}
	flags, err := unix.FcntlInt(uintptr(target), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC != 0 {
		t.Fatal("restored target is close-on-exec; it would not survive execve")
	}
}

// TestDupCloseOnExecSetsFlag: the saved copies must not leak into children.
func TestDupCloseOnExecSetsFlag(t *testing.T) {
	fd, err := dupCloseOnExec(int(os.Stdin.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("saved descriptor is not close-on-exec")
	}
}

// TestRestoreFDIgnoresUnsaved: nothing to restore when the redirect never ran.
func TestRestoreFDIgnoresUnsaved(t *testing.T) {
	restoreFD(-1, 1)
}
