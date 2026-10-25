package miner

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// fakeConn is the smallest net.Conn that satisfies the prober's use: it is
// closed immediately and nothing is read or written.
type fakeConn struct{ closed bool }

func (c *fakeConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *fakeConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c *fakeConn) Close() error                     { c.closed = true; return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return nil }
func (c *fakeConn) RemoteAddr() net.Addr             { return nil }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

// TestPinnedIpv6ProbeDialsTheV6Target pins that the probe asks the question it
// claims to ask: network tcp6, the v6-only control endpoint, through the slot's
// dial. A probe over "tcp" would let a dual-stack or v4 path answer for IPv6.
func TestPinnedIpv6ProbeDialsTheV6Target(t *testing.T) {
	conn := &fakeConn{}
	var gotNetwork, gotAddr string
	prober := pinnedIpv6Prober{
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			gotNetwork, gotAddr = network, addr
			return conn, nil
		},
		timeout: time.Second,
		now:     func() time.Time { return time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC) },
	}

	state := prober.probe(context.Background())
	if gotNetwork != "tcp6" || gotAddr != pinnedIpv6ProbeTarget {
		t.Fatalf("probe dialed %s/%s, want tcp6/%s", gotNetwork, gotAddr, pinnedIpv6ProbeTarget)
	}
	if state.Verdict != pinnedIpv6Yes || state.Class != pinnedIpv6ClassConnected {
		t.Fatalf("probe state = %+v, want yes/connected", state)
	}
	if !conn.closed {
		t.Fatal("probe left the connection open; the handshake is the proof, not the socket")
	}
}

// TestPinnedIpv6ProbeClassifiesThroughTheLedger pins the probe-to-ledger join:
// the same table that classifies an error must be what the probe records, so a
// rule added to one cannot drift from the other.
func TestPinnedIpv6ProbeClassifiesThroughTheLedger(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name      string
		err       error
		wantVerb  pinnedIpv6Verdict
		wantClass string
	}{
		{"address type", errors.New("socks connect: address type not supported"), pinnedIpv6No, pinnedIpv6ClassAddrType},
		{"proxy closed", io.EOF, pinnedIpv6No, pinnedIpv6ClassImmediateEOF},
		{"unexpected eof", io.ErrUnexpectedEOF, pinnedIpv6No, pinnedIpv6ClassImmediateEOF},
		{"host unreachable", errors.New("socks connect: host unreachable"), pinnedIpv6No, pinnedIpv6ClassHostUnreach},
		{"timeout", timeoutError{}, pinnedIpv6Unknown, pinnedIpv6ClassTimeout},
	} {
		t.Run(c.name, func(t *testing.T) {
			prober := pinnedIpv6Prober{
				dial: func(context.Context, string, string) (net.Conn, error) { return nil, c.err },
				now:  func() time.Time { return now },
			}
			state := prober.probe(context.Background())
			if state.Verdict != c.wantVerb || state.Class != c.wantClass {
				t.Fatalf("probe = %q/%q, want %q/%q", state.Verdict, state.Class, c.wantVerb, c.wantClass)
			}
			if !state.CheckedAt.Equal(now) {
				t.Fatalf("verdict stamped %v, want %v", state.CheckedAt, now)
			}
		})
	}
}

// TestPinnedIpv6ProbeWithoutDialerIsUnknown: a direct slot has no proxy to ask.
// Returning "no" there would park IPv6 on the one slot that never needed a
// verdict.
func TestPinnedIpv6ProbeWithoutDialerIsUnknown(t *testing.T) {
	prober := pinnedIpv6Prober{now: func() time.Time { return time.Unix(0, 0) }}
	state := prober.probe(context.Background())
	if state.Verdict != pinnedIpv6Unknown {
		t.Fatalf("no-dialer probe = %q, want unknown", state.Verdict)
	}
	if d := pinnedIpv6DecideAtStart(state, time.Unix(0, 0)); d.EnablePinnedV6 {
		t.Fatal("an unknown verdict enabled v6; only a fresh yes may")
	}
}

// TestPinnedIpv6ProbeHonoursItsTimeout: a proxy that never answers must not hold
// the probe (and the slot goroutine) past the budget. The dial blocks on its
// context rather than sleeping, so the test is deterministic.
func TestPinnedIpv6ProbeHonoursItsTimeout(t *testing.T) {
	prober := pinnedIpv6Prober{
		dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		timeout: 20 * time.Millisecond,
		now:     time.Now,
	}
	start := time.Now()
	state := prober.probe(context.Background())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("probe took %s against a %s budget", elapsed, prober.timeout)
	}
	if state.Verdict != pinnedIpv6Unknown {
		t.Fatalf("a timed-out probe recorded %q; a timeout is ambiguity, not a verdict", state.Verdict)
	}
}

// TestPinnedIpv6ProbeSurvivesACancelledParent: shutting the slot down must end
// the probe, not leave it dialing.
func TestPinnedIpv6ProbeSurvivesACancelledParent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prober := pinnedIpv6Prober{
		dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return nil, ctx.Err()
		},
		timeout: time.Second,
		now:     time.Now,
	}
	state := prober.probe(ctx)
	if state.Verdict != pinnedIpv6Unknown {
		t.Fatalf("cancelled probe recorded %q, want unknown", state.Verdict)
	}
}

// TestProberFromSettingsRequiresAProxy pins the direct-slot path: no settings or
// no address yields a prober with no dial, which the ledger reads as unknown.
func TestProberFromSettingsRequiresAProxy(t *testing.T) {
	for _, settings := range []*connect.ProxySettings{nil, {Address: ""}} {
		prober := proberFromSettings(settings)
		if prober.dial != nil {
			t.Fatalf("settings %+v built a dialer; a direct slot has no proxy to probe", settings)
		}
	}
	if prober := proberFromSettings(&connect.ProxySettings{Network: "tcp", Address: "198.51.100.1:1080"}); prober.dial == nil {
		t.Fatal("a proxied slot built no dialer")
	}
}
