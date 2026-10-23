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
