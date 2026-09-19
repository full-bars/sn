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

// The gap this closes: a leaf-only handle accepts a path whose INTERMEDIATE
// component was swapped for a symlink; the walk from the trusted root refuses.
func TestOpenStateDirWithinRefusesSwappedIntermediateComponent(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "a")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	path := filepath.Join(home, "a", "state")

	// Documents the old behavior: the leaf is a real directory, so the
	// leaf-only handle follows the swapped ancestor.
	leaf, err := openStateDirHandle(path)
	if err != nil {
		t.Fatalf("leaf-only handle unexpectedly refused: %v", err)
	}
	leaf.Close()

	if h, err := openStateDirWithin(home, path, false); err == nil {
		h.Close()
		t.Fatal("openStateDirWithin followed a symlinked intermediate component")
	}
	if h, err := openStateDirIn(home, path); err == nil {
		h.Close()
		t.Fatal("openStateDirIn followed a symlinked intermediate component")
	}
	if h, err := openStateDirInCreate(home, path); err == nil {
		h.Close()
		t.Fatal("openStateDirInCreate followed a symlinked intermediate component")
	}
	if entries, _ := os.ReadDir(filepath.Join(outside, "state")); len(entries) != 0 {
		t.Fatalf("something was written through the symlink: %v", entries)
	}
}

// A real nested path works, including when the trust root itself is reached
// through a symlink (a home under a symlinked /home is normal).
func TestOpenStateDirWithinHappyPathAndSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "u", "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(t.TempDir(), "homelink")
	if err := os.Symlink(real, linkRoot); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	h, err := openStateDirWithin(linkRoot, filepath.Join(linkRoot, "u", "state"), false)
	if err != nil {
		t.Fatalf("openStateDirWithin: %v", err)
	}
	defer h.Close()
	if err := h.writeOwned("f", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(real, "u", "state", "f")); err != nil {
		t.Fatal(err)
	}
}

func TestOpenStateDirWithinRejectsPathNotBeneathRoot(t *testing.T) {
	home, other := t.TempDir(), t.TempDir()
	if h, err := openStateDirWithin(home, other, false); err == nil {
		h.Close()
		t.Fatal("accepted a path outside the root")
	}
	if h, err := openStateDirWithin(home, home, false); err == nil {
		h.Close()
		t.Fatal("accepted the root itself as its own state dir")
	}
	// Outside the trusted root, openStateDirIn falls back to a leaf pin.
	h, err := openStateDirIn(home, other)
	if err != nil {
		t.Fatalf("fallback leaf open: %v", err)
	}
	h.Close()
}

// Create mode makes the missing components 0700 under the root.
func TestOpenStateDirWithinCreatesMissingComponents(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "x", "y", ".urnetwork")
	h, err := openStateDirInCreate(home, path)
	if err != nil {
		t.Fatalf("openStateDirInCreate: %v", err)
	}
	defer h.Close()
	for _, d := range []string{"x", "x/y", "x/y/.urnetwork"} {
		fi, err := os.Stat(filepath.Join(home, d))
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v mode=%v", d, err, fi)
		}
	}
}

// A provider with a trusted home whose state path has a swapped intermediate
// component is refused end to end, and nothing lands in the link target.
func TestCmdDirectRefusesSwappedIntermediateUnderTrustedHome(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	home := filepath.Join(base, "home", "unt")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "x")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	stateDir := filepath.Join(link, ".urnetwork")
	orig := discoverProcessesFn
	origStopped := discoverStoppedFn
	discoverProcessesFn = func() []Provider {
		return []Provider{{StateDir: stateDir, StateHome: home, Unit: "urnetwork.service", Running: true}}
	}
	discoverStoppedFn = func([]Provider) []Provider { return nil }
	t.Cleanup(func() { discoverProcessesFn, discoverStoppedFn = orig, origStopped })

	if err := cmdDirectToggle([]string{"on", "--state-dir", stateDir}, false, false); err == nil {
		t.Fatal("cmdDirectToggle wrote through a swapped intermediate component")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("link target was modified: %v", entries)
	}
}

// Components created under a root-run walk belong to the root dir's owner.
func TestOpenStateDirWithinCreateOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown across users")
	}
	home := t.TempDir()
	const uid, gid = 4242, 4243
	if err := os.Chown(home, uid, gid); err != nil {
		t.Fatal(err)
	}
	h, err := openStateDirInCreate(home, filepath.Join(home, "a", ".urnetwork"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, d := range []string{"a", "a/.urnetwork"} {
		fi, _ := os.Lstat(filepath.Join(home, d))
		st := fi.Sys().(*syscall.Stat_t)
		if st.Uid != uid || st.Gid != gid {
			t.Fatalf("%s owner = %d:%d, want %d:%d", d, st.Uid, st.Gid, uid, gid)
		}
	}
}
