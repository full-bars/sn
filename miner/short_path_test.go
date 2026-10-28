package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// maxShortTempBase is the longest base directory a test temp dir may sit
	// under and still leave room for a unix socket inside it.
	//
	// The kernel caps sockaddr_un at 108 bytes (104 on darwin), and the whole
	// path counts, not just the name. A state dir needs room for
	// "/snNNNNNNNN" plus "/operators/operator.example/provider.sock" (~50
	// bytes) on top of the base, so a base longer than this cannot host a
	// socket no matter how short the directory name is.
	maxShortTempBase = 40

	// fallbackShortTempBase is used when TMPDIR is too long for the budget
	// above. /tmp is the one short absolute path a unix can be relied on to
	// have, and Cleanup removes what is created there.
	fallbackShortTempBase = "/tmp"
)

// shortTempBase picks a base directory short enough to bind a socket under.
//
// os.MkdirTemp("", ...) roots the directory at os.TempDir(), so a short
// directory NAME does not help when TMPDIR itself is long: a 74-character
// TMPDIR still produced a 126-byte socket path and failed with a bare
// "bind: invalid argument". The base has to be short, not just the name.
func shortTempBase() string {
	if base := os.TempDir(); len(base) <= maxShortTempBase {
		return base
	}
	return fallbackShortTempBase
}

// shortStateDir returns a temp dir with a SHORT absolute path for tests that
// bind a unix socket under it.
//
// t.TempDir() embeds the full test name in the path, which overruns the
// sockaddr_un limit and surfaces as "bind: invalid argument" that reads like a
// logic or permission bug rather than a path-length one. This keeps both the
// name and, when needed, the base short, so the socket path fits whatever
// TMPDIR the runner has.
func shortStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(shortTempBase(), "sn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// The whole path counts against the sockaddr_un limit, so a long TMPDIR must
// not reach the socket even with a two-character directory name. This is the
// case the first version of this helper missed: it shortened the name only,
// and a 74-character TMPDIR still failed with "bind: invalid argument".
func TestShortStateDirFitsUnderAnOversizedTMPDIR(t *testing.T) {
	t.Setenv("TMPDIR", "/tmp/"+strings.Repeat("a", 70))
	dir := shortStateDir(t)
	sock := filepath.Join(dir, "operators", "operator.example", "provider.sock")
	if n := len(sock); n >= 104 {
		t.Fatalf("socket path is %d bytes, over the 104-byte darwin sockaddr_un limit: %s", n, sock)
	}
}

// A TMPDIR that does fit must still be honoured: the fallback is for an
// oversized base, not a reason to ignore the runner's temp dir.
func TestShortStateDirHonoursATMPDIRThatFits(t *testing.T) {
	short := t.TempDir()
	if err := os.MkdirAll(short, 0o700); err != nil {
		t.Fatal(err)
	}
	if len(short) > maxShortTempBase {
		t.Skipf("runner temp dir %q is already too long for this case", short)
	}
	t.Setenv("TMPDIR", short)
	if dir := shortStateDir(t); !strings.HasPrefix(dir, short) {
		t.Fatalf("shortStateDir = %q, want it under the caller's TMPDIR %q when that base fits", dir, short)
	}
}
