package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unixSocketPathLimit is the path length past which binding a unix socket is
// not worth attempting on Windows. The documented cap is 108 including the
// terminator; the margin keeps room for the trailing socket file name.
const unixSocketPathLimit = 100

// socketTempDir returns a temp directory that can hold a unix socket on
// Windows, where a socket path is capped at roughly 108 characters.
//
// t.TempDir() cannot be used for that. It builds the directory name from the
// full test name plus a counter, so a descriptive test name lands near the cap
// before the socket's own file name is appended. bind then fails with
// "bind: invalid argument", which reads like a bug in the test's logic rather
// than a path that is too long, and the Windows lifecycle job goes red with
// nothing pointing at the real cause.
//
// The length is checked here, once, instead of at every call site: every caller
// joins a socket file name onto this directory, so a directory that leaves no
// room for one is the failure worth reporting.
func socketTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sn")
	if err != nil {
		t.Fatalf("temp dir for the control socket: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("remove socket temp dir %s: %v", dir, err)
		}
	})
	// "provider.sock" is the longest name the callers use, plus its separator.
	if used := len(dir) + len("provider.sock") + 1; used > unixSocketPathLimit {
		t.Fatalf("temp dir %s leaves %d characters for the control socket, over the %d-character unix limit on Windows; use a shorter TMPDIR",
			dir, used, unixSocketPathLimit)
	}
	return dir
}

// The helper exists so a long test name cannot push the socket path past the
// Windows limit. Pin that directly: the probe must leave room for the socket
// file, and it must not carry the test name the way t.TempDir does.
func TestSocketTempDirLeavesRoomForTheSocket(t *testing.T) {
	dir := socketTempDir(t)
	full := filepath.Join(dir, "provider.sock")
	if len(full) > unixSocketPathLimit {
		t.Fatalf("socket path is %d characters, over the %d the helper promises: %s", len(full), unixSocketPathLimit, full)
	}
	if strings.Contains(filepath.Base(dir), "TestSocketTempDirLeavesRoomForTheSocket") {
		t.Fatalf("temp dir %q carries the test name, which is what made the Windows path too long", dir)
	}
	// t.TempDir is the thing being avoided; show the difference is real rather
	// than a renamed call. A long test name is the failing case.
	longDir := t.TempDir()
	longPath := filepath.Join(longDir, "provider.sock")
	t.Logf("socket path: helper %d characters, t.TempDir %d characters, limit %d", len(full), len(longPath), unixSocketPathLimit)
}
