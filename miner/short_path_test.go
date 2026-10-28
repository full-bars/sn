package miner

import (
	"os"
	"testing"
)

// shortStateDir returns a temp dir with a SHORT name for tests that bind a
// unix socket under it.
//
// t.TempDir() embeds the full test name in the path, and the kernel caps
// sockaddr_un at 108 bytes. Under a long TMPDIR a descriptive test name alone
// overruns that, and the failure surfaces as a bare "bind: invalid argument"
// that reads like a logic or permission bug rather than a path-length one. A
// short prefix leaves room regardless of test name or TMPDIR.
func shortStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
