package miner

import (
	"context"
	"testing"
	"time"
)

// urlWarmupHold is true only while warmup is incomplete and the grace has
// not elapsed; a completed warmup or the grace releases it either way.
func TestURLWarmupHold(t *testing.T) {
	oldStart := processStart
	t.Cleanup(func() { processStart = oldStart })

	cases := []struct {
		name  string
		done  bool
		start time.Time
		hold  bool
	}{
		{"warmup done releases", true, time.Now(), false},
		{"inside the grace holds", false, time.Now(), true},
		{"just inside the grace holds", false, time.Now().Add(-(urlWarmupLaunchGrace - time.Second)), true},
		{"past the grace releases", false, time.Now().Add(-urlWarmupLaunchGrace - time.Second), false},
		{"done with an old start releases", true, time.Now().Add(-2 * urlWarmupLaunchGrace), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proxyWarmupDone.Store(c.done)
			t.Cleanup(func() { proxyWarmupDone.Store(false) })
			processStart = c.start
			if got := urlWarmupHold(); got != c.hold {
				t.Fatalf("urlWarmupHold() = %v, want %v", got, c.hold)
			}
		})
	}
}

// waitURLWarmupHold blocks while the hold stands, returns false when ctx
// ends first, and returns immediately once the grace has elapsed even though
// warmup is still incomplete.
func TestWaitURLWarmupHold(t *testing.T) {
	oldStart := processStart
	t.Cleanup(func() { processStart = oldStart })
	proxyWarmupDone.Store(false)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	// Grace elapsed: the wait releases without blocking.
	processStart = time.Now().Add(-urlWarmupLaunchGrace - time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !waitURLWarmupHold(ctx) {
		t.Fatal("an elapsed grace must release the wait")
	}

	// Hold standing: the wait blocks until the context ends.
	processStart = time.Now()
	ctx2, cancel2 := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- waitURLWarmupHold(ctx2) }()
	select {
	case <-done:
		t.Fatal("wait returned while the hold still stands")
	case <-time.After(200 * time.Millisecond):
	}
	cancel2()
	if <-done {
		t.Fatal("a cancelled wait must report false")
	}

	// Warmup completing mid-wait releases it.
	processStart = time.Now()
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	go func() {
		time.Sleep(100 * time.Millisecond)
		proxyWarmupDone.Store(true)
	}()
	if !waitURLWarmupHold(ctx3) {
		t.Fatal("warmup completion must release the wait")
	}
}

// The curated ramp signal: zero curated workers releases at once, and the
// hold releases only when every curated worker has launched.
func TestCuratedWarmupSignal(t *testing.T) {
	proxyWarmupDone.Store(false)
	warmupCuratedTotal.Store(0)
	warmupCuratedLaunched.Store(0)
	t.Cleanup(func() {
		proxyWarmupDone.Store(false)
		warmupCuratedTotal.Store(0)
		warmupCuratedLaunched.Store(0)
	})

	noteCuratedWarmupTotal(0)
	if !proxyWarmupDone.Load() {
		t.Fatal("a run with no curated workers must release warmup at once")
	}

	proxyWarmupDone.Store(false)
	warmupCuratedLaunched.Store(0)
	noteCuratedWarmupTotal(2)
	noteCuratedWarmupLaunch()
	if proxyWarmupDone.Load() {
		t.Fatal("one of two curated launches must not complete the ramp")
	}
	noteCuratedWarmupLaunch()
	if !proxyWarmupDone.Load() {
		t.Fatal("the last curated launch must complete the ramp")
	}
}

// shouldDeferURLWarmupLaunch holds only unproven URL-sourced identities
// while the hold stands: curated identities and promoted earners launch now,
// and a hold past grace defers nothing.
func TestShouldDeferURLWarmupLaunch(t *testing.T) {
	oldStart := processStart
	t.Cleanup(func() { processStart = oldStart })
	proxyWarmupDone.Store(false)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })
	now := time.Now()
	key := "10.0.0.1:1080"

	// Inside the grace, warmup incomplete.
	processStart = now
	if shouldDeferURLWarmupLaunch("file", key, now) {
		t.Fatal("a curated identity must never be deferred")
	}
	if !shouldDeferURLWarmupLaunch("url", key, now) {
		t.Fatal("an unproven URL identity must defer while the hold stands")
	}

	// Past the grace nothing defers.
	processStart = now.Add(-urlWarmupLaunchGrace - time.Second)
	if shouldDeferURLWarmupLaunch("url", key, now) {
		t.Fatal("an elapsed grace must release the launch")
	}

	// A promoted earner launches with the curated list regardless.
	processStart = now
	prevStore := globalProxyEarningsStore
	globalProxyEarningsStore = newProxyEarningsStore(proxyEarningsPath())
	t.Cleanup(func() { globalProxyEarningsStore = prevStore })
	creditEarningsAt(globalProxyEarningsStore, key, earningsPromotionBytes*2, now)
	if shouldDeferURLWarmupLaunch("url", key, now) {
		t.Fatal("a promoted earner must not defer")
	}
}
