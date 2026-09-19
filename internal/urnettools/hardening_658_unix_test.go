//go:build unix

package urnettools

// Regression tests for issue #658: the last three pathname-based state
// writes (cmd_direct toggle, unit-file backup, unit-file replace) must pin
// their directory with the descriptor handle like every other state write,
// refusing a symlinked or swapped directory instead of writing through it.

import (
	"os"
	"path/filepath"
	"testing"
)

// setDirectDiscovery mocks discovery so cmdDirectToggle resolves the target
// provider through --state-dir without needing a live provider or systemd.
// The User is left empty so cmdDirectToggle's home fallback derives the
// toggle path from the state dir itself, like a provider without a passwd
// entry: stateDir must therefore be <home>/.urnetwork-shaped.
func setDirectDiscovery(t *testing.T, stateDir string) {
	t.Helper()
	origProc := discoverProcessesFn
	origStopped := discoverStoppedFn
	discoverProcessesFn = func() []Provider {
		return []Provider{{User: "", StateDir: stateDir, Unit: "urnetwork.service", Running: true}}
	}
	discoverStoppedFn = func([]Provider) []Provider { return nil }
	t.Cleanup(func() {
		discoverProcessesFn = origProc
		discoverStoppedFn = origStopped
	})
}

// dirForStateDir builds a temp home-shaped state dir <base>/home/unt/.urnetwork.
func dirForStateDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "home", "unt", ".urnetwork")
}

// A state dir that IS a symlink must be refused outright: the old code
// opened through it (O_NOFOLLOW only guards the final component) and the
// toggle landed in the link target.
func TestCmdDirectRefusesSymlinkedStateDir(t *testing.T) {
	real := t.TempDir()
	stateDir := dirForStateDir(t)
	if err := os.MkdirAll(filepath.Dir(stateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, stateDir); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	setDirectDiscovery(t, stateDir)
	if err := cmdDirectToggle([]string{"on", "--state-dir", stateDir}, false, false); err == nil {
		t.Fatal("cmdDirectToggle wrote through a symlinked state dir; must refuse")
	}
	if _, err := os.Stat(filepath.Join(real, "direct")); err == nil {
		t.Fatalf("toggle landed in the symlink target; the swap was followed")
	}
}

// A symlink planted at the toggle file itself must not redirect the write.
func TestCmdDirectRefusesSymlinkToggle(t *testing.T) {
	stateDir := dirForStateDir(t)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(filepath.Dir(stateDir), "victim")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(stateDir, "direct")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	setDirectDiscovery(t, stateDir)
	if err := cmdDirectToggle([]string{"on", "--state-dir", stateDir}, false, false); err == nil {
		t.Fatal("cmdDirectToggle followed a symlink toggle; must refuse")
	}
	if b, _ := os.ReadFile(victim); string(b) != "precious" {
		t.Fatalf("symlink target modified: %q", b)
	}
}

// The happy path still lands the toggle and 0600 mode.
func TestCmdDirectWritesToggle(t *testing.T) {
	stateDir := dirForStateDir(t)
	setDirectDiscovery(t, stateDir)
	if err := cmdDirectToggle([]string{"on", "--state-dir", stateDir}, false, false); err != nil {
		t.Fatalf("cmdDirectToggle: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(stateDir, "direct"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "on\n" {
		t.Fatalf("toggle = %q, want on", b)
	}
	if fi, _ := os.Stat(filepath.Join(stateDir, "direct")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("toggle mode = %v, want 0600", fi.Mode().Perm())
	}
}

// A unit-file directory that is a symlink must be refused when backing up.
func TestWriteUnitBackupRefusesSymlinkedDir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "units")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	unitPath := filepath.Join(link, "urnetwork.service")
	if err := writeUnitBackup(unitPath, []byte("content")); err == nil {
		t.Fatal("writeUnitBackup wrote through a symlinked unit dir; must refuse")
	}
	if _, err := os.Stat(filepath.Join(real, "urnetwork.service.bak")); err == nil {
		t.Fatalf("backup landed in the symlink target; the swap was followed")
	}
}

// A symlink planted at the backup path must not redirect the write.
func TestWriteUnitBackupRefusesSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	unitPath := filepath.Join(dir, "urnetwork.service")
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(unitPath, []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, unitPath+".bak"); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := writeUnitBackup(unitPath, []byte("content")); err == nil {
		t.Fatal("writeUnitBackup followed a symlink backup path; must refuse")
	}
	if b, _ := os.ReadFile(victim); string(b) != "precious" {
		t.Fatalf("symlink target modified: %q", b)
	}
}

// The happy path writes the .bak next to the unit with the unit's content.
func TestWriteUnitBackupHappyPath(t *testing.T) {
	dir := t.TempDir()
	unitPath := filepath.Join(dir, "urnetwork.service")
	if err := os.WriteFile(unitPath, []byte("Type=simple"), 0o644); err != nil {
		t.Fatal(err)
	}
	content := []byte("Type=notify\nNotifyAccess=all\n")
	if err := writeUnitBackup(unitPath, content); err != nil {
		t.Fatalf("writeUnitBackup: %v", err)
	}
	b, err := os.ReadFile(unitPath + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(content) {
		t.Fatalf("backup = %q, want %q", b, content)
	}
}

// replaceUnitFile must refuse a unit directory that is a symlink.
func TestReplaceUnitFileRefusesSymlinkedDir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "units")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	unitPath := filepath.Join(link, "urnetwork.service")
	if err := os.WriteFile(unitPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := replaceUnitFile(unitPath, []byte("new")); err == nil {
		t.Fatal("replaceUnitFile wrote through a symlinked unit dir; must refuse")
	}
	if b, _ := os.ReadFile(filepath.Join(real, "urnetwork.service")); string(b) != "old" {
		t.Fatalf("unit in symlink target changed to %q", b)
	}
}

// replaceUnitFile must refuse a symlink planted at the temp path.
func TestReplaceUnitFileRefusesSymlinkTmp(t *testing.T) {
	dir := t.TempDir()
	unitPath := filepath.Join(dir, "urnetwork.service")
	if err := os.WriteFile(unitPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, unitPath+".tmp"); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := replaceUnitFile(unitPath, []byte("new")); err == nil {
		t.Fatal("replaceUnitFile wrote through a symlink temp path; must refuse")
	}
	if b, _ := os.ReadFile(victim); string(b) != "precious" {
		t.Fatalf("symlink target modified: %q", b)
	}
	if b, _ := os.ReadFile(unitPath); string(b) != "old" {
		t.Fatalf("unit changed to %q", b)
	}
}

// The happy path replaces the unit atomically (tmp renamed over).
func TestReplaceUnitFileHappyPath(t *testing.T) {
	dir := t.TempDir()
	unitPath := filepath.Join(dir, "urnetwork.service")
	if err := os.WriteFile(unitPath, []byte("old-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := replaceUnitFile(unitPath, []byte("new-content")); err != nil {
		t.Fatalf("replaceUnitFile: %v", err)
	}
	b, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new-content" {
		t.Fatalf("unit = %q, want new-content", b)
	}
	// The temp file must not be left behind.
	if _, err := os.Stat(unitPath + ".tmp"); err == nil {
		t.Fatalf("temp file left behind after replace")
	}
	// Anyone who can replace the unit file can also hand the directory
	// owner the new content; mode stays 0644 like the original write.
	if fi, _ := os.Stat(unitPath); fi.Mode().Perm() != 0o644 {
		t.Fatalf("unit mode = %v, want 0644", fi.Mode().Perm())
	}
}
