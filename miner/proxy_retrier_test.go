package miner

import (
	"context"
	"testing"
	"time"
)

// A retry waiting in a stuck retirement ends with the run's context, and one
// cancelled during its backoff never reloads at all.
func TestRetryReloadRunsOnTheRunContext(t *testing.T) {
	r, blocked, spawned := stuckRotationReloader(t, "198.51.100.91:443", time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	join := newSlotJoin()
	retrier := newProxyRetrier(ctx, join, func(c context.Context) error {
		_, err := r.reloadContext(c)
		return err
	})
	retrier.wait = func(int) time.Duration { return 0 }
	retrier.retry("198.51.100.91:443")
	<-blocked
	cancel()

	finished := make(chan struct{})
	go func() { join.wait(nil); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the retry kept waiting on a stuck worker after the run's context was cancelled")
	}
	if *spawned != 0 {
		t.Fatalf("a retry started %d workers after the run gave up", *spawned)
	}
}

func TestRetryCancelledDuringBackoffNeverReloads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	join := newSlotJoin()
	reloads := 0
	retrier := newProxyRetrier(ctx, join, func(context.Context) error { reloads++; return nil })
	retrier.wait = func(int) time.Duration { return time.Hour }
	retrier.retry("198.51.100.92:443")
	cancel()
	join.wait(nil)
	if reloads != 0 {
		t.Fatalf("reloaded %d times after cancellation", reloads)
	}
}
