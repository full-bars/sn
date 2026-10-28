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

// bbolt can panic inside Open, before it returns a handle, when a page it
// reads is damaged. The panic leaves the file it locked open, so every later
// open of the path would wait out the lock timeout instead of failing at once.
// A file written without a synced freelist makes Open rebuild the freelist and
// walk the pages, which is where the damage is met.
func TestProviderStoreOpenPanicInsideBboltReleasesTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), providerStoreFileName)
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{NoFreelistSync: true})
	if err != nil {
		t.Fatalf("create the file: %s", err)
	}
	for i := 0; i < 30; i++ {
		if err := db.Update(func(tx *bbolt.Tx) error {
			bucket, err := tx.CreateBucketIfNotExists([]byte("a"))
			if err != nil {
				return err
			}
			for j := 0; j < 200; j++ {
				if err := bucket.Put([]byte{byte(i), byte(j)}, make([]byte, 100)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("fill the file: %s", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the file: %s", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the file: %s", err)
	}
	for off := 2 * 4096; off+24 < len(raw); off += 4096 {
		for k := 8; k < 24; k++ {
			raw[off+k] = 0xff
		}
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("damage the file: %s", err)
	}

	_, err = openProviderStore(path)
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("open of a damaged file = %v, want the panic reported", err)
	}
	_, err = openProviderStore(path)
	if err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("second open = %v, want the damage again, not a lock timeout: the first open's file is still locked", err)
	}
}
