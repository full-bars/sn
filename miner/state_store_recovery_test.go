package miner

import (
	"errors"
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

// The marker is the only proof a quarantine happened, so it is published by
// the one rename that is durable on every platform. If that rename fails the
// damaged database stays where it is and no fresh store is handed out.
func TestQuarantineWaitsForADurableMarker(t *testing.T) {
	testExtenderState(t)
	closeProviderStore()
	path, err := providerStatePath(providerStoreFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("damaged database"), 0600); err != nil {
		t.Fatal(err)
	}
	original := providerStoreDurableRename
	t.Cleanup(func() { providerStoreDurableRename = original })
	providerStoreDurableRename = func(string, string) error { return errors.New("rename not durable") }
	if store, err := openProviderStoreForProcess(); err == nil || store != nil {
		t.Fatalf("quarantined without a durable marker: store=%v err=%v", store, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "damaged database" {
		t.Fatalf("damaged database moved before its marker was durable: %q, %v", data, err)
	}
}

// Windows cannot make the fresh database's name or the marker's removal
// durable. A crash that loses the fresh database after its hold was written
// must still reopen held, so the marker stands until the hold has run out.
func TestQuarantineHoldSurvivesALostFreshDatabase(t *testing.T) {
	testExtenderState(t)
	closeProviderStore()
	path, err := providerStatePath(providerStoreFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("damaged database"), 0600); err != nil {
		t.Fatal(err)
	}
	if store, err := openProviderStoreForProcess(); err != nil || store == nil {
		t.Fatalf("quarantine: %v", err)
	}
	recordExtenderActivationRefused(time.Now(), extenderTestAccountB)
	// Re-asserting the hold on a later open keeps what the record holds.
	closeProviderStore()
	if readRefusals(t, extenderTestAccountB) != 1 {
		t.Fatal("reopening under the marker dropped the record")
	}
	// The crash loses the fresh database.
	closeProviderStore()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if allow, _, _ := extenderActivationAllow(time.Now(), 1, extenderTestAccountA); allow {
		t.Fatal("a lost fresh database reopened without its hold")
	}
	// Once the hold has run out the marker goes and admission opens.
	closeProviderStore()
	store, err := openProviderStoreForProcess()
	if err != nil {
		t.Fatal(err)
	}
	if err := applyProviderStoreRecovery(store, path, time.Now().Add(extenderActivationWindow+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".recovery"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker outlived its hold: %v", err)
	}
}
