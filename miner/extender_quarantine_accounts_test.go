package miner

import (
	"os"
	"testing"
	"time"
)

func TestQuarantineHoldSurvivesOtherAccountActivity(t *testing.T) {
	testExtenderState(t)
	closeProviderStore()
	path, err := providerStatePath(providerStoreFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("damaged database"), 0600); err != nil {
		t.Fatal(err)
	}
	if providerStoreInstance() == nil {
		t.Fatal("quarantine failed")
	}
	now := time.Now()
	recordExtenderActivationSucceeded(extenderTestAccountB)
	recordExtenderActivationRefused(now, extenderTestAccountB)
	if _, spent := extenderActivationSpendSpare(now, extenderTestAccountA, extenderActivationPass{V4: extenderActivationFamilyResult{Tried: true}}); spent {
		t.Fatal("spare spent during quarantine")
	}
	for _, account := range []string{extenderTestAccountA, extenderTestAccountB} {
		if allow, wait, _ := extenderActivationAllow(now, 1, account); allow || wait < 59*time.Minute {
			t.Fatalf("account %s: allow=%v wait=%v", account, allow, wait)
		}
	}
}
