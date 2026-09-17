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
	// NOTE: PacketConnFactory check removed — field not in pinned full-bars/connect.
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

// NOTE: PacketConnFactory tests removed — the field does not exist in the pinned
// full-bars/connect version (4c85408). See wrap.go TODO for re-enablement.

// TestWrapNilPacketConnFactoryCreatesDefault removed — PacketConnFactory not available
// in pinned full-bars/connect version. See wrap.go TODO.

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
	wrapped := WrapConnectSettings(cs, bw, "proxy-1")

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
	wrapped := WrapConnectSettings(cs, bw, "proxy-2")

	conn, err := wrapped.DialContextSettings.DialContext(context.Background(), "tcp", "1.2.3.4:443")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}

	data := []byte("hello")
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if bw.BillableTx.Load() != uint64(len(data)) {
		t.Fatalf("BillableTx: expected %d, got %d", len(data), bw.BillableTx.Load())
	}
}

func TestWrapConnectSettingsNilBandwidthIsNoop(t *testing.T) {
	cs := connect.ConnectSettings{
		ProxySettings: &connect.ProxySettings{Network: "tcp", Address: "127.0.0.1:1"},
	}
	wrapped := WrapConnectSettings(cs, nil, "proxy-1")
	if wrapped.DialContextSettings != nil {
		t.Fatal("expected cs to pass through unchanged when bw is nil")
	}
	if wrapped.ProxySettings != cs.ProxySettings {
		t.Fatal("expected ProxySettings to be unchanged")
	}
}
