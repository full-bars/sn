package miner

// Tests for the liveness port (provider/liveness_watchdog.go): the ping is
// observable, it is withheld only when the gate allows action, and the startup
// grace covers a slow launch. No test waits on the wall clock.

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestWatchdogPingIsObservableAndGated drives the feed loop one tick at a time
// and pins every transition: freshest tick pings; stale with the gate closed
// keeps pinging (and says what it would have done); stale with the gate open
// withholds the ping, asks for the episode only once, and runs the undo when
// progress resumes.
func TestWatchdogPingIsObservableAndGated(t *testing.T) {
	now := swapThrashT0
	progress := newLivenessProgress(func() time.Time { return now })
	progress.tick()

	var pings atomic.Int64
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, formatLogLine(format, args...)) }

	var gateOpen atomic.Bool
	var episodeStarts []bool
	gate := func(episodeStart bool) (bool, string) {
		episodeStarts = append(episodeStarts, episodeStart)
		if gateOpen.Load() {
			return true, ""
		}
		return false, "self-heal is off"
	}
	undoRan := make(chan struct{}, 1)
	onStall := func() func() { return func() { undoRan <- struct{}{} } }

	tick := make(chan time.Time, 1)
	idle := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runSdWatchdogLoop(ctx, tick, progress, livenessStaleAfter,
		func() error { pings.Add(1); return nil }, logf, gate, onStall,
		func() { idle <- struct{}{} })

	waitIdle := func(what string) {
		t.Helper()
		select {
		case <-idle:
		case <-time.After(2 * time.Second):
			t.Fatalf("watchdog loop did not reach %s", what)
		}
	}
	step := func(at time.Time) {
		t.Helper()
		// The loop signals idle once per iteration, right before waiting: the
		// signal after a tick proves that tick was handled completely.
		now = at
		tick <- at
		waitIdle("the next wait")
	}
	// The loop signals its first wait before any tick; consume it so every
	// step below pairs one tick with exactly one idle signal.
	waitIdle("its first wait")

	// Fresh progress: every tick feeds systemd.
	step(swapThrashT0.Add(time.Second))
	if got := pings.Load(); got != 1 {
		t.Fatalf("a fresh process must ping, got %d pings", got)
	}

	// Stale, but self-heal is off: the ping keeps flowing and the line says why.
	stale := swapThrashT0.Add(time.Hour)
	progress.nano.Store(stale.Add(-2 * livenessStaleAfter).UnixNano())
	step(stale)
	if got := pings.Load(); got != 2 {
		t.Fatalf("a refused stall must keep feeding systemd, got %d pings", got)
	}
	if joined := strings.Join(logs, "\n"); !strings.Contains(joined, "still being fed") {
		t.Fatalf("the passive line must say the watchdog is still fed:\n%s", joined)
	}

	// Stale and the gate opens: the ping is withheld, once, and the stall
	// handler runs once.
	gateOpen.Store(true)
	step(stale.Add(time.Second))
	if got := pings.Load(); got != 2 {
		t.Fatalf("an actionable stall must withhold the ping, got %d pings", got)
	}
	if joined := strings.Join(logs, "\n"); !strings.Contains(joined, "withholding the systemd watchdog ping") {
		t.Fatalf("the acting line must say the ping is withheld:\n%s", joined)
	}
	if len(episodeStarts) == 0 || !episodeStarts[len(episodeStarts)-1] {
		t.Fatalf("the first stale tick of an episode must ask the gate with episodeStart=true: %v", episodeStarts)
	}

	// The next stale tick withholds again but does not re-ask the episode start
	// (the budget already includes this episode's entry). The first two asks
	// are both at episode start: the refused tick never started an episode, so
	// the tick that acts is still the episode's first.
	step(stale.Add(2 * time.Second))
	if got := pings.Load(); got != 2 {
		t.Fatalf("the ping must stay withheld while the stall lasts, got %d pings", got)
	}
	if want := []bool{true, true, false}; !slices.Equal(episodeStarts, want) {
		t.Fatalf("episode-start asks = %v, want %v (a refused tick never starts an episode)", episodeStarts, want)
	}

	// Progress resumes: pings resume and the stall's undo runs.
	progress.tick()
	now = stale.Add(3 * time.Second)
	step(now)
	if got := pings.Load(); got != 3 {
		t.Fatalf("a resumed process must ping again, got %d pings", got)
	}
	select {
	case <-undoRan:
	case <-time.After(2 * time.Second):
		t.Fatal("progress resuming before systemd acted must run the stall's undo")
	}
	if joined := strings.Join(logs, "\n"); !strings.Contains(joined, "being fed again") {
		t.Fatalf("the resume line must say the feed is back:\n%s", joined)
	}
}

// The startup grace: before the progress loop has ticked once, a process is
// judged against the grace, not the stall budget; after it, the strict budget
// applies.
func TestLivenessProgressStartupGrace(t *testing.T) {
	now := swapThrashT0
	progress := newLivenessProgress(func() time.Time { return now })

	// Starting up: a long startup is not a stall yet.
	now = swapThrashT0.Add(livenessStaleAfter + time.Minute)
	if !progress.fresh(livenessStaleAfter) {
		t.Fatal("an unticked process inside the startup grace is not stale")
	}
	now = swapThrashT0.Add(livenessStartupGrace + time.Minute)
	if progress.fresh(livenessStaleAfter) {
		t.Fatal("a startup longer than the grace is itself a stall")
	}

	// After the first tick the strict budget applies, with no grace.
	now = swapThrashT0.Add(2 * livenessStartupGrace)
	progress.tick()
	now = now.Add(livenessStaleAfter - time.Second)
	if !progress.fresh(livenessStaleAfter) {
		t.Fatal("a tick inside the stall budget is fresh")
	}
	now = now.Add(2 * time.Second)
	if progress.fresh(livenessStaleAfter) {
		t.Fatal("a tick past the stall budget is stale")
	}
}

// The interval is a third of systemd's WATCHDOG_USEC, never under a second,
// and absent when the unit does not enable the watchdog.
func TestLivenessWatchdogInterval(t *testing.T) {
	cases := []struct {
		usec string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"nonsense", 0, false},
		{"3000000", time.Second, true},          // 3s / 3 = 1s
		{"1200000000", 400 * time.Second, true}, // 20m / 3
		{"1000", time.Second, true},             // clamped to the one-second floor
	}
	for _, tc := range cases {
		got, ok := sdWatchdogInterval(func(string) string { return tc.usec })
		if ok != tc.ok || got != tc.want {
			t.Errorf("WATCHDOG_USEC=%q: got (%s, %v), want (%s, %v)", tc.usec, got, ok, tc.want, tc.ok)
		}
	}
}
