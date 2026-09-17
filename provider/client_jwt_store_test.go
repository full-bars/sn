package provider

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const testClientId = "00000000-0000-0000-0000-000000000001"

func TestClientJWTStoreMissingFile(t *testing.T) {
	store := newClientJWTStore(filepath.Join(t.TempDir(), "does-not-exist.json"))

	_, ok := store.Get("proxy-1")
	if ok {
		t.Fatal("expected no entry from a missing store file")
	}
}

func TestClientJWTStoreCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	store := newClientJWTStore(path)

	_, ok := store.Get("proxy-1")
	if ok {
		t.Fatal("expected no entry from a corrupt store file")
	}
}

func TestClientJWTStorePutGetRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	entry := clientJWTEntry{
		ByClientJWT: createFakeJWTWithClaims(map[string]interface{}{
			"client_id": testClientId,
			"exp":       float64(time.Now().Unix() + 86400),
		}),
		ClientID: testClientId,
		MintedAt: time.Now(),
	}
	if err := store.Put("proxy-1", entry); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	got, ok := store.Get("proxy-1")
	if !ok {
		t.Fatal("expected entry after Put")
	}
	if got.ClientID != testClientId {
		t.Errorf("ClientID = %q, want %q", got.ClientID, testClientId)
	}

	// A fresh store instance reading the same file should see the same entry.
	reloaded := newClientJWTStore(path)
	got, ok = reloaded.Get("proxy-1")
	if !ok {
		t.Fatal("expected entry to survive a reload from disk")
	}
	if got.ClientID != testClientId {
		t.Errorf("reloaded ClientID = %q, want %q", got.ClientID, testClientId)
	}
}

func TestClientJWTStorePruneStaleEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	if err := store.Put("stale-proxy", clientJWTEntry{
		ByClientJWT: "irrelevant",
		ClientID:    testClientId,
		MintedAt:    time.Now().Add(-clientJWTStaleAfter - 24*time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put("fresh-proxy", clientJWTEntry{
		ByClientJWT: "irrelevant",
		ClientID:    testClientId,
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	reloaded := newClientJWTStore(path)
	if _, ok := reloaded.Get("stale-proxy"); ok {
		t.Error("expected stale entry to be pruned on load")
	}
	if _, ok := reloaded.Get("fresh-proxy"); !ok {
		t.Error("expected fresh entry to survive pruning")
	}
}

// TestClientJWTStorePruneSurvivesLaterFlush is a regression test: loadLocked
// prunes stale entries only in the in-memory map. flushLocked reloads the
// raw on-disk file (to merge concurrent writers) and, without re-applying
// the same filter, would write the stale entry straight back to disk the
// next time ANY other key flushed — silently undoing the prune. This puts a
// stale entry, reloads (pruning it in memory), flushes an unrelated key, and
// checks a fresh reload from disk still doesn't see the stale entry.
func TestClientJWTStorePruneSurvivesLaterFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	if err := store.Put("stale-proxy", clientJWTEntry{
		ByClientJWT: "irrelevant",
		ClientID:    testClientId,
		MintedAt:    time.Now().Add(-clientJWTStaleAfter - 24*time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// Reload: loadLocked prunes stale-proxy from this instance's in-memory
	// map, but the on-disk file still has it (loadLocked never writes).
	reloaded := newClientJWTStore(path)
	if _, ok := reloaded.Get("stale-proxy"); ok {
		t.Fatal("expected stale entry to be pruned on load")
	}

	// Flush an unrelated key. Before the fix, flushLocked's on-disk reload
	// pulls stale-proxy back in from disk and writes it out again.
	if err := reloaded.Put("unrelated-proxy", clientJWTEntry{
		ByClientJWT: "irrelevant",
		ClientID:    testClientId,
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// A third, independent instance reading the same file must not see the
	// stale entry resurrected by the flush above.
	final := newClientJWTStore(path)
	if _, ok := final.Get("stale-proxy"); ok {
		t.Fatal("stale entry was resurrected on disk by an unrelated flush")
	}
	if _, ok := final.Get("unrelated-proxy"); !ok {
		t.Fatal("expected the unrelated flush's own entry to persist")
	}
}

func TestClientJWTStoreDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	if err := store.Put("proxy-1", clientJWTEntry{
		ByClientJWT: "irrelevant",
		ClientID:    testClientId,
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete("proxy-1"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, ok := store.Get("proxy-1"); ok {
		t.Fatal("expected entry to be gone after Delete")
	}

	// The eviction must persist to disk, not just the in-memory map.
	reloaded := newClientJWTStore(path)
	if _, ok := reloaded.Get("proxy-1"); ok {
		t.Fatal("expected deletion to survive a reload from disk")
	}
}

func TestClientJWTStoreDeleteMissingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	if err := store.Delete("never-existed"); err != nil {
		t.Fatalf("Delete of a missing key should be a no-op, got: %v", err)
	}
}

func TestClientJWTStoreConcurrentPut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := filepath.Join("proxy", string(rune('a'+n%26)))
			_ = store.Put(key, clientJWTEntry{
				ByClientJWT: "irrelevant",
				ClientID:    testClientId,
				MintedAt:    time.Now(),
			})
		}(i)
	}
	wg.Wait()

	if _, ok := store.Get("proxy/a"); !ok {
		t.Error("expected at least one concurrent Put to have landed")
	}
}

func TestClientJWTStoreAnyNetworkID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_jwts.json")
	store := newClientJWTStore(path)

	if got := store.AnyNetworkID(); got != "" {
		t.Fatalf("expected empty NetworkID from empty store, got %q", got)
	}

	_ = store.Put("p1", clientJWTEntry{
		ClientID: testClientId,
		MintedAt: time.Now(),
	})
	if got := store.AnyNetworkID(); got != "" {
		t.Fatalf("expected empty NetworkID when entries have no NetworkID, got %q", got)
	}

	_ = store.Put("p2", clientJWTEntry{
		ClientID:  testClientId,
		NetworkID: "net-test-123",
		MintedAt:  time.Now(),
	})
	if got := store.AnyNetworkID(); got != "net-test-123" {
		t.Fatalf("AnyNetworkID() = %q, want net-test-123", got)
	}

	// Conflicting network IDs in store must return empty (ambiguous)
	_ = store.Put("p3", clientJWTEntry{
		ClientID:  testClientId,
		NetworkID: "net-conflicting-456",
		MintedAt:  time.Now(),
	})
	if got := store.AnyNetworkID(); got != "" {
		t.Fatalf("AnyNetworkID() with conflicting networks = %q, want empty (ambiguous)", got)
	}
}
