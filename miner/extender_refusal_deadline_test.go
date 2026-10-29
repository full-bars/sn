package miner

import (
	"testing"
	"time"
)

func TestSealDeadlineIncludesInitialDurabilityCheck(t *testing.T) {
	testExtenderState(t)
	shortenRefusalGate(t)
	release, err := lockExtenderRefusalBarrier()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sealExtenderRefusals() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("seal succeeded while its initial check was blocked")
		}
	case <-time.After(2 * time.Second):
		t.Error("seal waited indefinitely for its initial durability check")
	}
	release()
	extenderSealAttempts.Wait()
	extenderRefusalsHeld.mu.Lock()
	sealed := extenderRefusalsHeld.sealed
	extenderRefusalsHeld.mu.Unlock()
	if sealed {
		t.Error("abandoned seal became active later")
	}
}

func TestRefusalShutdownDeadlineIncludesHeldMutex(t *testing.T) {
	testExtenderState(t)
	shortenRefusalGate(t)
	extenderRefusalsHeld.mu.Lock()
	done := make(chan bool, 1)
	go func() { done <- waitExtenderRefusalsDurable("shutdown") }()
	select {
	case durable := <-done:
		if durable {
			t.Error("reported a blocked attempt durable")
		}
	case <-time.After(2 * time.Second):
		t.Error("shutdown waited for the held mutex")
	}
	extenderRefusalsHeld.mu.Unlock()
	extenderDurableAttempts.Wait()
	extenderLossAttempts.Wait()
}

func TestRefusalShutdownDeadlineIncludesLossRecord(t *testing.T) {
	testExtenderState(t)
	shortenRefusalGate(t)
	queueAtRiskRefusal("scope.deadline")
	release, err := lockExtenderRefusalBarrier()
	if err != nil {
		t.Fatal(err)
	}
	critLogMu.Lock()
	done := make(chan bool, 1)
	go func() { done <- waitExtenderRefusalsDurable("shutdown") }()
	select {
	case durable := <-done:
		if durable {
			t.Error("reported a blocked attempt durable")
		}
	case <-time.After(2 * time.Second):
		t.Error("shutdown waited for the loss record")
	}
	critLogMu.Unlock()
	release()
	extenderDurableAttempts.Wait()
	extenderLossAttempts.Wait()
}
