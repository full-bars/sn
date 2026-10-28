package miner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// joinTestPool drives a reloader whose workers finish only when the test lets
// them, so the order of "old worker returned" and "replacement started" is
// observable.
type joinTestPool struct {
	mu      sync.Mutex
	desired []*connect.ProxySettings
	done    []chan struct{}
	spawned []string
}

func (p *joinTestPool) reloader() *minerProxyReloader {
	r := newMinerProxyReloader(
		func() ([]*connect.ProxySettings, int, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			return append([]*connect.ProxySettings(nil), p.desired...), 0, nil
		},
		nil,
	)
	r.spawnGeneration = func(settings *connect.ProxySettings, gen uint64) (context.CancelFunc, <-chan struct{}, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		done := make(chan struct{})
		p.done = append(p.done, done)
		p.spawned = append(p.spawned, settings.Auth.Password)
		return func() {}, done, nil
	}
	return r
}

func (p *joinTestPool) spawnedPasswords() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.spawned...)
}

// Rotation cancels the old worker, but the replacement for the same identity
// must not start until that worker has returned from its teardown.
func TestReloadRotationWaitsForOldWorkerToFinish(t *testing.T) {
	p := &joinTestPool{desired: []*connect.ProxySettings{reloadTestSettings("198.51.100.50:443", "u", "old")}}
	r := p.reloader()
	if _, err := r.reload(); err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	p.desired = []*connect.ProxySettings{reloadTestSettings("198.51.100.50:443", "u", "new")}
	oldDone := p.done[0]
	p.mu.Unlock()

	finished := make(chan error, 1)
	go func() {
		_, err := r.reload()
		finished <- err
	}()

	select {
	case <-finished:
		t.Fatal("rotation returned while the old worker was still running")
	case <-time.After(100 * time.Millisecond):
	}
	if got := p.spawnedPasswords(); len(got) != 1 {
		t.Fatalf("replacement started before the old worker returned: %v", got)
	}

	close(oldDone)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if got := p.spawnedPasswords(); len(got) != 2 || got[1] != "new" {
		t.Fatalf("replacement did not start after the old worker returned: %v", got)
	}
}

// A worker that never returns leaves the identity down, reported as lost,
// instead of running it twice.
func TestReloadRotationDoesNotStartReplacementOverStuckWorker(t *testing.T) {
	p := &joinTestPool{desired: []*connect.ProxySettings{reloadTestSettings("198.51.100.51:443", "u", "old")}}
	r := p.reloader()
	r.retireTimeout = 20 * time.Millisecond
	if _, err := r.reload(); err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	p.desired = []*connect.ProxySettings{reloadTestSettings("198.51.100.51:443", "u", "new")}
	oldDone := p.done[0]
	p.mu.Unlock()

	summary, err := r.reload()
	if err == nil || summary.Lost != 1 {
		t.Fatalf("stuck worker must fail the rotation as lost: summary=%+v err=%v", summary, err)
	}
	if got := p.spawnedPasswords(); len(got) != 1 {
		t.Fatalf("replacement started over a worker that had not returned: %v", got)
	}

	// Once the worker is gone the next reload brings the identity back.
	close(oldDone)
	if _, err := r.reload(); err != nil {
		t.Fatal(err)
	}
	if got := p.spawnedPasswords(); len(got) != 2 || got[1] != "new" {
		t.Fatalf("identity was not restored after the worker returned: %v", got)
	}
}
