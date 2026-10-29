package miner

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/connect"
)

// The retiring worker finishes in the same step as the caller's cancellation:
// stopping the old slot is what closes its done channel and cancels the
// context. The wait then has nothing to wait for, so only a recheck before the
// spawn keeps the replacement from starting after the caller gave up.
func TestReloadDoesNotSpawnAfterCancellationWhenTheWaitCompletes(t *testing.T) {
	const address = "198.51.100.90:443"
	// A password change at the same account is a rotation: the old slot is
	// stopped inside the per-identity loop, after its interruption check.
	alice := reloadTestSettings(address, "alice", "pw")
	bob := reloadTestSettings(address, "alice", "pw2")
	r := newMinerProxyReloader(func() ([]*connect.ProxySettings, int, error) {
		return []*connect.ProxySettings{bob}, 0, nil
	}, nil)
	spawned := 0
	r.spawnGeneration = func(*connect.ProxySettings, uint64) (context.CancelFunc, <-chan struct{}, error) {
		spawned++
		return func() {}, nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	r.seedWorker(alice, func() {
		cancel()
		close(done)
	}, done)

	_, err := r.reloadContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("reload error = %v, want context.Canceled", err)
	}
	if spawned != 0 {
		t.Fatalf("started %d workers after the context was cancelled", spawned)
	}
	assertReloaderUnlocked(t, r)
}
