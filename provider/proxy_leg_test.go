package provider

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/net/proxy"
	"net"
	"strings"
	"testing"
	"time"
)

func socksTimeout() error {
	return &net.OpError{
		Op:     "socks connect",
		Net:    "tcp",
		Source: &net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 1080},
		Addr:   &net.TCPAddr{IP: net.IPv4(198, 51, 100, 9), Port: 443},
		Err:    errors.New("i/o timeout"),
	}
}

// legFixture registers n proxies and resets the platform clock.
func legFixture(t *testing.T, n int) {
	t.Helper()
	proxyHealthMu.Lock()
	proxyHealthByIndex = map[int]*proxyHealth{}
	proxyHealthByKey = map[string]*proxyHealth{}
	proxyHealthMu.Unlock()
	for i := 0; i < n; i++ {
		addr := fmt.Sprintf("10.0.0.%d:1080", i+1)
		RegisterProxy(i, addr, addr)
	}
	lastPlatformDialOK.Store(0)
	proxyLegShadowShedTotal.Store(0)
	t.Cleanup(func() {
		proxyHealthMu.Lock()
		proxyHealthByIndex = map[int]*proxyHealth{}
		proxyHealthByKey = map[string]*proxyHealth{}
		proxyHealthMu.Unlock()
		lastPlatformDialOK.Store(0)
	})
}

func TestIsProxyLegError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"socks connect timeout", socksTimeout(), true},
		{"wrapped socks error", fmt.Errorf("dial platform: %w", socksTimeout()), true},
		{"plain timeout", context.DeadlineExceeded, false},
		{"plain dial error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, false},
		{"local cancellation of a socks dial", fmt.Errorf("%w: %w", context.Canceled, socksTimeout()), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isProxyLegError(c.err); got != c.want {
				t.Fatalf("isProxyLegError = %v, want %v", got, c.want)
			}
		})
	}
}

// The point of the new category: a proxy-leg failure must not feed the auth
// counters, whose consumers renew or delete a good identity.
func TestProxyLegFailureIsItsOwnCategory(t *testing.T) {
	legFixture(t, 1)
	RecordProxyLegFailure(0, socksTimeout())
	RecordProxyLegFailure(0, socksTimeout())

	proxyHealthMu.Lock()
	h := proxyHealthByIndex[0]
	leg, auth, timeout, streak := h.failures.ProxyLegFailures.Load(), h.failures.AuthFailures.Load(), h.failures.TimeoutFailures.Load(), h.legStreak
	proxyHealthMu.Unlock()
	if leg != 2 || streak != 2 {
		t.Fatalf("proxy-leg=%d streak=%d, want 2 and 2", leg, streak)
	}
	if auth != 0 || timeout != 0 {
		t.Fatalf("a proxy-leg failure must not count as auth (%d) or timeout (%d)", auth, timeout)
	}
	if ProxyAuthFailureCount(0) != 0 {
		t.Fatal("the revocation and renewal watchers read this count and must not see proxy-leg failures")
	}
}

func TestNoteControlDialClassifiesOnlyPlatformDials(t *testing.T) {
	legFixture(t, 1)
	h := newProxyTransportHealth(context.Background(), 0)

	h.noteControlDial("api", socksTimeout())
	h.noteControlDial("dns", socksTimeout())
	h.noteControlDial("platform", context.DeadlineExceeded)
	h.noteControlDial("platform", fmt.Errorf("%w: %w", context.Canceled, socksTimeout()))
	if leg, _ := proxyLegTotals(); leg != 0 {
		t.Fatalf("non-platform dials and non-socks errors are not proxy-leg evidence, counted %d", leg)
	}

	h.noteControlDial("platform", socksTimeout())
	h.noteControlDial("platform", socksTimeout())
	if leg, _ := proxyLegTotals(); leg != 2 {
		t.Fatalf("two socks failures on the platform tag must count, got %d", leg)
	}

	h.noteControlDial("platform", nil)
	proxyHealthMu.Lock()
	streak := proxyHealthByIndex[0].legStreak
	proxyHealthMu.Unlock()
	if streak != 0 {
		t.Fatalf("a platform dial that succeeded through the proxy ends its streak, got %d", streak)
	}
	if !platformReachedRecently(time.Now()) {
		t.Fatal("a successful platform dial must mark the platform as reached")
	}
}

// The logger is what connect hands the typed result to.
func TestLoggerDeliversControlDialsToTheProxyHealth(t *testing.T) {
	legFixture(t, 1)
	h := newProxyTransportHealth(context.Background(), 0)
	l := h.logger(discardLogger{}).(*transportFailureLogger)
	l.ObserveControlDial("platform", "tcp", "connect.example:443", socksTimeout())
	if leg, _ := proxyLegTotals(); leg != 1 {
		t.Fatalf("the logger must deliver the failure to the proxy's health, got %d", leg)
	}
}

func setStreak(index, streak int, since time.Time) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	h := proxyHealthByIndex[index]
	h.legStreak = streak
	h.legStreakSince = since
}

func TestProxyLegShedCandidates(t *testing.T) {
	now := time.Now()
	long := now.Add(-proxyLegShedMinAge - time.Minute)

	t.Run("a long streak while the platform is reached elsewhere is a candidate", func(t *testing.T) {
		legFixture(t, 2)
		lastPlatformDialOK.Store(now.Add(-time.Minute).UnixNano())
		setStreak(0, proxyLegShedStreak, long)
		got := proxyLegShedCandidates(now)
		if len(got) != 1 || got[0].Index != 0 {
			t.Fatalf("candidates = %+v, want proxy 0 only", got)
		}
	})
	t.Run("OUTAGE GUARD: nothing is a candidate when no platform dial succeeded anywhere", func(t *testing.T) {
		legFixture(t, 2)
		setStreak(0, proxyLegShedStreak, long)
		setStreak(1, proxyLegShedStreak, long)
		if got := proxyLegShedCandidates(now); len(got) != 0 {
			t.Fatalf("a platform outage must not make every proxy a candidate, got %+v", got)
		}
	})
	t.Run("the outage guard expires with the window", func(t *testing.T) {
		legFixture(t, 1)
		lastPlatformDialOK.Store(now.Add(-platformReachedWindow - time.Minute).UnixNano())
		setStreak(0, proxyLegShedStreak, long)
		if got := proxyLegShedCandidates(now); len(got) != 0 {
			t.Fatalf("a stale platform success is no evidence the platform is up, got %+v", got)
		}
	})
	t.Run("a short streak is not enough", func(t *testing.T) {
		legFixture(t, 1)
		lastPlatformDialOK.Store(now.UnixNano())
		setStreak(0, proxyLegShedStreak-1, long)
		if got := proxyLegShedCandidates(now); len(got) != 0 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a young streak is not enough", func(t *testing.T) {
		legFixture(t, 1)
		lastPlatformDialOK.Store(now.UnixNano())
		setStreak(0, proxyLegShedStreak, now.Add(-proxyLegShedMinAge+time.Minute))
		if got := proxyLegShedCandidates(now); len(got) != 0 {
			t.Fatalf("got %+v", got)
		}
	})
}

// Shadow means shadow: it reports once per streak and changes nothing.
func TestEvaluateProxyLegShadowReportsOncePerStreakAndShedsNothing(t *testing.T) {
	legFixture(t, 1)
	now := time.Now()
	lastPlatformDialOK.Store(now.UnixNano())
	setStreak(0, proxyLegShedStreak, now.Add(-proxyLegShedMinAge-time.Minute))

	if n := evaluateProxyLegShadow(now); n != 1 {
		t.Fatalf("first evaluation flagged %d, want 1", n)
	}
	if n := evaluateProxyLegShadow(now.Add(time.Minute)); n != 0 {
		t.Fatalf("the same streak must be reported once, flagged %d more", n)
	}
	if got := proxyLegShadowShedTotal.Load(); got != 1 {
		t.Fatalf("shadow total = %d, want 1", got)
	}
	if _, flagged := proxyLegTotals(); flagged != 1 {
		t.Fatalf("flagged = %d, want 1", flagged)
	}
	proxyHealthMu.Lock()
	_, stillRegistered := proxyHealthByIndex[0]
	proxyHealthMu.Unlock()
	if !stillRegistered {
		t.Fatal("the shadow decision must not remove the proxy")
	}

	// a platform dial through the proxy ends the streak and re-arms the report
	recordPlatformDialOK(0)
	if _, flagged := proxyLegTotals(); flagged != 0 {
		t.Fatalf("a recovered proxy is no longer flagged, got %d", flagged)
	}
}

func TestProxyErrorLineShowsTheProxyLegCount(t *testing.T) {
	legFixture(t, 1)
	RecordProxyLegFailure(0, socksTimeout())
	proxyHealthMu.Lock()
	h := proxyHealthByIndex[0]
	line := formatProxyErrorEntry(0, h.address, &h.failures, h.lastError, h.lastErrorAt)
	proxyHealthMu.Unlock()
	if !strings.Contains(line, "proxy-leg:1") {
		t.Fatalf("the health line must show the proxy-leg count, got %q", line)
	}
}

func TestReRegisteringAProxyClearsItsStreak(t *testing.T) {
	legFixture(t, 1)
	for i := 0; i < proxyLegShedStreak; i++ {
		RecordProxyLegFailure(0, socksTimeout())
	}
	RegisterProxy(0, "10.0.0.1:1080", "10.0.0.1:1080")
	proxyHealthMu.Lock()
	streak, leg := proxyHealthByIndex[0].legStreak, proxyHealthByIndex[0].failures.ProxyLegFailures.Load()
	proxyHealthMu.Unlock()
	if streak != 0 || leg != 0 {
		t.Fatalf("a re-registered proxy starts clean, got streak=%d leg=%d", streak, leg)
	}
}

// The classification rests on what the real socks dialer returns, so check it
// against one: a server that accepts the TCP connection and never answers the
// handshake is a proxy that cannot carry traffic.
func TestRealSocksDialFailureIsAProxyLegError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// hold the connection open, say nothing
			t.Cleanup(func() { c.Close() })
		}
	}()

	dialer, err := proxy.SOCKS5("tcp", ln.Addr().String(), nil, proxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = dialer.(proxy.ContextDialer).DialContext(ctx, "tcp", "connect.example:443")
	if err == nil {
		t.Fatal("a proxy that never answers must fail the dial")
	}
	if !isProxyLegError(err) {
		t.Fatalf("the real socks dialer's error must classify as a proxy-leg error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "socks connect") {
		t.Fatalf("expected the error text Hermes saw in production, got %v", err)
	}
}
