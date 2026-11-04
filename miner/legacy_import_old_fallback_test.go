package miner

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestImportFallbackJournalFailureMustNotPermitMint(t *testing.T) {
	oldBackoff := legacyImportTransientBackoff
	legacyImportTransientBackoff = 0
	defer func() { legacyImportTransientBackoff = oldBackoff }()
	f := newLegacyImportFixture(t, true, 2)
	slot := f.proxySettings[0]
	store, err := readLegacyStoreRaw(f)
	if err != nil {
		t.Fatal(err)
	}
	delete(f.prover.serviceKey, f.proxyClientIDs[slot.Key()])
	f.prover.refreshErr[store[slot.Key()].ByClientJWT] = &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ETIMEDOUT}
	j := newLegacyImportJournal()
	j.Slots[slot.Key()] = legacyImportJournalSlot{State: "defer", Deferrals: legacyImportDeferralStarts - 1, FirstDeferredAt: time.Now().Unix()}
	if err := writeLegacyImportJournal(f.dir, j); err != nil {
		t.Fatal(err)
	}
	result := f.run(legacyImportHooks{afterMarkerWrite: func() error {
		path := filepath.Join(f.dir, legacyImportJournalName)
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.Mkdir(path, 0700)
	}})
	if result.mintAllowed(slot.Key()) {
		t.Fatal("transient fallback allowed mint without a durable decision")
	}
	defer withGlobalStore(t, filepath.Join(f.dir, legacyClientJwtStoreName))()
	setHotRestart(t, false)
	mint := func(context.Context) (string, connect.Id, error) { return "minted", connect.NewId(), nil }
	_, _, _, err = provideClientJWT(context.Background(), clientJWTProvideConfig{ClientJwtPath: f.target(slot), ProxySettings: slot, Mint: mint, FreshMint: mint})
	if err == nil {
		t.Fatal("mint proceeded with unreadable import journal")
	}
}

func TestImportFallbackLogoutKeepsDifferentOriginalIdentity(t *testing.T) {
	f := newLegacyImportFixture(t, true, 1)
	slot := f.proxySettings[0]
	store, err := readLegacyStoreRaw(f)
	if err != nil {
		t.Fatal(err)
	}
	original := store[slot.Key()]
	j := newLegacyImportJournal()
	j.Slots[slot.Key()] = legacyImportJournalSlot{State: "fallback", Reason: "proof_unavailable_bound", ClientID: original.ClientID, FallbackAt: time.Now().Unix()}
	if err := writeLegacyImportJournal(f.dir, j); err != nil {
		t.Fatal(err)
	}
	minted := legacyTestClientToken(f.networkID, connect.NewId().String(), connect.NewId().String())
	if err := os.WriteFile(f.target(slot), []byte(minted), 0600); err != nil {
		t.Fatal(err)
	}
	defer withGlobalStore(t, filepath.Join(f.dir, legacyClientJwtStoreName))()
	callbacks := &providerAuthenticationCallbacks{clientJwtPath: f.target(slot), networkJwtPath: filepath.Join(f.dir, "jwt"), identityKey: slot.Key(), cancel: func() {}}
	callbacks.AuthLogout()
	got, err := readLegacyStoreRaw(f)
	if err != nil {
		t.Fatal(err)
	}
	if got[slot.Key()].ClientID != original.ClientID {
		t.Fatal("minted identity logout deleted the original fallback identity")
	}
}

func TestImportFallbackPreservesOldStoreEntryAcrossMintAndOtherWrites(t *testing.T) {
	f := newLegacyImportFixture(t, true, 1)
	slot := f.proxySettings[0]
	store, err := readLegacyStoreRaw(f)
	if err != nil {
		t.Fatal(err)
	}
	old := store[slot.Key()]
	old.MintedAt = time.Now().Add(-2 * clientJWTStaleAfter)
	store[slot.Key()] = old
	raw, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	f.write(legacyClientJwtStoreName, raw)
	j := newLegacyImportJournal()
	j.Slots[slot.Key()] = legacyImportJournalSlot{State: "fallback", Reason: "proof_unavailable_bound", ClientID: old.ClientID, FallbackAt: time.Now().Unix()}
	if err := writeLegacyImportJournal(f.dir, j); err != nil {
		t.Fatal(err)
	}
	defer withGlobalStore(t, filepath.Join(f.dir, legacyClientJwtStoreName))()
	setHotRestart(t, false)
	minted := connect.NewId()
	mint := func(context.Context) (string, connect.Id, error) { return "minted", minted, nil }
	_, _, _, err = provideClientJWT(context.Background(), clientJWTProvideConfig{ClientJwtPath: f.target(slot), ProxySettings: slot, Mint: mint, FreshMint: mint})
	if err != nil {
		t.Fatal(err)
	}
	if err := loadGlobalClientJWTStore().Put("other", clientJWTEntry{ClientID: minted.String(), MintedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, err := readLegacyStoreRaw(f)
	if err != nil {
		t.Fatal(err)
	}
	if got[slot.Key()].ClientID != old.ClientID {
		t.Fatal("old fallback entry lost on mint or unrelated write")
	}
}
