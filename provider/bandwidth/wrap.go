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

	// NOTE: UDP/QUIC bandwidth wrapping is handled at the provider level
	// via PlatformTransportSettings.H3PacketConnFactory (see provide.go),
	// not here, because DialContextSettings bypasses the proxy dialer.
	return &connect.DialContextSettings{
		DialContext: wrappedDial,
	}
}

// WrapConnectSettings returns a copy of cs with its dial path instrumented
// for bandwidth tracking, WITHOUT bypassing cs.ProxySettings the way naively
// setting cs.DialContextSettings does.
//
// connect.ConnectSettings.DialContext (net.go) only consults ProxySettings
// when DialContextSettings is nil — setting DialContextSettings short-
// circuits the SOCKS5 proxy dial entirely and falls through to a direct
// connection. This is what made WrapDialContextSettings unsafe to use on the
// provider's actual proxy-relay path (see the removed DESIGN ADAPTATION
// comment at the provider/provide.go call site).
//
// This function instead captures whatever dial cs would otherwise perform
// (proxy SOCKS5 dial via ProxySettings.NewDialContext, an existing
// DialContextSettings, or a plain net.Dialer) as the base dial, then installs
// a DialContextSettings that calls that base dial and wraps the resulting
// net.Conn with byte counting. The proxy route is preserved; only the
// dial-function seam changes.
//
// Returns cs unchanged if bw is nil.
func WrapConnectSettings(cs connect.ConnectSettings, bw *ProxyBandwidth, proxyAddr string) connect.ConnectSettings {
	if bw == nil {
		return cs
	}

	var baseDial connect.DialContextFunction
	switch {
	case cs.DialContextSettings != nil:
		baseDial = cs.DialContextSettings.DialContext
	case cs.ProxySettings != nil:
		baseDial = cs.ProxySettings.NewDialContext(context.Background(), cs.NetDialer())
	default:
		baseDial = cs.NetDialer().DialContext
	}

	cs.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			conn, err := baseDial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return NewConn(conn, bw, proxyAddr), nil
		},
	}
	return cs
}
