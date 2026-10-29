package miner

import (
	"os"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestQuarantineHoldWriteFailureAndRestart(t *testing.T) {
	testExtenderState(t)
	closeProviderStore()
	path, err := providerStatePath(providerStoreFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("damaged database"), 0600); err != nil {
		t.Fatal(err)
	}
	providerStoreOpenedHook = func(db *bbolt.DB) { _ = db.Close() }
	store, err := openProviderStoreForProcess()
	providerStoreOpenedHook = nil
	if err == nil || store != nil {
		t.Fatalf("published store after failed hold: store=%v err=%v", store, err)
	}
	if _, err := os.Stat(path + ".recovery"); err != nil {
		t.Fatalf("recovery marker missing: %v", err)
	}
	closeProviderStore()
	store, err = openProviderStoreForProcess()
	if err != nil || store == nil {
		t.Fatalf("restart recovery: %v", err)
	}
	if allow, _, _ := extenderActivationAllow(time.Now(), 1, extenderTestAccountA); allow {
		t.Fatal("restart admitted an account without its hold")
	}
}
