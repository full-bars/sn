package miner

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// After the handoff gate has passed, a refusal the parent decides is journaled
// as it arrives even inside the pace interval, so the candidate's first
// admission reads it. Left to the pace it sat in the parent's memory while the
// candidate decided.
func TestCandidateCannotAdmitPastARefusalDecidedAfterTheGate(t *testing.T) {
	testExtenderState(t)
	extenderPersistPace.Store(int64(time.Hour))
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	// The first refusal starts the pace interval, as a busy listener does.
	holdExtenderRefusal(extenderTestAccountA, "scope.1501")
	if err := sealExtenderRefusals(); err != nil {
		t.Fatal(err)
	}
	releaseProviderStore()
	holdExtenderRefusal(extenderTestAccountA, "scope.1502")

	// The parent's memory is not visible to the candidate: a new process.
	resetExtenderRefusals()
	dir, err := providerStateDir()
	if err != nil {
		t.Fatal(err)
	}
	onDisk, _, err := readExtenderRefusalJournal(time.Now())
	if err != nil || len(onDisk) != 1 || onDisk[0].attempt != "scope.1502" {
		t.Fatalf("journal the candidate reads = %v (%v), want the late refusal", onDisk, err)
	}
	closeProviderStore()
	if err := os.Remove(filepath.Join(dir, providerStoreFileName)); err != nil {
		t.Fatal(err)
	}
	if allow, wait, _ := extenderActivationAllow(time.Now(), 1, extenderTestAccountA); allow || wait <= 0 {
		t.Fatalf("the candidate's admission = %t (wait %s), want held by the late refusal", allow, wait)
	}
}

// A handoff abandoned after the gate returns to the paced journal, because the
// parent keeps serving.
func TestAbandonedHandoffUnseals(t *testing.T) {
	testExtenderState(t)
	if err := sealExtenderRefusals(); err != nil {
		t.Fatal(err)
	}
	unsealExtenderRefusals()
	extenderRefusalsHeld.mu.Lock()
	sealed := extenderRefusalsHeld.sealed
	extenderRefusalsHeld.mu.Unlock()
	if sealed {
		t.Fatal("queue still sealed after the handoff was abandoned")
	}
}
