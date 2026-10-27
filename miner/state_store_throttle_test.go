package miner

import (
	"os"
	"path/filepath"
	"testing"
)

// A store that will not open must be counted, and must not be re-attempted on
// every call: bbolt's open waits on the file lock, so an unthrottled retry
// stalls every caller for as long as the failure lasts.
func TestProviderStoreOpenFailureIsThrottledAndCounted(t *testing.T) {
	dir := t.TempDir()
	// A directory where the database file belongs makes bbolt's open fail
	// deterministically.
	if err := os.MkdirAll(filepath.Join(dir, providerStoreFileName), 0o700); err != nil {
		t.Fatalf("seed: %s", err)
	}
	t.Setenv("URNETWORK_STATE_DIR", dir)
	closeProviderStore()

	count := func() uint64 {
		FlushPersistentErrors()
		return persistentErrorsSeamSnapshot()[providerStoreUnavailableCategory]
	}

	before := count()
	if store := providerStoreInstance(); store != nil {
		t.Fatal("store opened on a directory")
	}
	afterFirst := count()
	if afterFirst != before+1 {
		t.Fatalf("failure not counted: %d -> %d", before, afterFirst)
	}

	if store := providerStoreInstance(); store != nil {
		t.Fatal("store opened on a directory")
	}
	if afterSecond := count(); afterSecond != afterFirst {
		t.Fatalf("open retried inside the retry interval: count %d -> %d", afterFirst, afterSecond)
	}

	// A deliberate close clears the throttle, so the next call tries again.
	closeProviderStore()
	_ = providerStoreInstance()
	if afterReset := count(); afterReset != afterFirst+1 {
		t.Fatalf("close did not clear the throttle: count %d -> %d", afterFirst, afterReset)
	}
}
