package miner

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// resetLoopStatus forgets a loop's recorded status so a test that asserts exact
// restart counts also passes under -count=N, where the registry persists.
func resetLoopStatus(name string) {
	loopStatusMu.Lock()
	defer loopStatusMu.Unlock()
	delete(loopStatuses, name)
}

func TestNextLoopBackoff(t *testing.T) {
	cases := []struct {
		name         string
		prev, ranFor time.Duration
		want         time.Duration
	}{
		{"first failure", 0, time.Second, loopBackoffBase},
		{"doubles", 2 * time.Second, time.Second, 4 * time.Second},
		{"caps", loopBackoffMax, time.Second, loopBackoffMax},
		{"healthy run resets", loopBackoffMax, loopHealthyRun, loopBackoffBase},
	}
	for _, tc := range cases {
		if got := nextLoopBackoff(tc.prev, tc.ranFor); got != tc.want {
			t.Errorf("%s: nextLoopBackoff(%v, %v) = %v, want %v", tc.name, tc.prev, tc.ranFor, got, tc.want)
		}
	}
}

// A panicking loop must be contained, restarted, counted, and backed off; onFail
// must run after every failure so a loop's actuators can fail neutral.
func TestSuperviseLoopRestartsAfterPanic(t *testing.T) {
	const name = "test-miner-panic"
	resetLoopStatus(name)
	origSleep := loopSleep
	defer func() { loopSleep = origSleep }()
	var sleeps []time.Duration
	loopSleep = func(ctx context.Context, d time.Duration) bool {
		sleeps = append(sleeps, d)
		return ctx.Err() == nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	var runs, fails atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The registry entry is resolved on the caller's side, as every real
		// caller does (loop_supervisor.go).
		ref := newLoopRef(name)
		superviseLoop(ctx, ref, func() {
			if runs.Add(1) >= 4 {
				cancel() // stop after the 4th run
				return
			}
			panic("boom")
		}, func() { fails.Add(1) })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not return after ctx cancel")
	}

	if got := runs.Load(); got != 4 {
		t.Fatalf("runs = %d, want 4 (3 panics then a clean stop)", got)
	}
	if got := fails.Load(); got != 3 {
		t.Fatalf("onFail calls = %d, want 3", got)
	}
	want := []time.Duration{loopBackoffBase, 2 * loopBackoffBase, 4 * loopBackoffBase}
	if len(sleeps) != len(want) {
		t.Fatalf("backoff sleeps = %v, want %v", sleeps, want)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Fatalf("backoff sleeps = %v, want %v", sleeps, want)
		}
	}
	if st := loopStatusOf(name); st.Restarts != 3 || st.Up {
		t.Fatalf("status = %+v, want 3 restarts and not up after stop", st)
	}
}

// A loop that keeps panicking must wait between runs: the supervisor backs off
// instead of spinning. Every run after the first is preceded by exactly one
// sleep, and the sleeps grow until the cap.
func TestSuperviseLoopBacksOffOnRepeatedPanic(t *testing.T) {
	const name = "test-miner-backoff"
	resetLoopStatus(name)
	origSleep := loopSleep
	defer func() { loopSleep = origSleep }()
	var sleeps []time.Duration
	loopSleep = func(ctx context.Context, d time.Duration) bool {
		sleeps = append(sleeps, d)
		return len(sleeps) < 6 // stop pretending to wait after the 6th
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	superviseLoop(ctx, newLoopRef(name), func() { panic("boom") }, nil)

	want := []time.Duration{
		loopBackoffBase,
		2 * loopBackoffBase,
		4 * loopBackoffBase,
		8 * loopBackoffBase,
		16 * loopBackoffBase,
		32 * loopBackoffBase,
	}
	if len(sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Fatalf("sleeps = %v, want %v (must back off, not spin)", sleeps, want)
		}
	}
	// Five runs were actually restarted; the sixth backoff ended the supervisor
	// before another run started, and a restart that never happened is not
	// counted.
	if st := loopStatusOf(name); st.Restarts != 5 || st.Up {
		t.Fatalf("status = %+v, want 5 restarts and not up", st)
	}
}

// A normal long-running loop is not restarted: it runs once, and cancelling the
// context is a clean stop.
func TestSuperviseLoopNormalLoopNotRestarted(t *testing.T) {
	const name = "test-miner-normal"
	resetLoopStatus(name)
	origSleep := loopSleep
	defer func() { loopSleep = origSleep }()

	ctx, cancel := context.WithCancel(context.Background())
	var runs atomic.Int32
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseLoop(ctx, newLoopRef(name), func() {
			if runs.Add(1) == 1 {
				close(started)
			}
			<-ctx.Done() // a well-behaved loop runs until cancellation
		}, nil)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not return after ctx cancel")
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("runs = %d, want 1 (a normal loop is not restarted)", got)
	}
	if st := loopStatusOf(name); st.Restarts != 0 || st.Up {
		t.Fatalf("status = %+v, want 0 restarts and not up", st)
	}
}

// A shutdown during the backoff must stop the supervisor without counting a
// restart that never happened, and without waiting out the backoff.
func TestSuperviseLoopStopsOnContextCancel(t *testing.T) {
	const name = "test-miner-cancel-backoff"
	resetLoopStatus(name)
	origSleep := loopSleep
	defer func() { loopSleep = origSleep }()
	ctx, cancel := context.WithCancel(context.Background())
	loopSleep = func(ctx context.Context, d time.Duration) bool { cancel(); return false }

	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseLoop(ctx, newLoopRef(name), func() { panic("boom") }, nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not return when cancelled during the backoff")
	}
	if st := loopStatusOf(name); st.Restarts != 0 || st.Up {
		t.Fatalf("status = %+v, want 0 restarts and not up", st)
	}
}

// A cancel that lands after the backoff but before the next run is a shutdown,
// not a run: fn must not be invoked on a dead context, and the restart must not
// be counted.
func TestSuperviseLoopDoesNotStartOnCancelledContext(t *testing.T) {
	const name = "test-miner-cancelled-entry"
	resetLoopStatus(name)
	origSleep := loopSleep
	defer func() { loopSleep = origSleep }()
	// A zero sleep keeps the test off the wall clock; the entry check must
	// return before fn or the sleep is reached at all.
	loopSleep = func(ctx context.Context, d time.Duration) bool { return true }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var runs atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseLoop(ctx, newLoopRef(name), func() { runs.Add(1) }, nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not return on an already-cancelled context")
	}
	if got := runs.Load(); got != 0 {
		t.Fatalf("runs = %d, want 0: fn must not start on a cancelled context", got)
	}
	if st := loopStatusOf(name); st.Restarts != 0 || st.Up {
		t.Fatalf("status = %+v, want 0 restarts and not up", st)
	}
}

// A loop that returns while the context is live is a failure too, not a clean
// exit: without this a silently ended loop is never noticed.
func TestSuperviseLoopRestartsAfterUnexpectedReturn(t *testing.T) {
	const name = "test-miner-return"
	resetLoopStatus(name)
	origSleep := loopSleep
	defer func() { loopSleep = origSleep }()
	loopSleep = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }

	ctx, cancel := context.WithCancel(context.Background())
	var runs atomic.Int32
	superviseLoop(ctx, newLoopRef(name), func() {
		if runs.Add(1) >= 2 {
			cancel()
		}
	}, nil)
	if got := runs.Load(); got != 2 {
		t.Fatalf("runs = %d, want 2", got)
	}
	if st := loopStatusOf(name); st.Restarts != 1 {
		t.Fatalf("restarts = %d, want 1", st.Restarts)
	}
}

// The miner owns the supervised-loop registry: the seam must be filled, and the
// scraped families must carry one series per registered loop.
func TestMinerLoopSupervisorMetricsSeam(t *testing.T) {
	if metricsSeamLoops == nil {
		t.Fatal("metricsSeamLoops is nil: the supervisor did not claim the seam")
	}
	resetLoopStatus("test-miner-metrics")
	loopStatusOf("test-miner-metrics") // register
	setLoopUp("test-miner-metrics", true, true)

	var b strings.Builder
	writeSeamLoops(&b)
	out := b.String()
	// The real scrape builder must carry the same series: a loop registered by
	// the supervisor is scrape-visible without any further wiring.
	scrape := minerExtraMetrics()
	for _, want := range []string{
		"# TYPE urnet_loop_restarts_total counter",
		"# TYPE urnet_loop_up gauge",
		`urnet_loop_restarts_total{loop="test-miner-metrics"} 1`,
		`urnet_loop_up{loop="test-miner-metrics"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q in:\n%s", want, out)
		}
		if !strings.Contains(scrape, want) {
			t.Errorf("scrape missing %q", want)
		}
	}
	setLoopUp("test-miner-metrics", false, false)
}

// The manager's wait group still joins a supervised loop: a panic restarts it
// in place, and cancelling stops it and releases the join.
func TestMinerLoopSupervisorStartLoopJoins(t *testing.T) {
	const name = "test-miner-start-loop"
	resetLoopStatus(name)
	t.Cleanup(func() { resetLoopStatus(name) })
	origSleep := loopSleep
	defer func() { loopSleep = origSleep }()
	loopSleep = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }

	m := &proxyManager{}
	ctx, cancel := context.WithCancel(context.Background())
	var runs atomic.Int32
	first := make(chan struct{}, 1)
	second := make(chan struct{}, 1)
	m.startLoop(ctx, name, func() {
		if runs.Add(1) == 1 {
			first <- struct{}{}
			panic("boom")
		}
		second <- struct{}{}
		<-ctx.Done()
	})
	<-first
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("supervised loop was not restarted after the panic")
	}
	cancel()
	joined := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("stop path did not join the supervised loop")
	}
	if got := runs.Load(); got != 2 {
		t.Fatalf("runs = %d, want 2 (one panic, one restarted run)", got)
	}
	if st := loopStatusOf(name); st.Restarts != 1 {
		t.Fatalf("restarts = %d, want 1", st.Restarts)
	}
}
