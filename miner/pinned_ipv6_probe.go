package miner

// The IPv6 probe: one dial, through the slot's own proxy, to a v6-only target.
//
// The ledger (pinned_ipv6_ledger.go) decides what to do with the answer; this
// file only produces it. The probe is deliberately one dial and not a sweep:
// the LA7 run proved that asking 500 proxies the same question at once is what
// costs a core, and the verdict is worth nothing if collecting it is the
// outage.

import (
	"context"
	"errors"
	"io"
	"net"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// pinnedIpv6ProbeTarget is the v6-only control endpoint. It has to be an
// address that only answers over IPv6: a dual-stack name would let an IPv4-only
// proxy "succeed" and hand back a false yes, which is worse than no verdict at
// all.
const pinnedIpv6ProbeTarget = "connect-v6.bringyour.com:443"

// pinnedIpv6ProbeTimeout is one dial's budget. Long enough for a slow proxy to
// answer, short enough that a parked probe does not hold a slot's goroutine for
// the life of the run.
const pinnedIpv6ProbeTimeout = 8 * time.Second

// pinnedIpv6Prober answers one question for one proxy. dial is a seam so the
// classification is testable without a network, and now is a seam so a caller
// can stamp the verdict with the same clock the ledger uses.
type pinnedIpv6Prober struct {
	dial    connect.DialContextFunction
	timeout time.Duration
	now     func() time.Time
}

// newPinnedIpv6Prober builds the real prober for one slot's proxy.
//
// forward is the dial the SOCKS client uses to reach the proxy itself. The
// caller passes the slot's own base dial (the bandwidth-wrapped one) so the
// probe travels the same path a real dial does - a probe that bypasses the
// slot's path can answer a question nobody asked.
func newPinnedIpv6Prober(settings *connect.ProxySettings, forward proxy.Dialer) pinnedIpv6Prober {
	if settings == nil {
		return pinnedIpv6Prober{now: time.Now, timeout: pinnedIpv6ProbeTimeout}
	}
	return pinnedIpv6Prober{
		dial:    settings.NewDialContext(context.Background(), forward),
		timeout: pinnedIpv6ProbeTimeout,
		now:     time.Now,
	}
}

// probe runs the dial and returns the ledger state it implies.
//
// It never returns an error: "could not ask" is a result (unknown), not a
// failure to record. A caller that treated it as an error would either retry
// in a tight loop or park the family on a proxy that was merely slow.
func (self pinnedIpv6Prober) probe(ctx context.Context) pinnedIpv6State {
	now := self.now
	if now == nil {
		now = time.Now
	}
	if self.dial == nil {
		// No dialer means no proxy to ask (a direct slot). That is not an IPv6
		// verdict; the direct slot keeps v6 enabled exactly as today.
		return pinnedIpv6State{Verdict: pinnedIpv6Unknown, CheckedAt: now(), Class: pinnedIpv6ClassUnknown}
	}

	timeout := self.timeout
	if timeout <= 0 {
		timeout = pinnedIpv6ProbeTimeout
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := self.dial(dialCtx, "tcp6", pinnedIpv6ProbeTarget)
	if err == nil {
		// Nothing is exchanged: a completed SOCKS connect to a v6 address is
		// the whole proof. Close promptly so the probe costs the proxy one
		// handshake and nothing else.
		_ = conn.Close()
		return notePinnedIpv6Success(pinnedIpv6State{}, now())
	}

	sawEOF := isEOF(err)
	_, class := classifyPinnedIpv6Probe(err, sawEOF)
	verdict, _ := classifyPinnedIpv6Probe(err, sawEOF)
	return pinnedIpv6State{
		Verdict:   verdict,
		CheckedAt: now(),
		Class:     class,
	}
}

// isEOF reports whether the failure is the proxy closing the connection without
// a SOCKS reply.
//
// This is the shape LA7 saw: an IPv4-only proxy accepting the SOCKS request for
// a tcp6 target and dropping the connection, which surfaces as io.EOF or, when
// part of the reply arrived, io.ErrUnexpectedEOF. go's SOCKS client wraps it, so
// errors.Is is tried before the text.
func isEOF(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "unexpected eof") || strings.HasSuffix(text, ": eof")
}

// pinnedIpv6ProbeTransport is the dial a probe should use when the caller has
// no slot-specific one: a plain dialer that this process owns. Exists so a test
// or a diagnostic can exercise the real prober without a proxy.
func pinnedIpv6ProbeTransport() proxy.Dialer {
	return &net.Dialer{Timeout: pinnedIpv6ProbeTimeout}
}

// proberFromSettings is the wiring helper: a prober for a slot's proxy, or the
// zero prober (no dial, unknown) for a direct slot.
func proberFromSettings(settings *connect.ProxySettings) pinnedIpv6Prober {
	if settings == nil || settings.Address == "" {
		return pinnedIpv6Prober{now: time.Now, timeout: pinnedIpv6ProbeTimeout}
	}
	return newPinnedIpv6Prober(settings, pinnedIpv6ProbeTransport())
}

// runtimeGoroutines is a test seam for assertions about the probe's cost; kept
// out of the hot path and out of probe itself.
var runtimeGoroutines = goruntime.NumGoroutine
