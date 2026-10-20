package urnettools

// Tests for the CodeRabbit findings addressed on this branch. Each test pins
// the behaviour the finding named, so the fix cannot be undone silently: the
// regressions here were all cases where the tool reported success while the
// provider state did not change, or where a guard checked one layer away from
// the thing it needed to check.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- CR13: a dangling symlink must not prove containment --------------------

// TestStateDirInsideHomeRejectsDanglingSymlink pins the containment check. A
// component that exists but does not resolve used to be walked past, so an
// unresolved path could be accepted as inside ownerHome; a target appearing
// outside the home later would then be followed when the state directory is
// read.
func TestStateDirInsideHomeRejectsDanglingSymlink(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()

	// A real directory inside home: must still be accepted, or the check is
	// useless.
	if !stateDirInsideHome(home, filepath.Join(home, ".urnetwork")) {
		t.Error("a plain directory inside home must be accepted")
	}

	// home/.urnetwork -> a path that does not exist yet, under a directory
	// that DOES exist outside home. The unresolved link must be refused.
	link := filepath.Join(home, ".urnetwork")
	target := filepath.Join(outside, "not-created-yet")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if stateDirInsideHome(home, link) {
		t.Error("a dangling symlink inside home must not prove containment")
	}

	// And once the target exists outside home, it must still be refused.
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if stateDirInsideHome(home, link) {
		t.Error("a symlink resolving outside home must not prove containment")
	}
}

// --- CR6: duplicate sysctl assignments must all be handled ------------------

// TestUpsertBSDConfLinesRemovesDuplicateAssignments pins that a key assigned
// twice is rewritten once and removed thereafter. /etc/rc.d/sysctl applies
// lines in file order, so the stale second line used to win at boot while the
// command still printed "persisted".
func TestUpsertBSDConfLinesRemovesDuplicateAssignments(t *testing.T) {
	conf := strings.Join([]string{
		"# site defaults",
		"net.inet.tcp.recvspace=8192",
		"kern.maxfiles=1000",
		"net.inet.tcp.recvspace=65536",
		"",
	}, "\n")

	out, err := upsertBSDConfLines(conf, [][]string{{"net.inet.tcp.recvspace", "4194304"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if n := strings.Count(out, "net.inet.tcp.recvspace="); n != 1 {
		t.Errorf("key assigned %d times, want exactly 1:\n%s", n, out)
	}
	if !strings.Contains(out, "net.inet.tcp.recvspace=4194304") {
		t.Errorf("first assignment was not rewritten to the tuned value:\n%s", out)
	}
	if strings.Contains(out, "65536") || strings.Contains(out, "8192") {
		t.Errorf("a stale duplicate value survived:\n%s", out)
	}
	// Unmanaged content and the trailing newline must survive.
	if !strings.Contains(out, "kern.maxfiles=1000") {
		t.Errorf("unmanaged entry lost:\n%s", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("trailing newline lost:\n%q", out)
	}
}
