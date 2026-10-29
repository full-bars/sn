package miner

import (
	"context"
	"testing"
	"time"
)

// awaitSlotLaunchResult cancels the slot context shortly after run starts and
// returns what run reported. Nothing else is ever cancelled.
func awaitSlotLaunchResult(t *testing.T, run func(context.Context) bool) bool {
	t.Helper()
	got := make(chan bool, 1)
	slotCtx, slotCancel := context.WithCancel(context.Background())
	defer slotCancel()
	go func() { got <- run(slotCtx) }()
	time.Sleep(50 * time.Millisecond)
	slotCancel()
	select {
	case ok := <-got:
		return ok
	case <-time.After(5 * time.Second):
		t.Fatal("launch wait ignored the slot context")
		return true
	}
}

// A slot retired during its launch stagger must not launch, and must return
// without waiting out the delay.
func TestAwaitSlotLaunchEndsWhenSlotCancelledDuringDelay(t *testing.T) {
	settings := reloadTestSettings("198.51.100.90:443", "alice", "pw")
	ok := awaitSlotLaunchResult(t, func(ctx context.Context) bool {
		return awaitSlotLaunch(ctx, time.Hour, 0, map[string]string{}, settings)
	})
	if ok {
		t.Fatal("a slot cancelled during its delay was allowed to launch")
	}
}

// Same for an unproven URL proxy held for the curated ramp.
func TestAwaitSlotLaunchEndsWhenSlotCancelledDuringURLHold(t *testing.T) {
	withTempHome(t)
	resetProxyWarmupForStart()
	prev := processStart
	processStart = time.Now()
	t.Cleanup(func() { processStart = prev })
	settings := reloadTestSettings("198.51.100.91:443", "alice", "pw")
	sourceOf := map[string]string{settings.Key(): "url"}
	if !shouldDeferURLWarmupLaunch("url", settings.Key(), time.Now()) {
		t.Fatal("test premise: the URL proxy must be held")
	}
	ok := awaitSlotLaunchResult(t, func(ctx context.Context) bool {
		return awaitSlotLaunch(ctx, 0, 0, sourceOf, settings)
	})
	if ok {
		t.Fatal("a slot cancelled during the URL warmup hold was allowed to launch")
	}
}

func TestAwaitSlotLaunchLaunchesWhenNothingIsPending(t *testing.T) {
	settings := reloadTestSettings("198.51.100.92:443", "alice", "pw")
	if !awaitSlotLaunch(context.Background(), 0, 0, map[string]string{}, settings) {
		t.Fatal("an undelayed slot with a live context must launch")
	}
}
