//go:build unix

package urnettools

// Regression tests for the inline review of the container-discovery-ghost PR:
// descriptor-pinned session staging, no-follow/non-blocking jwt read, bounded
// lock wait, the session-save output path, and the converted self-heal /
// hotswap-counter writers (symlink/FIFO/state-dir-swap refusals).
// Unix-only (symlinks, FIFOs, hardlinks, flock).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// runWithin fails the test if fn does not return within d. The bugs these
// tests guard against are hangs, so a bare call would hang the suite.
func runWithin(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s (hung)", what, d)
	}
}

// A symlink at jwt pointing at an endless source must not be followed or read
// to completion (unbounded allocation in a root-run tool).
func TestStageSessionFilesJWTSymlinkNotFollowed(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("/dev/zero", filepath.Join(dir, "jwt")); err != nil {
		t.Fatal(err)
	}
	var err error
	runWithin(t, 10*time.Second, "stageSessionFiles with jwt->/dev/zero", func() {
		_, err = stageSessionFiles(Provider{StateDir: dir}, map[string][]byte{"jwt": []byte(jwtFor("net-a"))}, false)
	})
	if err == nil {
		t.Fatal("stageSessionFiles accepted a symlinked jwt; must refuse")
	}
	if _, serr := os.Stat(filepath.Join(dir, ".session-pending")); serr == nil {
		t.Fatal(".session-pending written despite the refusal")
	}
}

// A FIFO planted as jwt must not hang the open.
func TestStageSessionFilesJWTFifoDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, "jwt"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	var err error
	runWithin(t, 10*time.Second, "stageSessionFiles with jwt as FIFO", func() {
		_, err = stageSessionFiles(Provider{StateDir: dir}, map[string][]byte{"jwt": []byte(jwtFor("net-a"))}, false)
	})
	if err == nil {
		t.Fatal("stageSessionFiles accepted a FIFO jwt; must refuse")
	}
}

// The no-follow read helper must reject a FIFO instead of blocking in open.
func TestOpenStateFileNoFollowFifoDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, "jwt"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	var err error
	runWithin(t, 5*time.Second, "openStateFileNoFollow on a FIFO", func() {
		var f *os.File
		f, err = openStateFileNoFollow(dir, "jwt")
		if f != nil {
			f.Close()
		}
	})
	if err == nil {
		t.Fatal("openStateFileNoFollow accepted a FIFO")
	}
}

// A state dir that is itself a symlink is refused rather than followed.
func TestStageSessionFilesRefusesSymlinkedStateDir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := stageSessionFiles(Provider{StateDir: link}, map[string][]byte{"jwt": []byte(jwtFor("net-a"))}, false); err == nil {
		t.Fatal("stageSessionFiles followed a symlinked state dir")
	}
	if _, err := os.Stat(filepath.Join(real, ".session-pending")); err == nil {
		t.Fatal("files were staged through the symlink")
	}
}

// The core of the F1 fix: once the handle is open, replacing the state dir
// path with a symlink to another directory must not redirect later writes.
func TestStateDirHandleWritesSurviveDirectorySwap(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	victim := filepath.Join(base, "victim")
	for _, d := range []string{state, victim} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	h, err := openStateDirHandle(state)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	// Attacker (the directory owner) swaps the path for a symlink.
	moved := filepath.Join(base, "moved")
	if err := os.Rename(state, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, state); err != nil {
		t.Fatal(err)
	}

	if err := h.writeOwned("jwt", []byte("secret"), 0o600); err != nil {
		t.Fatalf("writeOwned: %v", err)
	}
	sub, err := h.mkdirOwned(".session-staging")
	if err != nil {
		t.Fatalf("mkdirOwned: %v", err)
	}
	sub.Close()

	if _, err := os.Stat(filepath.Join(moved, "jwt")); err != nil {
		t.Fatalf("write did not land in the pinned directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, ".session-staging")); err != nil {
		t.Fatalf("mkdir did not land in the pinned directory: %v", err)
	}
	entries, _ := os.ReadDir(victim)
	if len(entries) != 0 {
		t.Fatalf("swapped-in symlink target was written to: %v", entries)
	}
}

func TestStateDirHandleWriteRefusals(t *testing.T) {
	dir := t.TempDir()
	h, err := openStateDirHandle(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	// symlink at the final component
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := h.writeOwned("linked", []byte("x"), 0o600); err == nil {
		t.Fatal("wrote through a symlink")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("symlink target modified: %q", b)
	}

	// hardlink to a file outside the state dir
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(dir, "hard")); err != nil {
		t.Skipf("hardlink not permitted here: %v", err)
	}
	if err := h.writeOwned("hard", []byte("x"), 0o600); err == nil {
		t.Fatal("wrote to a multiply-linked file")
	} else if !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("unexpected error: %v", err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Fatalf("hardlinked file modified: %q", b)
	}

	// path components that could leave the directory
	for _, bad := range []string{"", ".", "..", "a/b", "../x"} {
		if err := h.writeOwned(bad, nil, 0o600); err == nil {
			t.Fatalf("writeOwned accepted name %q", bad)
		}
		if _, err := h.readFile(bad, 10); err == nil {
			t.Fatalf("readFile accepted name %q", bad)
		}
	}

	// existing subdirectory is an error, not silently reused
	if _, err := h.mkdirOwned("dup"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.mkdirOwned("dup"); err == nil {
		t.Fatal("mkdirOwned reused an existing directory")
	}
}

func TestStateDirHandleReadFileBounded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := openStateDirHandle(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.readFile("big", 99); err == nil {
		t.Fatal("readFile ignored its size bound")
	}
	if b, err := h.readFile("big", 100); err != nil || len(b) != 100 {
		t.Fatalf("readFile at the bound: %d bytes, err=%v", len(b), err)
	}
}

// A lock held by someone else must not block a queue update forever.
func TestAcquireExclusiveLockOwnedTimesOut(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "q.lock")
	release, err := acquireExclusiveLockOwned(lock, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	var got error
	start := time.Now()
	runWithin(t, 5*time.Second, "second lock attempt", func() {
		_, got = acquireExclusiveLockOwned(lock, "", 200*time.Millisecond)
	})
	if got == nil {
		t.Fatal("second acquire succeeded while the lock was held")
	}
	if el := time.Since(start); el < 150*time.Millisecond {
		t.Fatalf("gave up after %s, before the timeout", el)
	}
	release()
	rel2, err := acquireExclusiveLockOwned(lock, "", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("lock not reacquirable after release: %v", err)
	}
	rel2()
}

// fchown acts on the inode: a lock path hardlinked to another file must be
// refused before any ownership change.
func TestAcquireExclusiveLockOwnedRefusesHardlink(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(t.TempDir(), "other")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "q.lock")
	if err := os.Link(other, lock); err != nil {
		t.Skipf("hardlink not permitted here: %v", err)
	}
	if rel, err := acquireExclusiveLockOwned(lock, dir, time.Second); err == nil {
		rel()
		t.Fatal("accepted a hardlinked lock file")
	}
}

func TestWriteSessionBundleRefusesSymlinkAndFifo(t *testing.T) {
	dir := t.TempDir()

	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "out.enc")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionBundle(link, []byte("bundle")); err == nil {
		t.Fatal("writeSessionBundle followed a symlink")
	}
	if b, _ := os.ReadFile(victim); string(b) != "precious" {
		t.Fatalf("symlink target was modified: %q", b)
	}
	if fi, _ := os.Stat(victim); fi.Mode().Perm() != 0o644 {
		t.Fatalf("symlink target mode changed to %v", fi.Mode().Perm())
	}

	fifo := filepath.Join(dir, "out.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	var err error
	runWithin(t, 5*time.Second, "writeSessionBundle to a FIFO", func() {
		err = writeSessionBundle(fifo, []byte("bundle"))
	})
	if err == nil {
		t.Fatal("writeSessionBundle wrote to a FIFO")
	}
}

func TestWriteSessionBundleOverwritesRegularFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.enc")
	if err := os.WriteFile(out, []byte("old-and-longer-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionBundle(out, []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if string(b) != "new" {
		t.Fatalf("content = %q, want truncated overwrite", b)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// --- D4: converted writers (self-heal marker, hotswap counters) ---

// writeSelfHeal resolves through discovery; the mocked provider hands the
// tool a state dir. A state dir that IS a symlink must be refused outright:
// the old pathname writer opened THROUGH it (O_NOFOLLOW only guards the
// final component) and a root write landed in the link target.
func TestWriteSelfHealRefusesSymlinkedStateDir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	origDiscover := discoverSystemdFn
	origDocker := discoverDockerFn
	discoverSystemdFn = func() []Provider {
		return []Provider{{User: "test-user", StateDir: link, Unit: "urnetwork.service", Running: true}}
	}
	discoverDockerFn = func() []Provider { return nil }
	defer func() {
		discoverSystemdFn = origDiscover
		discoverDockerFn = origDocker
	}()
	if err := cmdSelfHeal([]string{"on", "--state-dir", link}); err == nil {
		t.Fatal("self-heal wrote through a symlinked state dir; must refuse")
	}
	if _, err := os.Stat(filepath.Join(real, "proxy_self_heal")); err == nil {
		t.Fatalf("marker landed in the symlink target; the swap was followed")
	}
}

// A symlink planted at the marker path itself must not redirect the write.
func TestWriteSelfHealRefusesSymlinkMarker(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "proxy_self_heal")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	origDiscover := discoverSystemdFn
	origDocker := discoverDockerFn
	discoverSystemdFn = func() []Provider {
		return []Provider{{User: "test-user", StateDir: dir, Unit: "urnetwork.service", Running: true}}
	}
	discoverDockerFn = func() []Provider { return nil }
	defer func() {
		discoverSystemdFn = origDiscover
		discoverDockerFn = origDocker
	}()
	if err := cmdSelfHeal([]string{"on", "--state-dir", dir}); err == nil {
		t.Fatal("self-heal followed a symlink marker; must refuse")
	}
	if b, _ := os.ReadFile(victim); string(b) != "precious" {
		t.Fatalf("symlink target modified: %q", b)
	}
}

// A FIFO planted as the marker must never hang the write.
func TestWriteSelfHealFifoMarkerNoHang(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, "proxy_self_heal"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	origDiscover := discoverSystemdFn
	origDocker := discoverDockerFn
	discoverSystemdFn = func() []Provider {
		return []Provider{{User: "test-user", StateDir: dir, Unit: "urnetwork.service", Running: true}}
	}
	discoverDockerFn = func() []Provider { return nil }
	defer func() {
		discoverSystemdFn = origDiscover
		discoverDockerFn = origDocker
	}()
	var err error
	runWithin(t, 5*time.Second, "self-heal write to a FIFO marker", func() {
		err = cmdSelfHeal([]string{"on", "--state-dir", dir})
	})
	if err == nil {
		t.Fatal("self-heal wrote into a FIFO marker; must refuse")
	}
}

// recordHotswapDecline is best-effort: a symlinked state dir must be a silent
// no-op, never a root write through the link into the target directory.
func TestRecordHotswapDeclineRefusesSymlinkedStateDir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	recordHotswapDecline(link, "version_old")
	if _, err := os.Stat(filepath.Join(real, hotswapCountsFile)); err == nil {
		t.Fatalf("hotswap counters landed in the symlink target; the swap was followed")
	}
}

// A symlink planted at the counters temp path must not redirect the write.
func TestRecordHotswapDeclineRefusesSymlinkCountsFile(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, hotswapCountsFile+".tmp")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	recordHotswapDecline(dir, "version_old")
	if b, _ := os.ReadFile(victim); string(b) != "untouched" {
		t.Fatalf("symlink target modified: %q", b)
	}
}

// A FIFO planted as the counters temp path must never hang the write.
func TestRecordHotswapDeclineFifoNoHang(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, hotswapCountsFile+".tmp"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	var err error
	runWithin(t, 5*time.Second, "hotswap counter write to a FIFO", func() {
		recordHotswapDecline(dir, "version_old")
	})
	_ = err
	if _, serr := os.Stat(filepath.Join(dir, hotswapCountsFile)); serr == nil {
		t.Fatalf("counters file appeared despite the FIFO at the temp path")
	}
}
