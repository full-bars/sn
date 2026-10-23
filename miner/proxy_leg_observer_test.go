package miner

// The observer is what turns a connect dial result into leg evidence. These
// tests pin the classification, because treating a cancelled dial or a platform
// failure as a proxy fault would blame the wrong end.

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestIsProxyLegErrorOnlyMatchesTheSocksConnectStep(t *testing.T) {
	if isProxyLegError(nil) {
		t.Fatal("nil is not a leg error")
	}
	if isProxyLegError(context.Canceled) {
		t.Fatal("a cancelled dial is not a leg error")
	}
	if isProxyLegError(&net.OpError{Op: "dial", Err: context.DeadlineExceeded}) {
		t.Fatal("a plain dial failure is not a leg error")
	}
	if !isProxyLegError(&net.OpError{Op: "socks connect", Err: context.DeadlineExceeded}) {
		t.Fatal("a socks connect failure is a leg error")
	}
}

func TestObserverRecordsFailuresAndClearsOnSuccess(t *testing.T) {
	minerProxyLeg = proxyLegTracker{}
	defer func() { minerProxyLeg = proxyLegTracker{} }()

	logger := &proxyLegObservingLogger{proxyLegId: "proxy:test"}
	now := time.Now()
	for i := 0; i < proxyLegShedStreak; i++ {
		logger.ObserveControlDial("platform", "tcp", "example:443", &net.OpError{Op: "socks connect", Err: context.DeadlineExceeded})
	}
	failures, _ := proxyLegTotals()
	if failures != proxyLegShedStreak {
		t.Fatalf("failures = %d, expected %d", failures, proxyLegShedStreak)
	}
	logger.ObserveControlDial("platform", "tcp", "example:443", nil)
	if failures, _ = proxyLegTotals(); failures != 0 {
		t.Fatalf("a successful dial left %d failures, expected the streak cleared", failures)
	}
	// another tag is not evidence about the proxy
	logger.ObserveControlDial("api", "tcp", "example:443", &net.OpError{Op: "socks connect", Err: context.DeadlineExceeded})
	if failures, _ = proxyLegTotals(); failures != 0 {
		t.Fatalf("the api tag counted %d leg failures", failures)
	}
	_ = now
}

func TestShadowSweepFlagsOnceAndHonoursTheOutageGuard(t *testing.T) {
	minerProxyLeg = proxyLegTracker{}
	lastPlatformDialOK.Store(0)
	defer func() {
		minerProxyLeg = proxyLegTracker{}
		lastPlatformDialOK.Store(0)
	}()

	start := time.Now()
	id := "proxy:sweep"
	for i := 0; i < proxyLegShedStreak; i++ {
		minerProxyLeg.RecordFailure(id, start.Add(time.Duration(i)*time.Minute))
	}
	now := start.Add(proxyLegShedMinAge)

	proxyLegShadowSweep(now)
	if _, flagged := proxyLegTotals(); flagged != 0 {
		t.Fatal("flagged while no path had reached the platform")
	}

	lastPlatformDialOK.Store(now.Add(-time.Minute).UnixNano())
	proxyLegShadowSweep(now)
	if _, flagged := proxyLegTotals(); flagged != 1 {
		t.Fatal("did not flag a full streak while another path reached the platform")
	}
	proxyLegShadowSweep(now)
	if _, flagged := proxyLegTotals(); flagged != 1 {
		t.Fatal("flagged twice; the sweep would log the same proxy twice")
	}
}

func TestLegFailureReachesTheProxyEntryWithoutTouchingAuthCounters(t *testing.T) {
	proxyLiveHealthRegistry.Lock()
	proxyLiveHealthRegistry.byKey = map[string]proxyLiveHealthEntry{}
	proxyLiveHealthRegistry.Unlock()
	minerProxyLeg = proxyLegTracker{}
	defer func() {
		minerProxyLeg = proxyLegTracker{}
		proxyLiveHealthRegistry.Lock()
		proxyLiveHealthRegistry.byKey = map[string]proxyLiveHealthEntry{}
		proxyLiveHealthRegistry.Unlock()
	}()

	const key = "1.2.3.4:8080"
	logger := &proxyLegObservingLogger{proxyLegId: key}
	logger.ObserveControlDial("platform", "tcp", "platform.example:443", &net.OpError{Op: "socks connect", Err: context.DeadlineExceeded})

	proxies := map[string]ProxyEntry{key: {Health: "up"}}
	overlayLiveProxyHealth(proxies)
	entry := proxies[key]
	if entry.ProxyLegFailures != 1 {
		t.Fatalf("proxy_leg_failures = %d, expected 1", entry.ProxyLegFailures)
	}
	if entry.AuthFailures != 0 {
		t.Fatalf("a leg failure counted as %d auth failures", entry.AuthFailures)
	}
	if entry.Health != "up" {
		t.Fatalf("a leg failure changed health to %q", entry.Health)
	}
}
