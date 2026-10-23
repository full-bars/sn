package miner

// The leg evidence is what tells a proxy that cannot carry traffic apart from
// one that is merely slow. These tests pin the decision itself: five failures
// in a row over ten minutes, with no success through the proxy, and only while
// another path reached the platform in the last five. The last condition is the
// outage guard, and it is the one a careless port gets wrong.

import (
	"testing"
	"time"
)

func TestProxyLegFlagsOnlyAfterTheStreakAndTheWindow(t *testing.T) {
	start := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	tracker := &proxyLegTracker{}

	for i := 0; i < proxyLegShedStreak; i++ {
		tracker.RecordFailure("a", start.Add(time.Duration(i)*time.Minute))
	}
	now := start.Add(5 * time.Minute)
	if tracker.Flagged("a", now, now.Add(-time.Minute)) {
		t.Fatal("flagged inside the ten minute window")
	}
	now = start.Add(proxyLegShedMinAge)
	if !tracker.Flagged("a", now, now.Add(-time.Minute)) {
		t.Fatal("a full streak over the window did not flag")
	}
	if tracker.Flagged("a", now, now.Add(-time.Minute)) {
		t.Fatal("flagged twice; the caller would log it twice")
	}
}

func TestProxyLegNeedsTheFullStreak(t *testing.T) {
	start := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	tracker := &proxyLegTracker{}
	for i := 0; i < proxyLegShedStreak-1; i++ {
		tracker.RecordFailure("a", start.Add(time.Duration(i)*time.Minute))
	}
	now := start.Add(time.Hour)
	if tracker.Flagged("a", now, now.Add(-time.Minute)) {
		t.Fatalf("flagged on %d failures, the streak is %d", proxyLegShedStreak-1, proxyLegShedStreak)
	}
}

func TestProxyLegSuccessVoidsTheEvidence(t *testing.T) {
	start := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	tracker := &proxyLegTracker{}
	for i := 0; i < proxyLegShedStreak; i++ {
		tracker.RecordFailure("a", start.Add(time.Duration(i)*time.Minute))
	}
	tracker.RecordSuccess("a", start.Add(20*time.Minute))
	now := start.Add(time.Hour)
	if tracker.Flagged("a", now, now.Add(-time.Minute)) {
		t.Fatal("flagged after a dial succeeded through the proxy")
	}
	// a new streak starts from zero, and the old window does not carry over
	tracker.RecordFailure("a", now)
	if tracker.Flagged("a", now.Add(time.Hour), now.Add(-time.Minute)) {
		t.Fatal("a single failure after a success flagged the proxy")
	}
}

func TestProxyLegOutageGuardBlocksEveryFlag(t *testing.T) {
	start := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	tracker := &proxyLegTracker{}
	for i := 0; i < proxyLegShedStreak; i++ {
		tracker.RecordFailure("a", start.Add(time.Duration(i)*time.Minute))
	}
	now := start.Add(proxyLegShedMinAge)
	if tracker.Flagged("a", now, time.Time{}) {
		t.Fatal("flagged with no path ever reaching the platform")
	}
	if tracker.Flagged("a", now, now.Add(-proxyLegPlatformRecent-time.Second)) {
		t.Fatal("flagged while the last platform success was older than the guard window")
	}
}

func TestProxyLegTotalsAndForget(t *testing.T) {
	start := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	tracker := &proxyLegTracker{}
	for i := 0; i < 3; i++ {
		tracker.RecordFailure("a", start)
	}
	for i := 0; i < proxyLegShedStreak; i++ {
		tracker.RecordFailure("b", start.Add(time.Duration(i)*time.Minute))
	}
	now := start.Add(proxyLegShedMinAge)
	tracker.Flagged("b", now, now.Add(-time.Minute))

	failures, flagged := tracker.Totals()
	if failures != 3+proxyLegShedStreak {
		t.Fatalf("totals failures = %d, expected %d", failures, 3+proxyLegShedStreak)
	}
	if flagged != 1 {
		t.Fatalf("totals flagged = %d, expected 1", flagged)
	}

	tracker.Forget("b")
	failures, flagged = tracker.Totals()
	if failures != 3 || flagged != 0 {
		t.Fatalf("after Forget totals = %d/%d, expected 3/0", failures, flagged)
	}
}
