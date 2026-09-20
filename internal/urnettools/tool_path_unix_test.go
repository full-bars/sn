//go:build unix

package urnettools

import (
	"os"
	"path/filepath"
	"testing"
)

func mkInstall(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	for _, n := range []string{"urnet-tools", "urnetwork"} {
		if err := os.WriteFile(filepath.Join(src, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

func TestLinkToolsIntoDirCreatesAndIsIdempotent(t *testing.T) {
	src, dir := mkInstall(t), filepath.Join(t.TempDir(), "bin")
	changed, err := linkToolsIntoDir(dir, src)
	if err != nil || len(changed) != 2 {
		t.Fatalf("first run: changed=%v err=%v", changed, err)
	}
	for _, n := range []string{"urnet-tools", "urnetwork"} {
		if got, _ := os.Readlink(filepath.Join(dir, n)); got != filepath.Join(src, n) {
			t.Fatalf("%s -> %q", n, got)
		}
	}
	if changed, err := linkToolsIntoDir(dir, src); err != nil || len(changed) != 0 {
		t.Fatalf("second run must be a quiet no-op: changed=%v err=%v", changed, err)
	}
}

func TestLinkToolsIntoDirNeverClobbersAndRepointsStale(t *testing.T) {
	src, dir := mkInstall(t), t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "urnet-tools"), []byte("mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/urnetwork", filepath.Join(dir, "urnetwork")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	changed, err := linkToolsIntoDir(dir, src)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "urnet-tools")); string(b) != "mine" {
		t.Fatalf("a real file was overwritten: %q", b)
	}
	if got, _ := os.Readlink(filepath.Join(dir, "urnetwork")); got != filepath.Join(src, "urnetwork") {
		t.Fatalf("stale link not repointed: %q", got)
	}
	if len(changed) != 1 || changed[0] != "urnetwork" {
		t.Fatalf("changed = %v, want only urnetwork", changed)
	}
}

func TestLinkToolsIntoDirSkipsTheInstallDirItself(t *testing.T) {
	src := mkInstall(t)
	if changed, err := linkToolsIntoDir(src, src); err != nil || len(changed) != 0 {
		t.Fatalf("linking a dir into itself must do nothing: %v %v", changed, err)
	}
}

// Under `go test` the executable is not called urnet-tools, so the self-heal
// must not touch the developer's real ~/.local/bin or /usr/local/bin.
func TestEnsureToolOnPathIgnoresNonToolBinaries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ensureToolOnPath()
	if _, err := os.Stat(filepath.Join(home, ".local")); err == nil {
		t.Fatal("ensureToolOnPath created files for a non-urnet-tools executable")
	}
}
