package miner

import (
	"context"
	"testing"
	"time"
)

// These tests cover the two properties the supervised admission dispatcher
// depends on:
//
//   - a panicking pass must give back the in-flight concurrency slot it
//     reserved (a leaked token per panic eats proxyAdmissionMaxConcurrency and
//     wedges every proxy auth), and must not leave the gate's mutex held, or
//     the restarted dispatcher blocks forever in waitForWaiter;
//   - the supervisor must actually restart the dispatcher after the panic, and
//     the restarted loop must still admit.

// poisonWaiter queues a waiter whose ready channel is already closed, so the
// pass that picks it panics in releaseWeightedRandom — after it has reserved a
// concurrency slot and while it holds the gate mutex. That is exactly the
// window a restart has to survive.
func poisonWaiter(g *proxyAdmissionGate) {
	w := &admissionWaiter{weight: 1, ready: make(chan struct{})}
	close(w.ready)
	g.push(w)
}

// TestAdmissionDispatchPanicReleasesSlotAndLock is the deterministic, direct
// form: one pass panics with a slot reserved and the lock held, and both must
// be free afterwards.
func TestAdmissionDispatchPanicReleasesSlotAndLock(t *testing.T) {
	g := &proxyAdmissionGate{
		limiter:     newAuthRateLimiter(1000, 1000, 1000),
		wake:        make(chan struct{}, 1),
		concurrency: make(chan struct{}, proxyAdmissionMaxConcurrency),
	}
	poisonWaiter(g)

	// runSafe stands in for the supervisor's recovery: the panic must unwind
	// through dispatchOnce's defer before it is recovered.
	runSafe("test-admission-panic", func() { g.dispatchOnce() })

	if held := len(g.concurrency); held != 0 {
		t.Fatalf("a panicking pass leaked %d concurrency slot(s); with %d total, repeated panics would wedge every proxy auth",
			held, proxyAdmissionMaxConcurrency)
	}
	if !g.mu.TryLock() {
		t.Fatal("the gate mutex is still held after a panicking pass; the restarted dispatcher would block forever in waitForWaiter")
	}
	g.mu.Unlock()
}

// TestAdmissionDispatchRestartsAndKeepsAdmitting is the supervised path: the
// dispatcher panics once, the supervisor restarts it, the reserved slot is free
// and the new loop admits a caller.
func TestAdmissionDispatchRestartsAndKeepsAdmitting(t *testing.T) {
	const name = "test-miner-admission-restart"
	resetLoopStatus(name)
	origSleep := loopSleep
	loopSleep = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }
	defer func() { loopSleep = origSleep }()

	g := &proxyAdmissionGate{
		limiter:     newAuthRateLimiter(1000, 1000, 1000),
		wake:        make(chan struct{}, 1),
		concurrency: make(chan struct{}, proxyAdmissionMaxConcurrency),
	}
	poisonWaiter(g)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// The gate has no shutdown of its own — the global one is built at init
		// and lives for the process — so the dispatcher blocks on its waiter
		// list rather than observing ctx. The test does not join it for the
		// same reason production does not: there is nothing to end, and after
		// the assertions the loop is parked on an empty waiter list.
		superviseLoop(ctx, newLoopRef(name), g.dispatchLoop, nil)
	}()
	defer cancel()

	deadline := time.Now().Add(5 * time.Second)
	for loopStatusOf(name).Restarts < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the supervisor never restarted the panicking dispatcher")
		}
		time.Sleep(time.Millisecond)
	}

	// The restarted loop must find the slot the panicking pass held free.
	if held := len(g.concurrency); held != 0 {
		t.Fatalf("the restarted dispatcher starts with %d/%d concurrency slots already held", held, proxyAdmissionMaxConcurrency)
	}

	admitCtx, admitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer admitCancel()
	release, err := g.Admit(admitCtx, 0)
	if err != nil {
		t.Fatalf("the restarted dispatcher did not admit a caller: %v", err)
	}
	release()
	if held := len(g.concurrency); held != 0 {
		t.Fatalf("after the released admit, %d concurrency slot(s) are still held", held)
	}
}
