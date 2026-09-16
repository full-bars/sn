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
	if wrapped.PacketConnFactory == nil {
		t.Fatal("expected non-nil PacketConnFactory")
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

func TestWrapPacketConnFactory(t *testing.T) {
	factoryCalled := false
	origFactory := func(ctx context.Context) (net.PacketConn, error) {
		factoryCalled = true
		return newMockPacketConn(), nil
	}

	ds := &connect.DialContextSettings{
		PacketConnFactory: origFactory,
	}

	bw := &ProxyBandwidth{}
	wrapped := WrapDialContextSettings(ds, bw, "proxy-4")

	pc, err := wrapped.PacketConnFactory(context.Background())
	if err != nil {
		t.Fatalf("PacketConnFactory: %v", err)
	}
	if !factoryCalled {
		t.Fatal("original factory was not called")
	}

	bwPC, ok := pc.(*PacketConn)
	if !ok {
		t.Fatalf("expected *PacketConn, got %T", pc)
	}
	if bwPC.ProxyAddress() != "proxy-4" {
		t.Fatalf("expected proxy address 'proxy-4', got %q", bwPC.ProxyAddress())
	}
}

func TestWrapNilPacketConnFactoryCreatesDefault(t *testing.T) {
	ds := &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return newMockConn(), nil
		},
		// PacketConnFactory is nil — should get a default UDP socket.
	}

	bw := &ProxyBandwidth{}
	wrapped := WrapDialContextSettings(ds, bw, "proxy-5")

	pc, err := wrapped.PacketConnFactory(context.Background())
	if err != nil {
		t.Fatalf("PacketConnFactory: %v", err)
	}
	if pc == nil {
		t.Fatal("expected non-nil PacketConn")
	}
	pc.Close()
}
