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

	// NOTE: PacketConnFactory (UDP/QUIC bandwidth wrapping) is omitted here because
	// the pinned full-bars/connect version (4c85408, 2026-08-15) does not expose it.
	// The field exists in newer connect (d159f46+) but full-bars/connect needs updating.
	// TCP bandwidth tracking via wrappedDial above covers the primary data path.
	// TODO: Re-enable PacketConnFactory wrapping once full-bars/connect is updated to d159f46+.
	return &connect.DialContextSettings{
		DialContext: wrappedDial,
	}
}
