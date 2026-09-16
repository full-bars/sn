package bandwidth

import (
	"context"
	"net"
	"testing"

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
