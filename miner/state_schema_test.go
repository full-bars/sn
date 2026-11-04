package miner

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestStateSchemaGuardRejectsFutureStore(t *testing.T) {
	dir := withTempHome(t)
	closeProviderStore()
	t.Cleanup(closeProviderStore)
	path := filepath.Join(dir, providerStoreFileName)
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("meta"))
		if err != nil {
			return err
		}
		return b.Put([]byte("schema"), []byte(`{"version":2,"min_reader":2}`))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := openProviderStoreForProcess()
	if store != nil || !errors.Is(err, errProviderStoreNewer) {
		t.Fatalf("future schema accepted: store=%v err=%v", store, err)
	}
	if !strings.Contains(err.Error(), "schema min_reader 2, this build reads up to 1") {
		t.Fatalf("unexpected refusal: %v", err)
	}
	t.Logf("refusal text: %s", err)
}
