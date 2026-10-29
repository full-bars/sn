package miner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// stuckRotation is a reloader whose current reload holds mu inside a
// retirement wait: alice was rotated to bob and alice's worker never returns.
// The retirement timeout is long enough that only a cancellation or a stop can
// end the wait inside a test.
type stuckRotation struct {
	r       *minerProxyReloader
	mu      sync.Mutex
	spawned []string
	first   chan error
}

func (s *stuckRotation) spawnedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.spawned...)
}

func newStuckRotation(t *testing.T, firstCtx context.Context) *stuckRotation {
	t.Helper()
	alice := reloadTestSettings("198.51.100.60:443", "alice", "pw")
	bob := reloadTestSettings("198.51.100.60:443", "bob", "pw")
	s := &stuckRotation{first: make(chan error, 1)}
	var desiredMu sync.Mutex
	desired := []*connect.ProxySettings{alice}
	cancelled := make(chan struct{})
	never := make(chan struct{})
	s.r = newMinerProxyReloader(func() ([]*connect.ProxySettings, int, error) {
		desiredMu.Lock()
		defer desiredMu.Unlock()
		return desired, 0, nil
	}, nil)
	s.r.retireTimeout = time.Minute
	s.r.spawnGeneration = func(settings *connect.ProxySettings, _ uint64) (context.CancelFunc, <-chan struct{}, error) {
		s.mu.Lock()
		s.spawned = append(s.spawned, settings.Auth.User)
		s.mu.Unlock()
		if settings.Auth.User == "alice" {
			return func() { close(cancelled) }, never, nil
		}
		return func() {}, nil, nil
	}
	if _, err := s.r.reload(); err != nil {
		t.Fatal(err)
	}
	desiredMu.Lock()
	desired = []*connect.ProxySettings{bob}
	desiredMu.Unlock()
	go func() {
		_, err := s.r.reloadContext(firstCtx)
		s.first <- err
	}()
	// Alice is cancelled under mu, right before the reload starts waiting for
	// her worker.
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the rotation never retired alice")
	}
	t.Cleanup(s.r.stop)
	return s
}

func requireReturns(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not end after the run was cancelled", what)
	}
}

// The audit's unpark reloads under the run's context: cancelling the run ends
// its wait behind a stuck retirement, and nothing is started.
func TestAuditUnparkReloadEndsWithTheRun(t *testing.T) {
	t.Cleanup(resetReapedProxyHoldsForTest)
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	s := newStuckRotation(t, runCtx)
	s.r.bindRunContext(runCtx)
	currentMinerProxyReloader.Store(s.r)
	t.Cleanup(func() { currentMinerProxyReloader.CompareAndSwap(s.r, nil) })
	setReapedProxyHold("held:1", time.Now().Add(time.Hour))

	unparked := make(chan struct{})
	go func() { liveProxyAuditUnpark([]string{"held:1"}); close(unparked) }()
	cancelRun()
	requireReturns(t, "the audit unpark", func() { <-unparked })
	requireReturns(t, "the stuck reload", func() { <-s.first })
	if got := s.spawnedKeys(); len(got) != 1 {
		t.Fatalf("spawned %v, want only the initial alice", got)
	}
}

// The reaper's release reload runs under the run's context too.
func TestDegradedReaperReloadEndsWithTheRun(t *testing.T) {
	t.Cleanup(resetReapedProxyHoldsForTest)
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	s := newStuckRotation(t, runCtx)
	s.r.bindRunContext(runCtx)
	setReapedProxyHold("held:2", time.Now().Add(-time.Second))
	reaper := newMinerDegradedReaper(s.r)

	cycled := make(chan struct{})
	go func() { reaper.cycle(); close(cycled) }()
	cancelRun()
	requireReturns(t, "the reaper cycle", func() { <-cycled })
	requireReturns(t, "the stuck reload", func() { <-s.first })
	if got := s.spawnedKeys(); len(got) != 1 {
		t.Fatalf("spawned %v, want only the initial alice", got)
	}
}

// A reload only waiting its turn behind a stuck one can be abandoned: its
// context ends the wait instead of the mutex holding it for the whole timeout.
func TestReloadWaitingForItsTurnHonorsCancellation(t *testing.T) {
	s := newStuckRotation(t, context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var err error
	requireReturns(t, "the queued reload", func() { _, err = s.r.reloadContext(ctx) })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("queued reload error = %v, want context.Canceled", err)
	}
}

// The run signals stop before it waits for quiescence, so a reload that is
// stuck inside a retirement wait lets go instead of holding shutdown for the
// retirement timeout.
func TestRunQuiesceStopsTheReloaderWhenTheRunIsEnding(t *testing.T) {
	s := newStuckRotation(t, context.Background())
	ended, end := context.WithCancel(context.Background())
	end()
	requireReturns(t, "the run's quiesce", func() { quiesceForRun(ended, s.r) })
	requireReturns(t, "the stuck reload", func() { <-s.first })
}

// While the run is live a worker returning is not the end: the reload that may
// be starting its replacement is waited for, not stopped.
func TestRunQuiesceLeavesTheReloaderRunningWhileTheRunIsLive(t *testing.T) {
	r := newMinerProxyReloader(func() ([]*connect.ProxySettings, int, error) { return nil, 0, nil }, nil)
	quiesceForRun(context.Background(), r)
	if _, err := r.reload(); err != nil {
		t.Fatalf("reloader was stopped by a live run's quiesce: %v", err)
	}
}
