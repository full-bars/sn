package miner

import "testing"

func TestProviderStoreSyncDirectory(t *testing.T) {
	if err := providerStoreSyncDir(t.TempDir()); err != nil {
		t.Fatalf("sync state directory: %v", err)
	}
}
