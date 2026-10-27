package miner

import (
	"errors"
	"path/filepath"
	"testing"
)

// A database file created by this open is a new directory entry, and bbolt
// syncs only the pages it writes. Without the directory sync a crash just
// after creation can leave the file unlinked or zero length, so the open has
// to sync the parent once, for the creation and not for every reopen.
func TestProviderStoreSyncsTheParentDirectoryOnCreation(t *testing.T) {
	original := providerStoreSyncDir
	t.Cleanup(func() { providerStoreSyncDir = original })
	var synced []string
	providerStoreSyncDir = func(dir string) error {
		synced = append(synced, dir)
		return nil
	}

	dir := t.TempDir()
	path := filepath.Join(dir, providerStoreFileName)
	store, err := openProviderStore(path)
	if err != nil {
		t.Fatalf("open a new database: %s", err)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Fatalf("directory syncs after creating %s = %v, want [%s]", path, synced, dir)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %s", err)
	}

	// The entry already exists, so a reopen has nothing to make durable and
	// must not pay for a directory sync.
	reopened, err := openProviderStore(path)
	if err != nil {
		t.Fatalf("reopen: %s", err)
	}
	defer reopened.Close()
	if len(synced) != 1 {
		t.Fatalf("directory syncs after reopening an existing %s = %v, want one", path, synced)
	}
}

// The sync is a durability guarantee, not an availability one: a directory
// that will not sync must not take away a database the process can already
// read and write, or a read-only directory would stop the provider.
func TestProviderStoreKeepsAStoreWhoseDirectoryWillNotSync(t *testing.T) {
	original := providerStoreSyncDir
	t.Cleanup(func() { providerStoreSyncDir = original })
	providerStoreSyncDir = func(string) error { return errors.New("sync failed") }

	path := filepath.Join(t.TempDir(), providerStoreFileName)
	store, err := openProviderStore(path)
	if err != nil {
		t.Fatalf("open with a failing directory sync: %s", err)
	}
	defer store.Close()
	if err := store.save(providerStoreBucketState, "probe", map[string]int{"count": 1}); err != nil {
		t.Fatalf("save after a failed directory sync: %s", err)
	}
}

// appendBoundedMany records outcomes and markers, so a dropped append must
// report the loss instead of returning nil as if it had written.
func TestProviderStoreAppendBoundedManyReportsAReleasedHandle(t *testing.T) {
	testExtenderState(t)
	store := providerStoreInstance()
	if store == nil {
		t.Fatal("no store to release")
	}
	if err := store.release(); err != nil {
		t.Fatalf("release: %s", err)
	}
	err := store.appendBoundedMany(providerStoreBucketIdentityEvents, []any{map[string]string{"outcome": "ok"}}, identityEventKeep)
	if !errors.Is(err, errProviderStoreReleased) {
		t.Fatalf("append on a released handle = %v, want %v", err, errProviderStoreReleased)
	}
}

// A handle closed without a handoff (a moved state directory, a test reset)
// reports the same loss, and a nil handle reports it rather than panicking.
func TestProviderStoreAppendBoundedManyReportsAClosedHandle(t *testing.T) {
	closed := &providerStore{}
	err := closed.appendBoundedMany(providerStoreBucketIdentityEvents, []any{map[string]string{"outcome": "ok"}}, identityEventKeep)
	if !errors.Is(err, errProviderStoreClosed) {
		t.Fatalf("append on a closed handle = %v, want %v", err, errProviderStoreClosed)
	}
	var nilStore *providerStore
	err = nilStore.appendBoundedMany(providerStoreBucketIdentityEvents, []any{map[string]string{"outcome": "ok"}}, identityEventKeep)
	if !errors.Is(err, errProviderStoreClosed) {
		t.Fatalf("append on a nil handle = %v, want %v", err, errProviderStoreClosed)
	}
}

// The error must not turn a working append into a failure: an empty batch is
// still nothing to do and a usable handle still records.
func TestProviderStoreAppendBoundedManyStillRecords(t *testing.T) {
	testExtenderState(t)
	store := providerStoreInstance()
	if err := store.appendBoundedMany(providerStoreBucketIdentityEvents, nil, identityEventKeep); err != nil {
		t.Fatalf("append of no records = %v, want nil", err)
	}
	if err := store.appendBoundedMany(providerStoreBucketIdentityEvents, []any{map[string]string{"outcome": "ok"}}, identityEventKeep); err != nil {
		t.Fatalf("append of one record = %v, want nil", err)
	}
}
