package miner

// The observer is what turns a connect dial result into leg evidence. These
// tests pin the classification, because treating a cancelled dial or a platform
// failure as a proxy fault would blame the wrong end.

import (
	"context"
	"errors"
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
	// a failure to reach the proxy is not evidence about its ability to
	// carry traffic: the inner error is a dial failure for the proxy address
	proxyUnreachable := &net.OpError{Op: "socks connect", Err: &net.OpError{Op: "dial", Err: context.DeadlineExceeded}}
	if isProxyLegError(proxyUnreachable) {
		t.Fatal("an unreachable proxy was counted as a leg failure")
	}
	auth := &net.OpError{Op: "socks connect", Err: errors.New("socks: username/password authentication failed")}
	if isProxyLegError(auth) {
		t.Fatal("a rejected login was counted as a leg failure")
	}
	// the proxy answered and could not reach the destination: this is the
	// leg failure the counter exists for
	if !isProxyLegError(&net.OpError{Op: "socks connect", Err: errors.New("connect failed")}) {
		t.Fatal("a proxy that answered and failed to connect was not counted")
	}
	if !isProxyLegError(&net.OpError{Op: "socks connect", Err: context.DeadlineExceeded}) {
		t.Fatal("a socks connect timeout is not counted")
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
		minerProxyLeg.RecordFailure(id, start.Add(time.Duration(i)*3*time.Minute))
	}
	now := start.Add(proxyLegShedMinAge + 2*time.Minute)

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

func TestLegOnlyOverlayKeepsPersistedHealthFields(t *testing.T) {
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

	const key = "5.6.7.8:1080"
	// a leg failure arrives before this process has made any health
	// observation of its own
	logger := &proxyLegObservingLogger{proxyLegId: key}
	logger.ObserveControlDial("platform", "tcp", "platform.example:443", &net.OpError{Op: "socks connect", Err: errors.New("connect failed")})

	proxies := map[string]ProxyEntry{key: {Health: "recently_offline", DownSince: "2026-10-23T08:00:00Z", AuthFailures: 7}}
	overlayLiveProxyHealth(proxies)
	entry := proxies[key]
	if entry.ProxyLegFailures != 1 {
		t.Fatalf("proxy_leg_failures = %d, expected 1", entry.ProxyLegFailures)
	}
	if entry.AuthFailures != 7 {
		t.Fatalf("a leg-only overlay erased persisted auth failures: %d", entry.AuthFailures)
	}
	if entry.DownSince != "2026-10-23T08:00:00Z" {
		t.Fatalf("a leg-only overlay erased the persisted down-since: %q", entry.DownSince)
	}
	if entry.Health != "recently_offline" {
		t.Fatalf("a leg-only overlay changed health to %q", entry.Health)
	}
}
