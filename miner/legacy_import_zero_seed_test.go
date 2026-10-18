//go:build linux || darwin

// The live NY5 state: a 3.23-fix save whose retained .provider.key is the
// all-zero seed. v33 used it as a real key (readProviderClientKeySeed plus the
// caller's 0 < len(seed) check) and the server holds the public key it derives.
// The marker must derive, the automatic import must reach and pass the proof
// step instead of skipping provider_key_invalid, and the key gate must then
// open on the derived marker without any creation flag. The proof here is the
// network-free stub of the import fixture.
package miner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfoundation/sn/clientauth"
)

func TestLegacyImportZeroSeedProvesSavedIdentityAndOpensKeyGate(t *testing.T) {
	fixture := newLegacyImportFixture(t, false, 0)
	fixture.seed = make([]byte, ed25519.SeedSize)
	fixture.public = ed25519.NewKeyFromSeed(fixture.seed).Public().(ed25519.PublicKey)
	fixture.write(".provider.key", fixture.seed)
	fixture.prover.serviceKey[fixture.directClientID] = fixture.public

	marker, err := clientauth.ProviderKeyIdentityMarker(fixture.seed)
	if err != nil || len(marker) == 0 {
		t.Fatalf("the all-zero seed must derive its identity marker: %v", err)
	}
	fixture.captureOriginals()
	result := fixture.run(legacyImportHooks{})
	if len(result.mintSlots) != 0 {
		t.Fatalf("a proven zero-seed slot must not mint a replacement client: %v", result.mintSlots)
	}
	if fixture.prover.publicCalls == 0 {
		t.Fatal("the import never reached the proof step")
	}
	store, storeErr := readLegacyStoreRaw(fixture)
	raw, readErr := os.ReadFile(filepath.Join(fixture.dir, ".provider.jwt"))
	if storeErr != nil || readErr != nil || string(raw) != store[legacyImportDirectKey].ByClientJWT {
		t.Fatalf("the zero-seed import skipped the proven credential: %v %v", readErr, storeErr)
	}
	if got := fixture.read(legacyProviderMarkerName); !bytes.Equal(got, marker) {
		t.Fatal("the import wrote a marker other than the derived all-zero identity")
	}
	if slot := fixture.journal().Slots[legacyImportDirectKey]; slot.State != "imported" || slot.Proof != "server_key" {
		t.Fatalf("the zero-seed import did not commit through the proof: %#v", slot)
	}
	for _, skip := range fixture.recorder.skippedList() {
		if strings.HasPrefix(skip, "provider_key_invalid") {
			t.Fatalf("the zero seed was still refused: %v", fixture.recorder.skippedList())
		}
	}
	if committed, fallback, _, _ := fixture.recorder.counts(); committed != 1 || fallback != 0 {
		t.Fatalf("zero-seed import outcomes = %d committed, %d fallback", committed, fallback)
	}
	fixture.assertOriginalsUntouched()

	// The key gate, exactly as the run opens it before any provider worker.
	owner, err := clientauth.OpenProviderClientKey(context.Background(), filepath.Join(fixture.dir, ".provider.key"), clientauth.ProviderClientKeyOptions{})
	if err != nil || owner == nil {
		t.Fatalf("the key gate refused the identity the import just proved: %v", err)
	}
	defer owner.Close()
	if !bytes.Equal(owner.Seed(), fixture.seed) {
		t.Fatal("the key gate did not retain the all-zero seed")
	}
	if owner.RotationBackupName() != "" {
		t.Fatal("the key gate rotated without explicit creation")
	}
}
