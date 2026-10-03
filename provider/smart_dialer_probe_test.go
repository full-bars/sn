package provider

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The loop waits out the initial delay, then probes on every tick until the
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
		})
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

// The probe starts only after the proxy authenticated, never before: a proxy
// that never gets through auth, or sits in slow retry, must not probe.
func TestSmartDialerProbeStartsAfterAuthSucceeds(t *testing.T) {
	source, err := os.ReadFile("provide.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	create := strings.Index(text, "connect.NewClientStrategy(proxyCtx, clientStrategySettings)")
	authFailed := strings.Index(text, "provideHandleAuthFailure(st, proxyCtx, proxySettings, isNative, isURLSourced, err)")
	start := strings.Index(text, "startSmartDialerProbes(proxyCtx, clientStrategy, st.apiUrl")
	if create < 0 || authFailed < 0 {
		t.Fatal("provideWithProxy no longer creates its strategy or handles auth failure this way; update this test")
	}
	if start < 0 {
		t.Fatal("provideWithProxy does not start the smart dialer probe")
	}
	if start < authFailed {
		t.Fatal("the smart dialer probe starts before the auth outcome is known; it must start only after auth succeeds")
	}
	if strings.Count(text, "startSmartDialerProbes(") != 1 {
		t.Fatal("expected exactly one probe start per proxy")
	}
}
