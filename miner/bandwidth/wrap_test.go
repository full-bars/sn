package bandwidth

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestWrapNilDialContextSettings(t *testing.T) {
	bw := &ProxyBandwidth{}
	wrapped := WrapDialContextSettings(nil, bw, "test-proxy")

	if wrapped == nil {
		t.Fatal("expected non-nil settings")
	}
	if wrapped.DialContext == nil {
		t.Fatal("expected non-nil DialContext")
	}
	// Nothing to carry: a nil input has no H3 endpoint seam to preserve.
	if wrapped.PacketConnFactory != nil {
		t.Fatal("expected no PacketConnFactory when the input had none")
	}
}

func TestWrapExistingDialContextSettings(t *testing.T) {
	called := false
	origDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		called = true
		return newMockConn(), nil
	}

	ds := &connect.DialContextSettings{
		DialContext: origDial,
	}

	bw := &ProxyBandwidth{}
	wrapped := WrapDialContextSettings(ds, bw, "proxy-3")

	// Original should be wrapped.
	conn, err := wrapped.DialContext(context.Background(), "tcp", "1.2.3.4:443")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	if !called {
		t.Fatal("original DialContext was not called")
	}

	// The returned conn should be a bandwidth-tracked Conn.
	bwConn, ok := conn.(*Conn)
	if !ok {
		t.Fatalf("expected *Conn, got %T", conn)
	}
	if bwConn.ProxyAddress() != "proxy-3" {
		t.Fatalf("expected proxy address 'proxy-3', got %q", bwConn.ProxyAddress())
	}

	// Write through the wrapper and verify counting.
	data := []byte("test payload")
	_, err = conn.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if bw.TotalTx.Load() != uint64(len(data)) {
		t.Fatalf("TotalTx: expected %d, got %d", len(data), bw.TotalTx.Load())
	}
}

// stubPacketConnFactoryBuilder stands in for a caller's H3 endpoint factory
// constructor (miner/h3_packet_conn.go, provider/h3_packet_conn.go) in the wrap
// tests: a factory for a proxied identity, none for a direct one.
func stubPacketConnFactoryBuilder(proxySettings *connect.ProxySettings, bw *ProxyBandwidth, proxyAddr string) func(context.Context) (net.PacketConn, error) {
	if proxySettings == nil {
		return nil
	}
	return func(context.Context) (net.PacketConn, error) {
		return newMockPacketConn(), nil
	}
}

// A caller's H3/UDP endpoint seam must survive the wrap. The wrap rebuilds the
// settings struct to wrap DialContext, so rebuilding it without this field
// would silently put a proxied identity's QUIC back on the host socket: connect
// opens its own wildcard socket whenever H3PacketConnFactory is nil.
func TestWrapDialContextSettingsCarriesThePacketConnFactory(t *testing.T) {
	packetConn := newMockPacketConn()
	ds := &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return newMockConn(), nil
		},
		PacketConnFactory: func(context.Context) (net.PacketConn, error) {
			return packetConn, nil
		},
	}

	wrapped := WrapDialContextSettings(ds, &ProxyBandwidth{}, "proxy-3")
	if wrapped.PacketConnFactory == nil {
		t.Fatal("the wrap dropped PacketConnFactory, so H3 would fall back to a host socket")
	}
	got, err := wrapped.PacketConnFactory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != net.PacketConn(packetConn) {
		t.Fatalf("the wrap did not carry the caller's factory: got %T", got)
	}
}

// WrapConnectSettings rebuilds DialContextSettings the same way and must carry
// the seam too. A proxy-only ConnectSettings has no seam of its own, so the
// wrap must BUILD one from that proxy: connect's QUIC carriers read
// DialContextSettings.PacketConnFactory and never ProxySettings, so a nil
// there is a wildcard socket on this host under a proxied identity (the api's
// alt-h3 and alt-whodis dialers are two such carriers).
func TestWrapConnectSettingsCarriesThePacketConnFactory(t *testing.T) {
	packetConn := newMockPacketConn()
	cs := connect.ConnectSettings{
		DialContextSettings: &connect.DialContextSettings{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return newMockConn(), nil
			},
			PacketConnFactory: func(context.Context) (net.PacketConn, error) {
				return packetConn, nil
			},
		},
	}
	wrapped := WrapConnectSettings(cs, &ProxyBandwidth{}, "proxy-1", nil)
	if wrapped.DialContextSettings == nil || wrapped.DialContextSettings.PacketConnFactory == nil {
		t.Fatal("the wrap dropped PacketConnFactory from the connect settings")
	}
	if got, err := wrapped.DialContextSettings.PacketConnFactory(context.Background()); err != nil || got != net.PacketConn(packetConn) {
		t.Fatalf("the wrap did not carry the caller's factory: got %T err %v", got, err)
	}

	// The proxy-only case: no factory of its own, so the wrap asks the
	// builder for one. This is the case the api alt-QUIC leak lived in.
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: "127.0.0.1:1"}
	bw := &ProxyBandwidth{}
	var builtFor *connect.ProxySettings
	var builtBw *ProxyBandwidth
	var builtAddr string
	proxyOnly := WrapConnectSettings(connect.ConnectSettings{
		ProxySettings: proxySettings,
	}, bw, "proxy-1", func(settings *connect.ProxySettings, tracker *ProxyBandwidth, proxyAddr string) func(context.Context) (net.PacketConn, error) {
		builtFor = settings
		builtBw = tracker
		builtAddr = proxyAddr
		return func(context.Context) (net.PacketConn, error) {
			return newMockPacketConn(), nil
		}
	})
	if proxyOnly.DialContextSettings == nil {
		t.Fatal("expected DialContextSettings to be set")
	}
	if proxyOnly.DialContextSettings.PacketConnFactory == nil {
		t.Fatal("a proxy-only connect settings got no QUIC endpoint factory, so its alt-h3/alt-whodis dials would leave from the host socket")
	}
	if builtFor != proxyOnly.ProxySettings {
		t.Fatal("the factory was not built from the connect settings' own proxy")
	}
	if builtBw != bw || builtAddr != "proxy-1" {
		t.Fatalf("the factory was built with tracker %p addr %q, want the wrap's %p and \"proxy-1\"", builtBw, builtAddr, bw)
	}
	if got, err := proxyOnly.DialContextSettings.PacketConnFactory(context.Background()); err != nil || got == nil {
		t.Fatalf("the installed factory did not produce an endpoint: got %T err %v", got, err)
	}
}

// TestWrapConnectSettingsPreservesProxyRouting is a regression test for the
// bug where the provider's relay-egress path never counted bytes because
// naively setting DialContextSettings makes connect.ConnectSettings.DialContext
// skip ProxySettings.NewDialContext entirely (see wrap.go doc comment).
//
// It dials through the wrapped settings to an arbitrary unreachable
// destination and asserts the resulting connection error names the PROXY
// address, not the destination address — proving the SOCKS5 proxy dialer
// was actually invoked rather than bypassed for a direct connection.
func TestWrapConnectSettingsPreservesProxyRouting(t *testing.T) {
	// Bind a local listener so dialing succeeds promptly if the proxy is
	// bypassed (proving the bypass path would connect to the destination).
	// The proxy at 127.0.0.1:1 still fails before reaching this listener.
	lc := net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dest := ln.Addr().String()

	cs := connect.ConnectSettings{
		ProxySettings: &connect.ProxySettings{
			Network: "tcp",
			// Port 1 on loopback: nothing listens there, so the dial fails
			// fast and deterministically without touching the network.
			Address: "127.0.0.1:1",
		},
	}

	bw := &ProxyBandwidth{}
	wrapped := WrapConnectSettings(cs, bw, "proxy-1", nil)

	if wrapped.DialContextSettings == nil {
		t.Fatal("expected DialContextSettings to be set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = wrapped.DialContextSettings.DialContext(ctx, "tcp", dest)
	if err == nil {
		t.Fatal("expected dial to fail (nothing listens on 127.0.0.1:1)")
	}
	// A bypassed dial would attempt to connect to the local listener and
	// succeed — proving the proxy hop was skipped. The SOCKS5 dialer
	// instead fails fast against 127.0.0.1:1 and reports that as the
	// underlying cause — proof the proxy hop ran.
	if !strings.Contains(err.Error(), "dial tcp 127.0.0.1:1") {
		t.Fatalf("expected dial error to show a failed TCP connect to the proxy address 127.0.0.1:1 (proof the SOCKS5 dialer ran), got: %v", err)
	}
}

func TestWrapConnectSettingsCountsBytes(t *testing.T) {
	cs := connect.ConnectSettings{
		DialContextSettings: &connect.DialContextSettings{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return newMockConn(), nil
			},
		},
	}

	bw := &ProxyBandwidth{}
	wrapped := WrapConnectSettings(cs, bw, "proxy-2", nil)

	conn, err := wrapped.DialContextSettings.DialContext(context.Background(), "tcp", "1.2.3.4:443")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}

	data := []byte("hello")
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if bw.TotalTx.Load() != uint64(len(data)) {
		t.Fatalf("TotalTx: expected %d, got %d", len(data), bw.TotalTx.Load())
	}
	// Billable is NOT counted at the socket: the wrapper cannot tell which bytes
	// belong to a client contract. It comes from the engine's relay counters.
	if bw.BillableTx.Load() != 0 || bw.BillableRx.Load() != 0 {
		t.Fatalf("the egress wrapper counted billable bytes: tx=%d rx=%d", bw.BillableTx.Load(), bw.BillableRx.Load())
	}
}

// A nil tracker means nothing to count, not nothing to protect. A direct
// identity passes through untouched (wrapping it would replace the engine's own
// dual-stack dial with a plain one); a proxied identity still gets its QUIC
// endpoint, because a nil factory there is a wildcard socket on this host under
// the proxied identity.
func TestWrapConnectSettingsNilBandwidth(t *testing.T) {
	t.Run("a direct identity passes through", func(t *testing.T) {
		cs := connect.ConnectSettings{}
		wrapped := WrapConnectSettings(cs, nil, "proxy-1", func(*connect.ProxySettings, *ProxyBandwidth, string) func(context.Context) (net.PacketConn, error) {
			t.Fatal("the builder was called for a direct identity")
			return nil
		})
		if wrapped.DialContextSettings != nil {
			t.Fatal("expected cs to pass through unchanged when bw is nil and there is no proxy")
		}
		if wrapped.ProxySettings != nil {
			t.Fatal("expected ProxySettings to be unchanged")
		}
	})

	t.Run("a proxied identity keeps its endpoint", func(t *testing.T) {
		cs := connect.ConnectSettings{
			ProxySettings: &connect.ProxySettings{Network: "tcp", Address: "127.0.0.1:1"},
		}
		wrapped := WrapConnectSettings(cs, nil, "proxy-1", func(*connect.ProxySettings, *ProxyBandwidth, string) func(context.Context) (net.PacketConn, error) {
			return func(context.Context) (net.PacketConn, error) {
				return newMockPacketConn(), nil
			}
		})
		if wrapped.DialContextSettings == nil {
			t.Fatal("expected the proxied settings to carry a dial context settings")
		}
		if wrapped.DialContextSettings.PacketConnFactory == nil {
			t.Fatal("a proxied identity with no tracker lost its QUIC endpoint, so its QUIC would leave from the host socket")
		}
		if wrapped.ProxySettings != cs.ProxySettings {
			t.Fatal("expected ProxySettings to be unchanged")
		}
		// The dial is still the proxy's: connect would have used it, so the
		// untracked wrap must not replace it with a plain one.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := wrapped.DialContextSettings.DialContext(ctx, "tcp", "203.0.113.9:443"); err == nil {
			t.Fatal("expected the dial to fail against the unroutable proxy at 127.0.0.1:1")
		} else if !strings.Contains(err.Error(), "127.0.0.1:1") {
			t.Fatalf("the untracked wrap did not keep the proxy dial: %v", err)
		}
	})
}
