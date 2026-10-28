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
type joinTestWorker struct {
	done       chan struct{}
	cancelled  chan struct{}
	retireOnce sync.Once
	cancelOnce sync.Once
	address    string
}

func (w *joinTestWorker) retire() { w.retireOnce.Do(func() { close(w.done) }) }

type joinTestPool struct {
	mu      sync.Mutex
	desired []*connect.ProxySettings
	workers []*joinTestWorker
	spawned []string
	overlap bool
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
		for _, worker := range p.workers {
			if worker.address == settings.Address {
				select {
				case <-worker.done:
				default:
					p.overlap = true
				}
			}
		}
		worker := &joinTestWorker{done: make(chan struct{}), cancelled: make(chan struct{}), address: settings.Address}
		p.workers = append(p.workers, worker)
		p.spawned = append(p.spawned, settings.Auth.Password)
		return func() { worker.cancelOnce.Do(func() { close(worker.cancelled) }) }, worker.done, nil
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
	oldWorker := p.workers[0]
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

	oldWorker.retire()
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
	oldWorker := p.workers[0]
	p.mu.Unlock()

	summary, err := r.reload()
	if err == nil || summary.Lost != 1 {
		t.Fatalf("stuck worker must fail the rotation as lost: summary=%+v err=%v", summary, err)
	}
	if got := p.spawnedPasswords(); len(got) != 1 {
		t.Fatalf("replacement started over a worker that had not returned: %v", got)
	}

	// Once the worker is gone the next reload brings the identity back.
	oldWorker.retire()
	if _, err := r.reload(); err != nil {
		t.Fatal(err)
	}
	if got := p.spawnedPasswords(); len(got) != 2 || got[1] != "new" {
		t.Fatalf("identity was not restored after the worker returned: %v", got)
	}
}

// Changing the username at a live address removes one identity and adds
// another at the same host:port. The addition is refused while the old identity
// owns the address, so the removal has to come first or the pool is left empty
// until the next reload.
func TestReloadUsernameChangeAtLiveAddressSwapsInOneReload(t *testing.T) {
	p := &joinTestPool{desired: []*connect.ProxySettings{reloadTestSettings("198.51.100.52:443", "alice", "pw")}}
	r := p.reloader()
	reserving := r.spawnGeneration
	r.spawnGeneration = func(settings *connect.ProxySettings, gen uint64) (context.CancelFunc, <-chan struct{}, error) {
		if err := r.reserveAddress(settings); err != nil {
			return nil, nil, err
		}
		return reserving(settings, gen)
	}
	if _, err := r.reload(); err != nil {
		t.Fatal(err)
	}

	p.mu.Lock()
	p.desired = []*connect.ProxySettings{reloadTestSettings("198.51.100.52:443", "bob", "pw")}
	alice := p.workers[0]
	p.mu.Unlock()
	t.Cleanup(alice.retire)

	type reloadResult struct {
		summary minerReloadSummary
		err     error
	}
	finished := make(chan reloadResult, 1)
	go func() {
		summary, err := r.reload()
		finished <- reloadResult{summary, err}
	}()
	select {
	case <-alice.cancelled:
	case result := <-finished:
		t.Fatalf("reload returned before cancelling Alice: %+v", result)
	case <-time.After(time.Second):
		t.Fatal("reload did not cancel Alice")
	}
	select {
	case result := <-finished:
		t.Fatalf("reload returned while Alice was still running: %+v", result)
	default:
	}
	if got := p.spawnedPasswords(); len(got) != 1 {
		t.Fatalf("replacement started while Alice was still running: %v", got)
	}
	alice.retire()
	result := <-finished
	summary, err := result.summary, result.err
	if err != nil {
		t.Fatalf("username change at a live address failed: %v", err)
	}
	p.mu.Lock()
	overlap := p.overlap
	p.mu.Unlock()
	if overlap {
		t.Fatal("replacement started before Alice retired")
	}
	if summary.Added != 1 || summary.Removed != 1 || summary.Rotated != 0 {
		t.Fatalf("username change summary = %+v, want one removal and one addition", summary)
	}
	want := []string{reloadTestSettings("198.51.100.52:443", "bob", "pw").Key()}
	if got := r.runningKeys(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("running = %v, want %v (summary %s)", got, want, summary)
	}
}
