package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An elapsed deadline proves only that the attempts failed until then. The
// refusal's wait must outlive the process that lost it: once the barrier is
// free again, a new process (memory gone) is still held.
func TestLostRefusalStillHoldsTheNextDecision(t *testing.T) {
	testExtenderState(t)
	shortenRefusalGate(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	queueAtRiskRefusal("scope.1801")
	release, err := lockExtenderRefusalBarrier()
	if err != nil {
		t.Fatal(err)
	}
	if waitExtenderRefusalsDurable("shutdown") {
		release()
		t.Fatal("reported durable while the barrier was held")
	}
	// The process ends here: its memory goes before the abandoned attempt
	// can keep anything, so only the hold can stop the next start.
	resetExtenderRefusals()
	release()
	extenderDurableAttempts.Wait()
	closeProviderStore()
	allow, wait, _ := extenderActivationAllow(time.Now(), 1, extenderTestAccountA)
	if allow || wait <= 0 {
		t.Fatalf("the next start was admitted (wait %s) after a refusal was lost", wait)
	}
}

// The hold is read by every decision, so a process that already has the store
// open when another writes it is held too, not only the next open.
func TestLossHoldAppliesToAStoreAlreadyOpen(t *testing.T) {
	testExtenderState(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	if err := holdExtenderRoleAfterLostRefusal(time.Now()); err != nil {
		t.Fatal(err)
	}
	if allow, wait, _ := extenderActivationAllow(time.Now(), 1, extenderTestAccountA); allow || wait <= 0 {
		t.Fatalf("an open store admitted past the loss hold (wait %s)", wait)
	}
}

// A loss record does not share the event log that another process can rotate.
func TestLossRecordSurvivesOrdinaryLogRotation(t *testing.T) {
	testExtenderState(t)
	path, err := writeExtenderLossRecord("refusal evidence")
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := critLogPath()
	if err != nil {
		t.Fatal(err)
	}
	if path == ordinary {
		t.Fatal("loss record shares the rotating event log")
	}
	if err := os.WriteFile(ordinary, []byte(strings.Repeat("x", critLogMaxSize+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	critLog("rotate")
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "refusal evidence") {
		t.Fatalf("loss record after ordinary log rotation: %q, %v", data, err)
	}
}

func TestLossRecordFallsBackAndNamesTheCause(t *testing.T) {
	testExtenderState(t)
	path, err := providerStatePath("refusal-loss.log")
	if err != nil {
		t.Fatal(err)
	}
	// The dedicated log cannot be opened for append: its name is a directory.
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	fallback := filepath.Join(t.TempDir(), "events.log")
	original := extenderLossRecordFallbacks
	extenderLossRecordFallbacks = func() []string { return []string{fallback} }
	t.Cleanup(func() { extenderLossRecordFallbacks = original })

	busy := awaitBusy(t)
	queueAtRiskRefusal("scope.1901")
	recordExtenderRefusalsLost("shutdown", busy)
	data, err := os.ReadFile(fallback)
	if err != nil {
		t.Fatalf("no loss record in the fallback: %v", err)
	}
	line := string(data)
	if !strings.Contains(line, "EXTENDER REFUSALS LOST at shutdown: 1 decided") || !strings.Contains(line, "still blocked") {
		t.Fatalf("loss record = %q, want the count and that the attempt was still blocked", line)
	}
}

// awaitBusy is the error a wait gives up with while the barrier is held.
func awaitBusy(t *testing.T) error {
	t.Helper()
	release, err := lockExtenderRefusalBarrier()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { release(); extenderDurableAttempts.Wait() }()
	return awaitExtenderRefusalsDurable(time.Now().Add(50 * time.Millisecond))
}

// Failures are preserved even if the last attempt is still running when the
// deadline fires. Scheduling or another writer can delay that last attempt.
func TestLossCauseSaysTheStoreWasUnwritable(t *testing.T) {
	testExtenderState(t)
	shortenRefusalGate(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	failJournalRename(t)
	releaseProviderStore()
	queueAtRiskRefusal("scope.2001")
	err := awaitExtenderRefusalsDurable(time.Now().Add(extenderRefusalGateDeadline))
	if err == nil || (!strings.Contains(err.Error(), "stayed unwritable") && !strings.Contains(err.Error(), "the attempt before failed")) {
		t.Fatalf("cause = %v, want the prior store and journal failure reported", err)
	}
}
