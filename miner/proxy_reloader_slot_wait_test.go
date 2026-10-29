package miner

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestReloadUsernameRotationWaitsForPlatformSlot(t *testing.T) {
	alice := reloadTestSettings("198.51.100.52:443", "alice", "pw")
	bob := reloadTestSettings("198.51.100.52:443", "bob", "pw")
	desired := []*connect.ProxySettings{alice}
	cancelled := make(chan struct{})
	done := make(chan struct{})
	spawned := make(chan string, 2)
	r := newMinerProxyReloader(func() ([]*connect.ProxySettings, int, error) { return desired, 0, nil }, nil)
	r.retireTimeout = time.Second
	r.spawnGeneration = func(s *connect.ProxySettings, _ uint64) (context.CancelFunc, <-chan struct{}, error) {
		spawned <- s.Auth.User
		if s.Auth.User == "alice" {
			return func() { close(cancelled) }, done, nil
		}
		return func() {}, nil, nil
	}
	if _, err := r.reload(); err != nil {
		t.Fatal(err)
	}
	<-spawned
	desired = []*connect.ProxySettings{bob}
	finished := make(chan error, 1)
	go func() { _, err := r.reload(); finished <- err }()
	<-cancelled
	select {
	case who := <-spawned:
		close(done)
		<-finished
		t.Fatalf("%s started before alice's worker returned", who)
	case <-time.After(100 * time.Millisecond):
	}
	close(done)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if who := <-spawned; who != "bob" {
		t.Fatalf("spawned %s, want bob", who)
	}
}

func TestReloadUsernameRotationTimeoutRetries(t *testing.T) {
	alice := reloadTestSettings("198.51.100.53:443", "alice", "pw")
	bob := reloadTestSettings("198.51.100.53:443", "bob", "pw")
	desired := []*connect.ProxySettings{alice}
	done := make(chan struct{})
	spawned := 0
	r := newMinerProxyReloader(func() ([]*connect.ProxySettings, int, error) { return desired, 0, nil }, nil)
	r.retireTimeout = 20 * time.Millisecond
	r.spawnGeneration = func(s *connect.ProxySettings, _ uint64) (context.CancelFunc, <-chan struct{}, error) {
		spawned++
		if s.Auth.User == "alice" {
			return func() {}, done, nil
		}
		return func() {}, nil, nil
	}
	if _, err := r.reload(); err != nil {
		t.Fatal(err)
	}
	desired = []*connect.ProxySettings{bob}
	if _, err := r.reload(); err == nil {
		t.Fatal("expected timeout for unfinished alice")
	}
	if spawned != 1 {
		t.Fatalf("started replacement over unfinished worker: %d", spawned)
	}
	if _, err := r.reload(); err == nil {
		t.Fatal("retry must still wait for alice")
	}
	close(done)
	if _, err := r.reload(); err != nil {
		t.Fatal(err)
	}
	if spawned != 2 {
		t.Fatalf("replacement did not start after retirement: %d", spawned)
	}
}
