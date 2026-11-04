package miner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestProofRejectionWriterPrunesAndBounds(t *testing.T) {
	dir := t.TempDir()
	rec := legacyImportProofRecord{ClientID: connect.NewId().String(), NetworkID: connect.NewId().String(), KeyFP: legacyImportFingerprint([]byte("key")), TokenFP: legacyImportFingerprint([]byte("token")), Proof: "refresh", ProvenAt: time.Now().Unix()}
	slots := map[string]legacyImportProofRecord{}
	for i := 0; i < legacyImportProofStoreMaxEntries; i++ {
		slots[fmt.Sprintf("slot-%d", i)] = rec
	}
	expired := rec
	expired.ProvenAt = time.Now().Add(-legacyImportProofMaxAge - time.Hour).Unix()
	slots["expired"] = expired
	raw, err := json.Marshal(legacyImportProofFile{Schema: legacyImportProofStoreSchema, Slots: slots})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, legacyImportProofStoreName), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := noteLegacyImportProofRejected(dir, "new-rejection", "revoked"); err != nil {
		t.Fatal(err)
	}
	got := loadLegacyImportProofStore(dir).slots
	if len(got) > legacyImportProofStoreMaxEntries {
		t.Fatalf("unbounded entries: %d", len(got))
	}
	if _, ok := got["expired"]; ok {
		t.Fatal("expired proof was not pruned")
	}
	if got["new-rejection"].LastRejection != "revoked" {
		t.Fatal("new rejection was pruned")
	}
}
