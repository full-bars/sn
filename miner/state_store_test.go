package miner

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// A record that could not be read is not a record that was never written: the
// callers save on absence, so reporting both as false is how a damaged or
// unavailable store ends up replaced with an empty record.
func TestProviderStoreLoadDistinguishesAbsentFromUnreadable(t *testing.T) {
	testExtenderState(t)
	store := providerStoreInstance()
	if store == nil {
		t.Fatal("no store")
	}

	var absent map[string]int
	found, err := store.load(providerStoreBucketState, "never-written", &absent)
	if found || err != nil {
		t.Fatalf("load of an absent record = (found=%t, err=%v), want (false, nil)", found, err)
	}

	seedRawValue(t, store, "torn", "{not json")
	var torn map[string]int
	found, err = store.load(providerStoreBucketState, "torn", &torn)
	if found || err == nil {
		t.Fatalf("load of a value that will not decode = (found=%t, err=%v), want (false, error)", found, err)
	}

	// A handle that cannot read at all reports that too, rather than an
	// empty record the caller would save back.
	released := providerStoreInstance()
	if err := released.release(); err != nil {
		t.Fatalf("release: %s", err)
	}
	found, err = released.load(providerStoreBucketState, "torn", &torn)
	if found || !errors.Is(err, errProviderStoreReleased) {
		t.Fatalf("load on a released handle = (found=%t, err=%v), want (false, %v)", found, err, errProviderStoreReleased)
	}
}

func TestProviderStoreContainsTransactionPanic(t *testing.T) {
	store, err := openProviderStore(filepath.Join(t.TempDir(), providerStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = runProviderStoreUpdate(store.db, func(*bbolt.Tx) error { panic("damaged page") })
	if err == nil {
		t.Fatal("transaction panic returned nil")
	}
	if err := store.save(providerStoreBucketState, "after-panic", map[string]int{"ok": 1}); err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	found, err := store.load(providerStoreBucketState, "after-panic", &got)
	if err != nil || !found || got["ok"] != 1 {
		t.Fatalf("load after panic = %t, %#v, %v", found, got, err)
	}
}

func TestProviderStoreCountsCorruptionAndReplacesFile(t *testing.T) {
	testExtenderState(t)
	counts := withPersistentErrorStore(t)
	path, err := providerStatePath(providerStoreFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("garbage bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := openProviderStoreForProcess()
	if err != nil || store == nil {
		t.Fatalf("open recovered store = %v, %v", store, err)
	}
	if got := counts.Snapshot()[providerStoreCorruptCategory]; got != 1 {
		t.Fatalf("corrupt count = %d, want 1", got)
	}
	if err := store.save(providerStoreBucketState, "usable", true); err != nil {
		t.Fatal(err)
	}
}

func TestProviderStoreCountsFailedWritesButNotReleasedWrites(t *testing.T) {
	counts := withPersistentErrorStore(t)
	store, err := openProviderStore(filepath.Join(t.TempDir(), providerStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.save(providerStoreBucketState, "failed", true); err == nil {
		t.Fatal("write on closed database succeeded")
	}
	if got := counts.Snapshot()[providerStoreWriteFailedCategory]; got != 1 {
		t.Fatalf("write failure count = %d, want 1", got)
	}
	released, err := openProviderStore(filepath.Join(t.TempDir(), providerStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := released.release(); err != nil {
		t.Fatal(err)
	}
	if err := released.save(providerStoreBucketState, "released", true); !errors.Is(err, errProviderStoreReleased) {
		t.Fatalf("released write = %v", err)
	}
	if got := counts.Snapshot()[providerStoreWriteFailedCategory]; got != 1 {
		t.Fatalf("released write changed failure count to %d", got)
	}
}

// The activation gate decides from the record, so a record it cannot read must
// not be replaced by the one it would have written: the accounts the stored
// value still holds are the operator's own budget history.
func TestExtenderActivationRefusesWhenTheRecordCannotBeDecoded(t *testing.T) {
	testExtenderState(t)
	store := providerStoreInstance()
	seedRawValue(t, store, providerStoreKeyExtenderActivation, "{not json")

	now := time.Unix(1_800_000_000, 0)
	allow, wait, used := extenderActivationAllow(now, postsPerDualStackPass, extenderTestAccountA)
	if allow {
		t.Fatal("a start was allowed on a record that could not be read")
	}
	if wait != providerStoreRetryInterval {
		t.Fatalf("wait = %s, want %s", wait, providerStoreRetryInterval)
	}
	if used != 0 {
		t.Fatalf("used = %d, want 0", used)
	}
	if got := rawValue(t, store, providerStoreKeyExtenderActivation); got != "{not json" {
		t.Fatalf("stored record = %q, want the value that was there: the refused start overwrote it", got)
	}
}

// The measurement is the only writer of the capability record, so it is the one
// place a failed read could replace the stored history with a record built from
// nothing. It must leave the record alone and report the measurement locally.
func TestMeasureHostCapabilityLeavesAnUnreadableRecordAlone(t *testing.T) {
	testExtenderState(t)
	store := providerStoreInstance()
	seedRawValue(t, store, providerStoreKeyHostCapability, "{not json")

	// The probe is not what this test measures, and the address precheck
	// answers without a socket.
	original := ipv6GlobalAddressPresent
	t.Cleanup(func() { ipv6GlobalAddressPresent = original })
	ipv6GlobalAddressPresent = func() bool { return false }

	now := time.Unix(1_800_000_000, 0)
	measured := measureHostCapability(now)
	if measured.CheckedAtUnix != now.Unix() {
		t.Fatalf("measured record = %+v, want this measurement reported locally", measured)
	}
	if got := rawValue(t, store, providerStoreKeyHostCapability); got != "{not json" {
		t.Fatalf("stored capability = %q, want the value that was there: the measurement overwrote a record it could not read", got)
	}
}

// seedRawValue writes raw bytes under a state record key, standing in for a
// value a later build wrote, a torn page or a decode this build does not
// understand. save cannot produce one: it marshals its argument.
func seedRawValue(t *testing.T, store *providerStore, key, raw string) {
	t.Helper()
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(providerStoreBucketState))
		if err != nil {
			return err
		}
		return bucket.Put([]byte(key), []byte(raw))
	}); err != nil {
		t.Fatalf("seed %s: %s", key, err)
	}
}

// rawValue is the stored bytes for one key, so a test can tell a rewritten
// record from the one that was there.
func rawValue(t *testing.T, store *providerStore, key string) string {
	t.Helper()
	var raw []byte
	if err := store.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(providerStoreBucketState))
		if bucket == nil {
			return nil
		}
		raw = append([]byte(nil), bucket.Get([]byte(key))...)
		return nil
	}); err != nil {
		t.Fatalf("read %s: %s", key, err)
	}
	return string(raw)
}

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

// A value this build cannot decode is not a mystery the retry loop should
// swallow: it holds whatever role depends on it, so it must be counted the way
// a corrupt file is.
func TestProviderStoreCountsAnUndecodableRecord(t *testing.T) {
	counts := withPersistentErrorStore(t)
	store, err := openProviderStore(filepath.Join(t.TempDir(), providerStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(providerStoreBucketState))
		if err != nil {
			return err
		}
		return b.Put([]byte("bad"), []byte("{not json"))
	}); err != nil {
		t.Fatal(err)
	}
	var out extenderActivationRecord
	if _, err := store.load(providerStoreBucketState, "bad", &out); err == nil {
		t.Fatal("an undecodable record loaded without error")
	}
	if got := counts.Snapshot()[providerStoreDecodeFailedCategory]; got != 1 {
		t.Fatalf("decode failure count = %d, want 1", got)
	}
}

// bbolt's Check walks pages in its own goroutine, so a damaged page panics
// outside any recover the store owns and kills the process. Opening a store with
// one clobbered page must return an error or succeed — never crash.
func TestProviderStoreOpenSurvivesDamagedPage(t *testing.T) {
	path := filepath.Join(t.TempDir(), providerStoreFileName)
	store, err := openProviderStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := store.save(providerStoreBucketState, fmt.Sprintf("k%d", i), strings.Repeat("x", 256)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.release(); err != nil {
		t.Fatal(err)
	}

	// Clobber the page id of a page past the two meta pages and the freelist.
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	pageSize := int64(os.Getpagesize())
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xff}, 8), pageSize*3); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// The assertion is simply that this call returns rather than taking the
	// process down; either outcome is acceptable.
	if reopened, err := openProviderStore(path); err == nil {
		_ = reopened.release()
	}
}

// A record that will not decode inside an update is one failure, counted as a
// decode failure. No write was attempted, so it must not also count as a failed
// write: the two categories are what an operator reads to tell a damaged value
// from a disk that will not take writes.
func TestProviderStoreUndecodableUpdateIsNotAFailedWrite(t *testing.T) {
	counts := withPersistentErrorStore(t)
	store, err := openProviderStore(filepath.Join(t.TempDir(), providerStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(providerStoreBucketState))
		if err != nil {
			return err
		}
		return b.Put([]byte("bad"), []byte("{not json"))
	}); err != nil {
		t.Fatal(err)
	}
	var out extenderActivationRecord
	if err := store.updateJSON(providerStoreBucketState, "bad", &out, func(bool) error { return nil }); err == nil {
		t.Fatal("an undecodable record updated without error")
	}
	snapshot := counts.Snapshot()
	if got := snapshot[providerStoreDecodeFailedCategory]; got != 1 {
		t.Fatalf("decode failure count = %d, want 1", got)
	}
	if got := snapshot[providerStoreWriteFailedCategory]; got != 0 {
		t.Fatalf("write failure count = %d, want 0: no write was attempted", got)
	}
}

// A key that holds a zero-length value is a record that exists and says
// nothing, not a record that was never written. Reading it as absent would let
// the caller save over it without the failure being counted.
func TestProviderStoreLoadCountsAnEmptyValue(t *testing.T) {
	counts := withPersistentErrorStore(t)
	store, err := openProviderStore(filepath.Join(t.TempDir(), providerStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(providerStoreBucketState))
		if err != nil {
			return err
		}
		return b.Put([]byte("empty"), []byte{})
	}); err != nil {
		t.Fatal(err)
	}
	var out extenderActivationRecord
	found, err := store.load(providerStoreBucketState, "empty", &out)
	if err == nil {
		t.Fatalf("an empty stored value loaded as (found=%t, nil), want a decode error", found)
	}
	if got := counts.Snapshot()[providerStoreDecodeFailedCategory]; got != 1 {
		t.Fatalf("decode failure count = %d, want 1", got)
	}
	if found, err := store.load(providerStoreBucketState, "never-written", &out); found || err != nil {
		t.Fatalf("a key that was never written loaded as (found=%t, %v), want (false, nil)", found, err)
	}
}
