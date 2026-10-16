package crv4

import (
	"os"
	"path/filepath"
	"testing"
)

// A fresh layout creates its state directory, publishes a seed, and reloads
// that same seed. The state directory is deliberately NOT pre-created, so the
// branch that orders a newly created directory and its parent is the branch
// this runs through.
//
// This is a smoke test, not a regression test for the routed flush. On the
// platforms that build this file the raw flush and the helper's flush are the
// same syscall, so reverting the helper leaves this green. Platforms without
// private seed custody refuse before any I/O, so there is nothing to assert
// there and the test skips.
func TestFreshSeedDirectoryCreateAndReload(t *testing.T) {
	if err := seedFilesSupported(); err != nil {
		t.Skipf("private seed custody is unavailable here: %v", err)
	}
	path := filepath.Join(t.TempDir(), "state", "hotkey.seed")
	seed, created, err := LoadOrCreateSeedFile(path)
	if err != nil {
		t.Fatalf("fresh create: created=%v err=%v", created, err)
	}
	if !created {
		t.Fatal("fresh create did not report created")
	}
	// The directory was absent on entry, so its creation here is what puts the
	// call through the create branch that orders it and its parent.
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("fresh create did not create the state directory: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("seed file missing: %v", err)
	}
	reload, reloadCreated, err := LoadOrCreateSeedFile(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloadCreated {
		t.Fatal("reload reported a create")
	}
	if reload != seed {
		t.Fatal("reload returned a different seed")
	}
}
