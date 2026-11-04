package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func statOrFatal(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func noTempLeft(t *testing.T, dir string) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(dir, ".client_jwts-*.tmp"))
	if len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// Tests that saving an entry that is already byte-identical on disk skips the
// write: the file inode is untouched, no temp file is created, and flushes does
// not increment while skipped increments.
func TestClientJWTStoreUnchangedPutWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client_jwts.json")
	store := newClientJWTStore(path)

	entry := clientJWTEntry{
		ByClientJWT: "token-1",
		ClientID:    testClientId,
		NetworkID:   "net-1",
		MintedAt:    time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
	}

	if err := store.Put("proxy-1", entry); err != nil {
		t.Fatal(err)
	}
	before := statOrFatal(t, store.path)

	// Second identical Put should skip
	if err := store.Put("proxy-1", entry); err != nil {
		t.Fatal(err)
	}
	after := statOrFatal(t, store.path)

	if !os.SameFile(before, after) {
		t.Fatal("an unchanged Put replaced the store file inode")
	}
	if store.flushes != 1 || store.skipped != 1 {
		t.Fatalf("flushes=%d skipped=%d, want 1 and 1", store.flushes, store.skipped)
	}
	noTempLeft(t, dir)
}

// Tests that a changed Put performs the write and replaces the on-disk file.
func TestClientJWTStoreChangedPutWritesOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client_jwts.json")
	store := newClientJWTStore(path)

	entry1 := clientJWTEntry{
		ByClientJWT: "token-1",
		ClientID:    testClientId,
		NetworkID:   "net-1",
		MintedAt:    time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
	}
	entry2 := clientJWTEntry{
		ByClientJWT: "token-2",
		ClientID:    testClientId,
		NetworkID:   "net-1",
		MintedAt:    time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
	}

	if err := store.Put("proxy-1", entry1); err != nil {
		t.Fatal(err)
	}
	before := statOrFatal(t, store.path)

	// Changed Put must write to disk
	if err := store.Put("proxy-1", entry2); err != nil {
		t.Fatal(err)
	}
	after := statOrFatal(t, store.path)

	if os.SameFile(before, after) {
		t.Fatal("a changed Put did not replace the store file")
	}
	if store.flushes != 2 || store.skipped != 0 {
		t.Fatalf("flushes=%d (want 2) skipped=%d (want 0)", store.flushes, store.skipped)
	}
	got, ok := newClientJWTStore(path).Get("proxy-1")
	if !ok || got.ByClientJWT != "token-2" {
		t.Fatalf("reloaded store got %+v ok=%v, want token-2", got, ok)
	}
	noTempLeft(t, dir)
}

// Tests that the skip does not fire when the on-disk file is absent or corrupt.
func TestClientJWTStoreSkipDoesNotFireWhenFileAbsentOrCorrupt(t *testing.T) {
	entry := clientJWTEntry{
		ByClientJWT: "token-1",
		ClientID:    testClientId,
		NetworkID:   "net-1",
		MintedAt:    time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
	}

	t.Run("absent", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "client_jwts.json")
		store := newClientJWTStore(path)

		if err := store.Put("proxy-1", entry); err != nil {
			t.Fatal(err)
		}
		if store.flushes != 1 || store.skipped != 0 {
			t.Fatalf("initial write: flushes=%d skipped=%d", store.flushes, store.skipped)
		}

		// Delete the on-disk file underneath
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}

		// Putting the same in-memory entry must NOT skip because disk file is absent
		if err := store.Put("proxy-1", entry); err != nil {
			t.Fatal(err)
		}
		if store.flushes != 2 || store.skipped != 0 {
			t.Fatalf("after remove: flushes=%d (want 2) skipped=%d (want 0)", store.flushes, store.skipped)
		}

		// Verify file is recreated on disk
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected store file to be recreated: %v", err)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "client_jwts.json")
		store := newClientJWTStore(path)

		if err := store.Put("proxy-1", entry); err != nil {
			t.Fatal(err)
		}
		if store.flushes != 1 || store.skipped != 0 {
			t.Fatalf("initial write: flushes=%d skipped=%d", store.flushes, store.skipped)
		}

		// Corrupt the on-disk file underneath
		if err := os.WriteFile(path, []byte("not valid json content"), 0600); err != nil {
			t.Fatal(err)
		}

		// Putting the same in-memory entry must NOT skip because disk file is corrupt
		if err := store.Put("proxy-1", entry); err != nil {
			t.Fatal(err)
		}
		if store.flushes != 2 || store.skipped != 0 {
			t.Fatalf("after corrupt: flushes=%d (want 2) skipped=%d (want 0)", store.flushes, store.skipped)
		}

		// Verify file was restored with valid JSON
		reloaded := newClientJWTStore(path)
		got, ok := reloaded.Get("proxy-1")
		if !ok || got.ByClientJWT != "token-1" {
			t.Fatalf("reloaded corrupt file got %+v ok=%v, want token-1", got, ok)
		}
	})
}

// Tests that deleting a key already absent from disk writes nothing.
func TestClientJWTStoreDeleteAlreadyMissingKeyWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client_jwts.json")
	store := newClientJWTStore(path)

	entry1 := clientJWTEntry{ByClientJWT: "t1", ClientID: testClientId, MintedAt: time.Now()}
	entry2 := clientJWTEntry{ByClientJWT: "t2", ClientID: testClientId, MintedAt: time.Now()}
	if err := store.Put("p1", entry1); err != nil {
		t.Fatal(err)
	}
	if err := store.Put("p2", entry2); err != nil {
		t.Fatal(err)
	}

	// Another process removes p1 from disk
	other := newClientJWTStore(path)
	if err := other.Delete("p1"); err != nil {
		t.Fatal(err)
	}

	before := statOrFatal(t, path)
	flushesBefore := store.flushes
	skipsBefore := store.skipped

	// Current store still has p1 in-memory, deletes it: on disk p1 is already gone
	if err := store.Delete("p1"); err != nil {
		t.Fatal(err)
	}

	after := statOrFatal(t, path)
	if !os.SameFile(before, after) {
		t.Fatal("deleting an already gone key rewrote the file")
	}
	if store.flushes != flushesBefore || store.skipped != skipsBefore+1 {
		t.Fatalf("flushes=%d (want %d) skipped=%d (want %d)", store.flushes, flushesBefore, store.skipped, skipsBefore+1)
	}
}

// Tests that skip logging is rate-limited to at most once per minute and suppresses bursts.
func TestClientJWTStoreSkipLoggingRateLimited(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client_jwts.json")
	store := newClientJWTStore(path)

	baseTime := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	clock := baseTime
	store.clock = func() time.Time {
		return clock
	}

	entry := clientJWTEntry{
		ByClientJWT: "token-stable",
		ClientID:    testClientId,
		NetworkID:   "net-1",
		MintedAt:    baseTime,
	}

	// Initial write
	if err := store.Put("proxy-1", entry); err != nil {
		t.Fatal(err)
	}

	// First skip logs immediately with 0 suppressed
	out := captureTlog(t, func() {
		if err := store.Put("proxy-1", entry); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "client_jwt_store: skip write 1 entries (identical content, 0 suppressed)") {
		t.Fatalf("expected first skip log, got:\n%s", out)
	}

	// Next 4 identical puts within the minute must be suppressed (no log output)
	out = captureTlog(t, func() {
		for i := 0; i < 4; i++ {
			clock = clock.Add(5 * time.Second)
			if err := store.Put("proxy-1", entry); err != nil {
				t.Fatal(err)
			}
		}
	})
	if strings.Contains(out, "skip write") {
		t.Fatalf("expected suppressed skips to produce no log output within 1m, got:\n%s", out)
	}

	// Advance clock past 1 minute; next skip must report the 4 suppressed writes
	clock = clock.Add(2 * time.Minute)
	out = captureTlog(t, func() {
		if err := store.Put("proxy-1", entry); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "client_jwt_store: skip write 1 entries (identical content, 4 suppressed)") {
		t.Fatalf("expected skip log reporting 4 suppressed, got:\n%s", out)
	}
}
