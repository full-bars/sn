package miner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// proxy_measurement_sweep_run_test.go drives the sweep with explicit ticks and a
// fake probe, so no case sleeps or touches a network. The dispatch itself is
// asynchronous, so each case waits on the probe's own signal and uses a
// generous timeout only as a failure guard.

// sweepRecorder is the fake probe of the sweep cases: it reports each identity
// it is called for and answers with ok.
type sweepRecorder struct {
	mu     sync.Mutex
	called []string
	signal chan string
}

func newSweepRecorder() *sweepRecorder {
	return &sweepRecorder{signal: make(chan string, 32)}
}

func (self *sweepRecorder) probe(_ context.Context, settings *connect.ProxySettings) proxyUDPResult {
	key := settings.Key()
	self.mu.Lock()
	self.called = append(self.called, key)
	self.mu.Unlock()
	self.signal <- key
	return proxyUDPResult{Status: proxyUDPOK, TCPExit: "198.51.100.9", UDPExit: "198.51.100.9"}
}

func (self *sweepRecorder) count() int {
	self.mu.Lock()
	defer self.mu.Unlock()
	return len(self.called)
}

func sweepTestSettings(addresses ...string) []*connect.ProxySettings {
	settings := make([]*connect.ProxySettings, 0, len(addresses))
	for _, address := range addresses {
		settings = append(settings, measurementTestSettings(address))
	}
	return settings
}

// waitForCalled waits until the probe has answered n times in total.
func waitForCalled(t *testing.T, recorder *sweepRecorder, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for recorder.count() < n {
		select {
		case <-recorder.signal:
		case <-deadline:
			t.Fatalf("probe answered %d times, want %d", recorder.count(), n)
		}
	}
}

func TestMeasurementSweepChecksOnlyDueIdentities(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	recorder := newSweepRecorder()
	store := newProxyUDPCheckStore(recorder.probe)
	warm := measurementTestSettings("192.0.2.20:1080")
	cold := measurementTestSettings("192.0.2.21:1080")
	// the warm identity already published a verdict
	store.Check(context.Background(), warm, now)
	waitForCalled(t, recorder, 1)

	started := store.sweepOnce(context.Background(), now, []*connect.ProxySettings{warm, cold}, 4)
	if started != 1 {
		t.Fatalf("started %d checks, want 1", started)
	}
	waitForCalled(t, recorder, 2)
	recorder.mu.Lock()
	called := append([]string(nil), recorder.called...)
	recorder.mu.Unlock()
	if len(called) != 2 || called[1] != cold.Key() {
		t.Fatalf("sweep checked the wrong identities: %d calls, second %s", len(called), called[1])
	}
}

func TestMeasurementSweepRotatesThroughThePool(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	recorder := newSweepRecorder()
	store := newProxyUDPCheckStore(recorder.probe)
	settings := sweepTestSettings(
		"192.0.2.30:1080", "192.0.2.31:1080", "192.0.2.32:1080",
		"192.0.2.33:1080", "192.0.2.34:1080", "192.0.2.35:1080",
	)
	// three ticks of two: each tick takes the two lowest keys the sweep has not
	// tried yet, so every identity is reached before any is repeated
	for tick := 0; tick < 3; tick++ {
		started := store.sweepOnce(context.Background(), now, settings, 2)
		if started != 2 {
			t.Fatalf("tick %d started %d checks, want 2", tick, started)
		}
		waitForCalled(t, recorder, (tick+1)*2)
	}
	waitForCalled(t, recorder, len(settings))
	recorder.mu.Lock()
	called := append([]string(nil), recorder.called...)
	recorder.mu.Unlock()
	seen := map[string]bool{}
	for _, key := range called {
		if seen[key] {
			t.Fatalf("an identity was checked twice before the pool was covered: %v", called)
		}
		seen[key] = true
	}
	if len(seen) != len(settings) {
		t.Fatalf("covered %d identities, want %d", len(seen), len(settings))
	}
}

func TestMeasurementSweepLeavesACancelledCheckBehind(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	recorder := newSweepRecorder()
	store := newProxyUDPCheckStore(recorder.probe)
	cancelled := measurementTestSettings("192.0.2.40:1080")
	first := measurementTestSettings("192.0.2.41:1080")
	second := measurementTestSettings("192.0.2.42:1080")

	// a dispatch whose context was already cancelled records its attempt and
	// publishes nothing
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store.Check(ctx, cancelled, now)
	if recorder.count() != 0 {
		t.Fatal("a cancelled dispatch reached the probe")
	}

	started := store.sweepOnce(context.Background(), now, []*connect.ProxySettings{cancelled, first, second}, 2)
	if started != 2 {
		t.Fatalf("started %d checks, want 2", started)
	}
	waitForCalled(t, recorder, 2)
	recorder.mu.Lock()
	called := append([]string(nil), recorder.called...)
	recorder.mu.Unlock()
	for _, key := range called {
		if key == cancelled.Key() {
			t.Fatal("the identity of a cancelled check took the batch")
		}
	}
}

func TestMeasurementSweepLoopStopsOnCancel(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	recorder := newSweepRecorder()
	store := newProxyUDPCheckStore(recorder.probe)
	settings := sweepTestSettings("192.0.2.50:1080")

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		store.sweepLoop(ctx, ticks, func() []*connect.ProxySettings { return settings }, 1)
	}()

	// an unbuffered send means the loop took the tick
	select {
	case ticks <- now:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep loop did not take a tick")
	}
	waitForCalled(t, recorder, 1)

	cancel()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep loop did not stop")
	}
}
