package bandwidth

import (
	"context"
	"net"

	"github.com/urnetwork/connect"
)

// WrapDialContextSettings wraps an existing (or nil) DialContextSettings
// with bandwidth-tracking Conn and PacketConn wrappers. If ds is nil,
// a default direct-dial settings is created and then wrapped.
//
// The returned settings can be assigned directly to
// sdk.DeviceLocalSettings.ProviderDialContextSettings.
//
// Usage:
//
//	registry := bandwidth.NewRegistry()
//	bw := registry.Register(proxyIndex)
//	settings.ProviderDialContextSettings = bandwidth.WrapDialContextSettings(
//	    settings.ProviderDialContextSettings, bw, proxyAddr,
//	)
func WrapDialContextSettings(ds *connect.DialContextSettings, bw *ProxyBandwidth, proxyAddr string) *connect.DialContextSettings {
	if ds == nil {
		ds = &connect.DialContextSettings{}
	}

	// Capture the original DialContext (or build a default direct dialer).
	origDial := ds.DialContext
	if origDial == nil {
		dialer := &net.Dialer{}
		origDial = dialer.DialContext
	}

	// Wrap TCP path: count bytes on every stream connection.
	wrappedDial := func(ctx context.Context, network string, addr string) (net.Conn, error) {
		conn, err := origDial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return NewConn(conn, bw, proxyAddr), nil
	}

	// Wrap UDP path: count bytes on every QUIC endpoint.
	origFactory := ds.PacketConnFactory
	wrappedFactory := func(ctx context.Context) (net.PacketConn, error) {
		if origFactory != nil {
			pc, err := origFactory(ctx)
			if err != nil {
				return nil, err
			}
			return NewPacketConn(pc, bw, proxyAddr), nil
		}
		// Default: create a wildcard UDP socket.
		pc, err := net.ListenPacket("udp4", ":0")
		if err != nil {
			return nil, err
		}
		return NewPacketConn(pc, bw, proxyAddr), nil
	}

	return &connect.DialContextSettings{
		DialContext:       wrappedDial,
		PacketConnFactory: wrappedFactory,
	}
}
