package miner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

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
