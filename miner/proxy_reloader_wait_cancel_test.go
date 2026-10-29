package miner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// stuckRotationReloader runs alice at address and then asks for bob at the same
// address while alice's worker never returns. The reload therefore blocks in
// its retirement wait; blocked closes once alice has been cancelled.
func stuckRotationReloader(t *testing.T, address string, timeout time.Duration) (r *minerProxyReloader, blocked <-chan struct{}, spawned *int) {
	t.Helper()
	alice := reloadTestSettings(address, "alice", "pw")
	bob := reloadTestSettings(address, "bob", "pw")
	desired := []*connect.ProxySettings{bob}
	r = newMinerProxyReloader(func() ([]*connect.ProxySettings, int, error) { return desired, 0, nil }, nil)
	r.retireTimeout = timeout
	count := 0
	r.spawnGeneration = func(*connect.ProxySettings, uint64) (context.CancelFunc, <-chan struct{}, error) {
		count++
		return func() {}, nil, nil
	}
	cancelled := make(chan struct{})
	r.seedWorker(alice, func() { close(cancelled) }, make(chan struct{}))
	return r, cancelled, &count
}

func TestReloadWaitEndsWithTheCallersContext(t *testing.T) {
	r, blocked, spawned := stuckRotationReloader(t, "198.51.100.80:443", time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { _, err := r.reloadContext(ctx); finished <- err }()
	<-blocked
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("reload error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reload kept waiting for the stuck worker after its context was cancelled")
	}
	if *spawned != 0 {
		t.Fatalf("started %d workers after cancellation", *spawned)
	}
	assertReloaderUnlocked(t, r)
}

func TestStopEndsAReloadWaitingOnARetiringWorker(t *testing.T) {
	r, blocked, _ := stuckRotationReloader(t, "198.51.100.81:443", time.Hour)
	finished := make(chan error, 1)
	go func() { _, err := r.reload(); finished <- err }()
	<-blocked
	stopped := make(chan struct{})
	go func() { r.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop() waited out the reload's retirement wait")
	}
	if err := <-finished; !errors.Is(err, errProxyReloaderStopped) {
		t.Fatalf("reload error = %v, want errProxyReloaderStopped", err)
	}
}

// Several stuck retirements share one deadline instead of one timeout each.
func TestReloadRetirementWaitsShareOneDeadline(t *testing.T) {
	const timeout = 200 * time.Millisecond
	var desired []*connect.ProxySettings
	r := newMinerProxyReloader(func() ([]*connect.ProxySettings, int, error) { return desired, 0, nil }, nil)
	r.retireTimeout = timeout
	r.spawnGeneration = func(*connect.ProxySettings, uint64) (context.CancelFunc, <-chan struct{}, error) {
		return func() {}, nil, nil
	}
	for _, address := range []string{"198.51.100.82:443", "198.51.100.83:443", "198.51.100.84:443", "198.51.100.85:443"} {
		r.seedWorker(reloadTestSettings(address, "alice", "pw"), func() {}, make(chan struct{}))
		desired = append(desired, reloadTestSettings(address, "bob", "pw"))
	}
	began := time.Now()
	_, err := r.reload()
	elapsed := time.Since(began)
	if err == nil {
		t.Fatal("expected the stuck retirements to fail the reload")
	}
	if elapsed > 2*timeout {
		t.Fatalf("reload took %s for 4 stuck slots, want about one timeout (%s)", elapsed, timeout)
	}
}

func assertReloaderUnlocked(t *testing.T, r *minerProxyReloader) {
	t.Helper()
	unlocked := make(chan struct{})
	go func() { r.quiesce(); close(unlocked) }()
	select {
	case <-unlocked:
	case <-time.After(5 * time.Second):
		t.Fatal("reloader mutex is still held")
	}
}
