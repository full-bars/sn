package miner

// The observer turns the slot's own dial outcomes into IPv6 verdicts. These
// tests drive it with fake dialers and an injected clock: no network, no
// sleeps, no live box.

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// v6TestConn is a net.Conn that exists only to be returned and closed.
type v6TestConn struct{}

func (v6TestConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (v6TestConn) Write(b []byte) (int, error)      { return len(b), nil }
func (v6TestConn) Close() error                     { return nil }
func (v6TestConn) LocalAddr() net.Addr              { return nil }
func (v6TestConn) RemoteAddr() net.Addr             { return nil }
func (v6TestConn) SetDeadline(time.Time) error      { return nil }
func (v6TestConn) SetReadDeadline(time.Time) error  { return nil }
func (v6TestConn) SetWriteDeadline(time.Time) error { return nil }

func v6TestSettings() *connect.ProxySettings {
	return &connect.ProxySettings{
		Network: "tcp",
		Address: "198.51.100.9:1080",
		Auth:    &proxy.Auth{User: "session-a", Password: "pw"},
	}
}

// newTestV6Observer builds the observer with the same hosts production would
// pass: the v6 pin host, and the v4 and agnostic hosts that corroborate.
func newTestV6Observer(t *testing.T, ledger *v6Ledger, now func() time.Time, dial connect.DialContextFunction) *v6DialObserver {
	t.Helper()
	observer := newV6DialObserver(
		dial,
		v6TestSettings(),
		"wss://connect-v6.example",
		"wss://connect-v4.example",
		"wss://connect.example",
		ledger,
		nil,
		now,
	)
	if observer == nil {
		t.Fatal("observer was not built for a proxied slot")
	}
	return observer
}

// succeedV4 records a corroborating platform success through the same slot.
func succeedV4(t *testing.T, observer *v6DialObserver) {
	t.Helper()
	if _, err := observer.DialContext(context.Background(), "tcp", "connect-v4.example:443"); err != nil {
		t.Fatalf("corroborating v4 dial: %v", err)
	}
}

// TestV6ObserverFlipsToNoOnThreeCorroboratedFailures is the rule that keeps a
// dead proxy from costing dual-stack: three classified v6 failures, each while
// v4 through the same slot works, mean this proxy does not carry IPv6.
func TestV6ObserverFlipsToNoOnThreeCorroboratedFailures(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ledger := newV6Ledger(func() time.Time { return now })
	var dials int
	observer := newTestV6Observer(t, ledger, func() time.Time { return now },
		func(_ context.Context, network, addr string) (net.Conn, error) {
			if network == "tcp6" {
				dials++
				return nil, errors.New("socks connect tcp6 198.51.100.9:1080->connect-v6.example:443: unknown error host unreachable")
			}
			return v6TestConn{}, nil
		})
	succeedV4(t, observer)

	for i := 1; i <= 2; i++ {
		_, _ = observer.DialContext(context.Background(), "tcp6", "connect-v6.example:443")
		if got := ledger.Verdict(v6LedgerKey(v6TestSettings())); got != v6Unknown {
			t.Fatalf("after %d failures the verdict is %v; a flaky proxy must not lose dual-stack on one or two", i, got)
		}
	}
	_, _ = observer.DialContext(context.Background(), "tcp6", "connect-v6.example:443")
	if got := ledger.Verdict(v6LedgerKey(v6TestSettings())); got != v6No {
		t.Fatalf("verdict after 3 corroborated failures = %v, want no", got)
	}
	if dials != 3 {
		t.Fatalf("inner dialer called %d times, want 3", dials)
	}
}

// TestV6ObserverNeedsCorroboration: a proxy or platform outage must read as
// unknown, otherwise every restart during an outage would demote the fleet.
func TestV6ObserverNeedsCorroboration(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ledger := newV6Ledger(func() time.Time { return now })
	observer := newTestV6Observer(t, ledger, func() time.Time { return now },
		func(_ context.Context, network, _ string) (net.Conn, error) {
			if network == "tcp6" {
				return nil, errors.New("socks connect tcp6 198.51.100.9:1080->connect-v6.example:443: unknown error host unreachable")
			}
			return nil, errors.New("proxy unreachable")
		})
	for i := 0; i < 6; i++ {
		_, _ = observer.DialContext(context.Background(), "tcp6", "connect-v6.example:443")
	}
	if got := ledger.Verdict(v6LedgerKey(v6TestSettings())); got != v6Unknown {
		t.Fatalf("verdict without a working v4 path = %v, want unknown", got)
	}
}

// TestV6ObserverIgnoresCancel keeps our own cancellation out of the evidence,
// including the EOF that a cancelled SOCKS stream can surface as.
func TestV6ObserverIgnoresCancel(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ledger := newV6Ledger(func() time.Time { return now })
	observer := newTestV6Observer(t, ledger, func() time.Time { return now },
		func(ctx context.Context, network, _ string) (net.Conn, error) {
			if network == "tcp6" {
				return nil, io.EOF
			}
			return v6TestConn{}, nil
		})
	succeedV4(t, observer)
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _ = observer.DialContext(ctx, "tcp6", "connect-v6.example:443")
	}
	if got := ledger.Verdict(v6LedgerKey(v6TestSettings())); got != v6Unknown {
		t.Fatalf("cancelled dials produced verdict %v, want unknown", got)
	}
}

// TestV6ObserverRecordsYesOnSuccess: one completed SOCKS connect to the
// v6-only name is proof.
func TestV6ObserverRecordsYesOnSuccess(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ledger := newV6Ledger(func() time.Time { return now })
	observer := newTestV6Observer(t, ledger, func() time.Time { return now },
		func(context.Context, string, string) (net.Conn, error) { return v6TestConn{}, nil })
	if _, err := observer.DialContext(context.Background(), "tcp6", "connect-v6.example:443"); err != nil {
		t.Fatalf("v6 dial: %v", err)
	}
	if got := ledger.Verdict(v6LedgerKey(v6TestSettings())); got != v6Yes {
		t.Fatalf("verdict after a successful v6 dial = %v, want yes", got)
	}
}

// TestV6ObserverRefusesOnceParked is the storm fix: after a no, the v6 pin's
// dials never reach the proxy, while ordinary tcp traffic still does.
func TestV6ObserverRefusesOnceParked(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ledger := newV6Ledger(func() time.Time { return now })
	key := v6LedgerKey(v6TestSettings())
	ledger.Record(key, v6No)

	var innerCalls int
	observer := newTestV6Observer(t, ledger, func() time.Time { return now },
		func(_ context.Context, network, _ string) (net.Conn, error) {
			innerCalls++
			return v6TestConn{}, nil
		})
	if _, err := observer.DialContext(context.Background(), "tcp6", "connect-v6.example:443"); !isV6Held(err) {
		t.Fatalf("parked proxy dial error = %v, want the local refusal", err)
	}
	if innerCalls != 0 {
		t.Fatalf("inner dialer called %d times for a parked proxy, want 0", innerCalls)
	}
	if _, err := observer.DialContext(context.Background(), "tcp", "connect.example:443"); err != nil {
		t.Fatalf("ordinary tcp dial was blocked: %v", err)
	}
	if innerCalls != 1 {
		t.Fatalf("ordinary tcp dials = %d, want 1", innerCalls)
	}
}

// TestV6ObserverIgnoresOtherHosts: the same wrapper carries exit-NAT client
// traffic, so a tcp6 dial to anywhere else, and tcp to the v6 host, are not
// evidence about this proxy's IPv6.
func TestV6ObserverIgnoresOtherHosts(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ledger := newV6Ledger(func() time.Time { return now })
	observer := newTestV6Observer(t, ledger, func() time.Time { return now },
		func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("socks connect tcp6 198.51.100.9:1080->example.com:443: unknown error host unreachable")
		})
	for i := 0; i < 6; i++ {
		_, _ = observer.DialContext(context.Background(), "tcp6", "example.com:443")
		_, _ = observer.DialContext(context.Background(), "tcp", "connect-v6.example:443")
	}
	if got := ledger.Verdict(v6LedgerKey(v6TestSettings())); got != v6Unknown {
		t.Fatalf("unrelated traffic produced verdict %v, want unknown", got)
	}
}

// TestV6ObserverFlipsALiveYes: a proxy that worked and then stopped flips on
// the ledger's own three-strike service counter, not on one failure.
func TestV6ObserverFlipsALiveYes(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ledger := newV6Ledger(func() time.Time { return now })
	key := v6LedgerKey(v6TestSettings())
	ledger.Record(key, v6Yes)

	var fail bool = true
	observer := newTestV6Observer(t, ledger, func() time.Time { return now },
		func(_ context.Context, network, _ string) (net.Conn, error) {
			if network == "tcp6" && fail {
				return nil, errors.New("socks connect tcp6 198.51.100.9:1080->connect-v6.example:443: unknown error host unreachable")
			}
			return v6TestConn{}, nil
		})
	for i := 1; i <= 2; i++ {
		_, _ = observer.DialContext(context.Background(), "tcp6", "connect-v6.example:443")
		if got := ledger.Verdict(key); got != v6Yes {
			t.Fatalf("after %d failures a live yes became %v", i, got)
		}
	}
	_, _ = observer.DialContext(context.Background(), "tcp6", "connect-v6.example:443")
	if got := ledger.Verdict(key); got != v6No {
		t.Fatalf("after 3 failures a live yes is %v, want no", got)
	}
}

// TestV6LedgerKeyIncludesTheUsername: rotating gateways share host:port while
// the exit changes, so the session in the username is part of the identity.
func TestV6LedgerKeyIncludesTheUsername(t *testing.T) {
	base := &connect.ProxySettings{Address: "198.51.100.9:1080", Auth: &proxy.Auth{User: "session-a"}}
	other := &connect.ProxySettings{Address: "198.51.100.9:1080", Auth: &proxy.Auth{User: "session-b"}}
	if v6LedgerKey(base) == v6LedgerKey(other) {
		t.Fatal("two sessions on one gateway share a verdict key; a rotating exit would inherit another's IPv6 verdict")
	}
	if v6LedgerKey(nil) != "" {
		t.Fatal("a direct slot produced a ledger key")
	}
}
