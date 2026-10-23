package miner

import (
	"testing"
	"time"
)

// A failure that is not evidence about the proxy (backend degraded, attempt
// cut short by a deadline) backs off for the slow retry delay instead of a
// give-up ramp step: it must not park a working-but-slow proxy for an hour,
// and the hold must actually be recorded in the failure history.
func TestProxyAuthSlowBackoffIsShort(t *testing.T) {
	key := "10.0.0.9:1080"
	if proxyURLSlowAuthRetryDelay != 30*time.Second {
		t.Fatalf("proxyURLSlowAuthRetryDelay = %v, want 30s", proxyURLSlowAuthRetryDelay)
	}
	got := proxyAuthSlowBackoff(key)
	if got != proxyURLSlowAuthRetryDelay {
		t.Fatalf("proxyAuthSlowBackoff = %v, want %v", got, proxyURLSlowAuthRetryDelay)
	}
	if globalProxyFailureHistory.Eligible(key, time.Now().Add(29*time.Second)) {
		t.Fatal("the slow backoff must still hold at 29s")
	}
	if !globalProxyFailureHistory.Eligible(key, time.Now().Add(31*time.Second)) {
		t.Fatal("the slow backoff must be released by 31s")
	}
}
