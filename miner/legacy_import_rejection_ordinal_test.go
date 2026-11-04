package miner

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestProofRejectionOrdinalSurvivesSuccessfulReproof(t *testing.T) {
	dir := t.TempDir()
	claim := legacyImportProofClaim{
		ClientID: connect.NewId().String(), NetworkID: connect.NewId().String(),
		KeyFP: legacyImportFingerprint([]byte("key")), TokenFP: legacyImportFingerprint([]byte("token")),
	}
	if err := noteLegacyImportProofRejected(dir, "slot", "first"); err != nil {
		t.Fatal(err)
	}
	first := loadLegacyImportProofStore(dir)
	stale := loadLegacyImportProofStore(dir)
	first.now = func() time.Time { return time.Now().Add(time.Minute) }
	stale.now = first.now
	first.record("slot", claim, "refresh")
	stale.record("slot", claim, "refresh")
	if err := first.flush(); err != nil {
		t.Fatal(err)
	}
	if err := noteLegacyImportProofRejected(dir, "slot", "second"); err != nil {
		t.Fatal(err)
	}
	if err := stale.flush(); err != nil {
		t.Fatal(err)
	}
	got := loadLegacyImportProofStore(dir).slots["slot"]
	if got.ProvenAt != 0 || got.LastRejection != "second" {
		t.Fatalf("stale reproof replaced newer rejection: %+v", got)
	}
}
