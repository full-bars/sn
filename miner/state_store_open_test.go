package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.etcd.io/bbolt"
)

// A panic after bbolt.Open has taken the file lock must not leave the handle
// open. bbolt's flock is held by the open file description, so an orphaned
// handle keeps every later open of the same path waiting on the lock.
func TestProviderStoreOpenPanicReleasesTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), providerStoreFileName)

	providerStoreOpenedHook = func(*bbolt.DB) { panic("injected after open") }
	t.Cleanup(func() { providerStoreOpenedHook = nil })

	if _, err := openProviderStore(path); err == nil {
		t.Fatal("open with a panic after the lock was taken = nil error, want the panic reported")
	}

	providerStoreOpenedHook = nil
	store, err := openProviderStore(path)
	if err != nil {
		t.Fatalf("second open after the panic = %v, want the lock released: the orphaned handle still holds it", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close the second open: %s", err)
	}
}

// The restore path runs after a handoff released the file to a candidate. A
// damaged file found there must be reported and left in place, not moved aside
// and replaced with an empty store, because that resets the extender activation
// budget and an operator could trigger it by aborting a handoff.
func TestRestoreAfterAbortedHandoffDoesNotQuarantineACorruptStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	closeProviderStore()
	t.Cleanup(closeProviderStore)

	path, err := providerStatePath(providerStoreFileName)
	if err != nil {
		t.Fatalf("store path: %s", err)
	}
	damaged := []byte("this is not a bbolt database")
	if err := os.WriteFile(path, damaged, 0o600); err != nil {
		t.Fatalf("write the damaged file: %s", err)
	}

	releaseProviderStore()
	if err := restoreProviderStoreAfterAbortedHandoff(); err == nil {
		t.Fatal("restore over a damaged file = nil, want the failure reported")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the file after the restore: %s", err)
	}
	if string(got) != string(damaged) {
		t.Fatal("the restore path rewrote the damaged file instead of leaving it for a normal open")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the state dir: %s", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".corrupt.") {
			t.Fatalf("the restore path quarantined the file as %s", entry.Name())
		}
	}

	// The next normal open still quarantines, so the damage is not kept forever.
	closeProviderStore()
	store, err := openProviderStoreForProcess()
	if err != nil || store == nil {
		t.Fatalf("normal open after the refused restore = (%v, %v), want the quarantine to succeed", store, err)
	}
}
