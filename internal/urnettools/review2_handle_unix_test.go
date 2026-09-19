//go:build unix

package urnettools

// Tests for the review round after #658: handle-relative rename/removeAll,
// the pinned pending-overrides queue, and the direct toggle's directory
// ownership. Ownership across users needs root, so that test skips
// otherwise; the rest run as any user.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func mustHandle(t *testing.T, dir string) *stateDirHandle {
	t.Helper()
	h, err := openStateDirHandle(dir)
	if err != nil {
		t.Fatalf("openStateDirHandle(%s): %v", dir, err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

// rename replaces a symlink at the destination instead of writing through it.
func TestHandleRenameReplacesSymlinkAtDestination(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "victim")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "dst")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	h := mustHandle(t, dir)
	if err := h.writeOwned("src", []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.rename("src", "dst"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("symlink target was written through: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "dst")); string(b) != "new" {
		t.Fatalf("dst = %q, want new", b)
	}
	if err := h.rename("../x", "y"); err == nil {
		t.Fatal("rename accepted a path component with ..")
	}
}

// removeAll removes a tree, does not follow a symlink inside it, and treats a
// missing name as success.
func TestHandleRemoveAllDoesNotFollowSymlinks(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(dir, ".session-staging")
	if err := os.MkdirAll(filepath.Join(tree, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "sub", "f"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, "link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	h := mustHandle(t, dir)
	if err := h.removeAll(".session-staging"); err != nil {
		t.Fatalf("removeAll: %v", err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Fatalf("tree still present: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("removeAll followed a symlink out of the tree: %v", err)
	}
	if err := h.removeAll(".session-staging"); err != nil {
		t.Fatalf("removeAll of a missing name: %v", err)
	}
}

func readQueue(t *testing.T, dir string) []pendingOp {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "pending_overrides.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ops []pendingOp
	if err := json.Unmarshal(b, &ops); err != nil {
		t.Fatal(err)
	}
	return ops
}

// Two queued ops both survive, and a symlink planted at the temp name (which
// the old CreateTemp naming dodged by being random) neither wedges the queue
// nor is written through.
func TestQueuePendingOverrideSurvivesPlantedTempSymlink(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, pendingOverridesTmp)); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := queuePendingOverride(dir, "set", "a", "1"); err != nil {
		t.Fatalf("first queue: %v", err)
	}
	if err := queuePendingOverride(dir, "set", "b", "2"); err != nil {
		t.Fatalf("second queue: %v", err)
	}
	if ops := readQueue(t, dir); len(ops) != 2 || ops[0].Key != "a" || ops[1].Key != "b" {
		t.Fatalf("queue = %+v, want a then b", ops)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("temp-name symlink was written through: %q", b)
	}
	if _, err := os.Lstat(filepath.Join(dir, pendingOverridesTmp)); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

// A queue that is a symlink is refused, not read and then overwritten as if
// it were empty.
func TestQueuePendingOverrideRefusesSymlinkedQueue(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	real := filepath.Join(outside, "q.json")
	if err := os.WriteFile(real, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "pending_overrides.json")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := queuePendingOverride(dir, "set", "a", "1"); err == nil {
		t.Fatal("queuePendingOverride accepted a symlinked queue file")
	}
	if b, _ := os.ReadFile(real); string(b) != "[]" {
		t.Fatalf("symlink target modified: %q", b)
	}
}

// With the direct toggle dir missing, the toggle is still created and
// readable, at the caller's own ownership when that already matches.
func TestCmdDirectCreatesMissingToggleDir(t *testing.T) {
	stateDir := dirForStateDir(t)
	setDirectDiscovery(t, stateDir)
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("precondition: state dir should not exist yet: %v", err)
	}
	if err := cmdDirectToggle([]string{"off", "--state-dir", stateDir}, false, false); err != nil {
		t.Fatalf("cmdDirectToggle: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(stateDir, "direct")); err != nil || string(b) != "off\n" {
		t.Fatalf("toggle = %q, %v", b, err)
	}
}

// adoptOwnerOf re-owns a freshly created directory to the reference owner
// and files created under it follow. Changing owner needs root.
func TestAdoptOwnerOfHandsDirAndFilesToReferenceOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown across users")
	}
	base := t.TempDir()
	refDir, newDir := filepath.Join(base, "ref"), filepath.Join(base, "new")
	for _, d := range []string{refDir, newDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const uid, gid = 4242, 4243
	if err := os.Chown(refDir, uid, gid); err != nil {
		t.Fatal(err)
	}
	ref, h := mustHandle(t, refDir), mustHandle(t, newDir)
	if err := h.adoptOwnerOf(ref); err != nil {
		t.Fatalf("adoptOwnerOf: %v", err)
	}
	if err := h.writeOwned("direct", []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{newDir, filepath.Join(newDir, "direct")} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if st.Uid != uid || st.Gid != gid {
			t.Fatalf("%s owner = %d:%d, want %d:%d", p, st.Uid, st.Gid, uid, gid)
		}
	}
}
