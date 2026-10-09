//go:build linux

package provider

import (
	"testing"
	"time"
)

// A stall leaves a lean start cap behind, like a thrash escape, so the restart
// systemd performs does not walk back into the same spiral.
func TestRecordLivenessStallLeavesALeanStartCap(t *testing.T) {
	withTempHome(t)
	lastRunningProxyCount.Store(500)
	t.Cleanup(func() { lastRunningProxyCount.Store(0) })

	recordLivenessStall()

	cap, ok := activeThrashCap(time.Now())
	if !ok || cap != 300 {
		t.Fatalf("expected an active cap of 300 (60%% of 500), got %d ok=%v", cap, ok)
	}
	if n := len(thrashRestartsWithin(readThrashCapState(), time.Now())); n != 1 {
		t.Fatalf("the stall must count in the anti-loop ring, ring has %d entries", n)
	}
}

func TestRecordLivenessStallCanBeUndone(t *testing.T) {
	withTempHome(t)
	lastRunningProxyCount.Store(500)
	t.Cleanup(func() { lastRunningProxyCount.Store(0) })

	before := readThrashCapState()
	undo := recordLivenessStall()
	if _, ok := activeThrashCap(time.Now()); !ok {
		t.Fatal("the stall must leave a cap in place until undone")
	}
	undo()
	after := readThrashCapState()
	if after.Cap != before.Cap || len(after.Restarts) != len(before.Restarts) {
		t.Fatalf("undo must restore the previous cap state, before=%+v after=%+v", before, after)
	}
	if _, ok := activeThrashCap(time.Now()); ok {
		t.Fatal("no cap may remain after the stall resolved itself")
	}
}
