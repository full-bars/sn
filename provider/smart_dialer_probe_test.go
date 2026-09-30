package provider

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The loop waits out the initial delay (so a starting proxy is not probed
// while it is still authenticating), then probes on every tick until the
// context ends.
func TestSmartDialerProbeLoopProbesAfterDelayThenRepeats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSmartDialerProbes(ctx, 20*time.Millisecond, 10*time.Millisecond, func(context.Context) int {
			calls.Add(1)
			return 0
		}, func(string, ...any) {})
	}()

	time.Sleep(5 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("probed %d times inside the initial delay, want 0", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := calls.Load(); got < 3 {
		t.Fatalf("probed %d times, want at least 3 (initial then repeating)", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the probe loop did not stop when the context was canceled")
	}
}

// A round that attempted nothing (smart dialer off, or every transport already
// measured) is silent; a round that probed says so once.
func TestSmartDialerProbeLoopLogsOnlyRoundsThatProbed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var rounds atomic.Int32
	var logged atomic.Int32
	go runSmartDialerProbes(ctx, 0, 5*time.Millisecond, func(context.Context) int {
		if rounds.Add(1) == 2 {
			return 3
		}
		return 0
	}, func(string, ...any) { logged.Add(1) })

	deadline := time.Now().Add(2 * time.Second)
	for rounds.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if got := logged.Load(); got != 1 {
		t.Fatalf("logged %d times over %d rounds, want exactly 1 (the round that probed 3)", got, rounds.Load())
	}
}

// The probe is only worth anything if every proxy's strategy actually starts
// it, and the start is one line inside a very large function that no unit test
// drives. Read the source so that a refactor which drops the line fails here
// instead of silently leaving the smart dialer with nothing to compare.
func TestEveryProxyStrategyStartsTheSmartDialerProbe(t *testing.T) {
	source, err := os.ReadFile("provide.go")
	if err != nil {
		t.Fatal(err)
	}
	create := strings.Index(string(source), "connect.NewClientStrategy(proxyCtx, clientStrategySettings)")
	start := strings.Index(string(source), "startSmartDialerProbes(proxyCtx, clientStrategy, st.apiUrl")
	if create < 0 {
		t.Fatal("provideWithProxy no longer creates its strategy this way; update this test")
	}
	if start < 0 || start < create {
		t.Fatal("provideWithProxy does not start the smart dialer probe right after creating the proxy's client strategy")
	}
}
