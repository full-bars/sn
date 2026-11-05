package miner

import (
	"path/filepath"
	"testing"
)

// An unchanged update must not commit again, while a changed record must commit.
func TestProviderStoreSkipsAnUnchangedJSONRecord(t *testing.T) {
	store, err := openProviderStore(filepath.Join(t.TempDir(), providerStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	record := map[string]int{"posts": 1}
	key := "unchanged-record"
	before := providerStoreCommits.Load()
	if err := store.updateJSON(providerStoreBucketState, key, &record, func(bool) error { return nil }); err != nil {
		t.Fatalf("first write: %v", err)
	}
	committed := providerStoreCommits.Load()
	if committed != before+1 {
		t.Fatalf("first write did not commit: want %d, got %d", before+1, committed)
	}

	// the same value again, as a held recheck produces when nothing changed
	if err := store.updateJSON(providerStoreBucketState, key, &record, func(bool) error { return nil }); err != nil {
		t.Fatalf("an unchanged record must not be an error: %v", err)
	}
	if got := providerStoreCommits.Load(); got != committed {
		t.Fatalf("unchanged record committed again: %d -> %d", committed, got)
	}

	var got map[string]int
	found, err := store.load(providerStoreBucketState, key, &got)
	if err != nil || !found || got["posts"] != 1 {
		t.Fatalf("the skipped write changed the stored value: found=%t %#v %v", found, got, err)
	}

	// a real change must still commit
	if err := store.updateJSON(providerStoreBucketState, key, &record, func(bool) error {
		record["posts"] = 2
		return nil
	}); err != nil {
		t.Fatalf("changed write: %v", err)
	}
	if after := providerStoreCommits.Load(); after != committed+1 {
		t.Fatalf("a changed record did not commit: want %d, got %d", committed+1, after)
	}
}
