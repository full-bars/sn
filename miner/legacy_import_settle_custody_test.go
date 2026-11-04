package miner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urfoundation/sn/clientauth"
	"github.com/urnetwork/connect"
)

func TestImportMintedFallbackSettlesAndDoesNotReprove(t *testing.T) {
	f := newLegacyImportFixture(t, false, 0)
	store, err := readLegacyStoreRaw(f)
	if err != nil {
		t.Fatal(err)
	}
	old := store[legacyImportDirectKey]
	old.MintedAt = time.Now().Add(-2 * clientJWTStaleAfter)
	store[legacyImportDirectKey] = old
	raw, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	f.write(legacyClientJwtStoreName, raw)
	j := newLegacyImportJournal()
	j.Slots[legacyImportDirectKey] = legacyImportJournalSlot{State: "fallback", Reason: "proof_unavailable_bound", ClientID: old.ClientID, FallbackAt: time.Now().Add(-time.Hour).Unix(), Target: filepath.Base(f.target(nil))}
	if err := writeLegacyImportJournal(f.dir, j); err != nil {
		t.Fatal(err)
	}

	// Use the real registration path and local synthetic server to produce the
	// credential and custody sidecars, rather than hand-writing a binding.
	marker, err := clientauth.ProviderKeyIdentityMarker(f.seed)
	if err != nil {
		t.Fatal(err)
	}
	f.write(legacyProviderMarkerName, marker)
	registration := newProviderRegistrationFixtureIn(t, f.dir)
	id, err := providerCaptureLaunchRegister(t, registration, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id.String() == old.ClientID {
		t.Fatal("mint reused the legacy identity")
	}
	f.write("jwt", []byte(legacyTestNetworkToken(f.networkID)))
	credential, err := os.ReadFile(f.target(nil))
	if err != nil {
		t.Fatal(err)
	}

	f.run(legacyImportHooks{})
	if got := f.journal().Slots[legacyImportDirectKey]; got.State != "imported" || got.Reason != "minted_identity_serving" {
		t.Errorf("minted fallback did not settle: %+v", got)
	}
	if err := os.Remove(filepath.Join(f.dir, legacyImportProofStoreName)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	f.resetProverCounts()
	f.run(legacyImportHooks{})
	public, refresh := f.proverCounts()
	t.Logf("second start re-proofs: public=%d refresh=%d", public, refresh)
	if public != 0 || refresh != 0 {
		t.Errorf("settled slot was re-admitted for re-proof")
	}
	got, err := os.ReadFile(f.target(nil))
	if err != nil || string(got) != string(credential) {
		t.Fatalf("serving credential changed: %v", err)
	}

	defer withGlobalStore(t, filepath.Join(f.dir, legacyClientJwtStoreName))()
	if err := loadGlobalClientJWTStore().Put("other", clientJWTEntry{ClientID: connect.NewId().String(), MintedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	gotStore, err := readLegacyStoreRaw(f)
	if err != nil || gotStore[legacyImportDirectKey].ClientID != old.ClientID {
		t.Fatalf("legacy entry lost after unrelated write: %v", err)
	}
	if err := os.Remove(f.target(nil)); err != nil {
		t.Fatal(err)
	}
	result := f.run(legacyImportHooks{})
	if !result.mintAllowed(legacyImportDirectKey) {
		t.Error("settlement blocked the existing mint path after credential loss")
	}
	setHotRestart(t, true)
	calls := 0
	mint := func(context.Context) (string, connect.Id, error) {
		calls++
		return "replacement", connect.NewId(), nil
	}
	_, _, reused, err := provideClientJWT(context.Background(), clientJWTProvideConfig{ClientJwtPath: f.target(nil), Mint: mint})
	if err != nil || reused || calls != 1 {
		t.Fatalf("missing credential: mint calls=%d reused=%t error=%v", calls, reused, err)
	}
	gotStore, err = readLegacyStoreRaw(f)
	if err != nil || gotStore[legacyImportDirectKey].ClientID != old.ClientID {
		t.Fatalf("legacy entry lost after mint: %v", err)
	}
}
