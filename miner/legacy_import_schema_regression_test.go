package miner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect"
)

func TestProofStoreSchemaHasNoValidityFlag(t *testing.T) {
	dir := t.TempDir()
	store := loadLegacyImportProofStore(dir)
	claim := legacyImportProofClaim{ClientID: connect.NewId().String(), NetworkID: connect.NewId().String(), KeyFP: legacyImportFingerprint([]byte("key")), TokenFP: legacyImportFingerprint([]byte("token"))}
	store.record("slot", claim, "refresh")
	if err := store.flush(); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, legacyImportProofStoreName))
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["credential_validity_only"]; ok {
			t.Fatal("unexpected persisted schema field")
		}
	}
	check()
	if err := noteLegacyImportProofRejected(dir, "slot", "revoked"); err != nil {
		t.Fatal(err)
	}
	check()
}
