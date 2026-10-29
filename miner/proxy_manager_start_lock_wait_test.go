package miner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A lock holder that outlives the old five-attempt, 100 ms retry window is a
// slow but live process, not a reason to abort provider startup. Startup must
// still be waiting after that window and complete once the holder lets go.
func TestProxyStartSurvivesLockHeldPastOldRetryWindow(t *testing.T) {
	withTempHome(t)
	release, err := acquireProxyLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	m := newProxyManager(defaultProxyManagerConfig())
	finished := make(chan error, 1)
	go func() { _, _, err := m.start(context.Background()); finished <- err }()
	select {
	case err := <-finished:
		if err == nil {
			m.stop()
		}
		t.Fatalf("startup ended while the lock was held past the old retry window: %v", err)
	case <-time.After(900 * time.Millisecond):
	}
	release()
	if err := <-finished; err != nil {
		t.Fatalf("startup failed after the lock holder released: %v", err)
	}
	m.stop()
}

// The wait is not unbounded: a lock that is never released fails startup with
// the lock error, and a cancelled run context ends the wait at once.
func TestProxyStartLockWaitIsBoundedAndCancellable(t *testing.T) {
	withTempHome(t)
	release, err := acquireProxyLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	prev := proxyStartLockWait
	proxyStartLockWait = 250 * time.Millisecond
	defer func() { proxyStartLockWait = prev }()
	m := newProxyManager(defaultProxyManagerConfig())
	if _, _, err := m.start(context.Background()); err == nil {
		m.stop()
		t.Fatal("startup succeeded while another process held the lock")
	}

	proxyStartLockWait = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m = newProxyManager(defaultProxyManagerConfig())
	if _, _, err := m.start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled startup error = %v, want context.Canceled", err)
	}
}

// An error that waiting cannot cure must surface at once with its own cause,
// not after the full startup wait.
func TestProxyLockWaitReturnsNonRetryableErrorImmediately(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("URNETWORK_STATE_DIR", filepath.Join(blocker, "state"))

	start := time.Now()
	_, err := acquireProxyLockWait(context.Background(), time.Minute)
	if err == nil {
		t.Fatal("acquired the lock under an uncreatable state directory")
	}
	if errors.Is(err, errProxyLockHeld) {
		t.Fatalf("non-retryable error reported as a held lock: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("returned after %s, want an immediate failure", elapsed)
	}
}

// A lock that is genuinely held is still waited on, and acquired once the
// holder releases it.
func TestProxyLockWaitAcquiresAfterHolderReleases(t *testing.T) {
	withTempHome(t)
	release, err := acquireProxyLock()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(350 * time.Millisecond)
		release()
	}()

	got, err := acquireProxyLockWait(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("wait did not acquire after release: %v", err)
	}
	got()
}
