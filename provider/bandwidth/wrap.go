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

	// NOTE: UDP/QUIC wrapping of the platform tunnel is handled at the provider
	// level via PlatformTransportSettings.H3PacketConnFactory (see provide.go),
	// not here, because DialContextSettings bypasses the proxy dialer. That
	// wrapper feeds the TOTAL counters only. Billable is not counted on any
	// socket: it comes from the remote provider's relay accounting.
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
// Every connection it dials is also a live client session on bw (see
// NewSessionConn), which is what drives bw.Clients: the engine's NAT no longer
// takes a tracker, so this dial is the only place sn sees client sessions
// begin and end. Note this counts live egress connections, not distinct
// client sources, so one client with several flows counts several times.
//
// Returns cs unchanged if bw is nil.
func WrapConnectSettings(cs connect.ConnectSettings, bw *ProxyBandwidth, proxyAddr string) connect.ConnectSettings {
	return wrapConnectSettings(cs, bw, proxyAddr, true)
}

// WrapConnectSettingsTotal is WrapConnectSettings for the provider's OWN
// connections to the platform (the API, auth and the H1 tunnel), which are not
// client sessions: it counts their bytes into the TOTAL only and never into
// Clients or billable. With it Total is every byte on the sockets that go
// through the proxy (relay egress, control plane, H1 tunnel and, through the H3
// factory, the H3 tunnel), the same quantity the 3.23-fix line reports.
//
// Use it on a copy made AFTER the relay copy: the relay copy is built from the
// settings it is given, and wrapping first would count relay bytes twice.
//
// An identity without a proxy is returned unchanged. Installing a dial context
// there would replace the engine's own dual-stack address race with a plain
// dial, which is not a trade worth making for a counter.
func WrapConnectSettingsTotal(cs connect.ConnectSettings, bw *ProxyBandwidth, proxyAddr string) connect.ConnectSettings {
	if cs.ProxySettings == nil && cs.DialContextSettings == nil {
		return cs
	}
	return wrapConnectSettings(cs, bw, proxyAddr, false)
}

func wrapConnectSettings(cs connect.ConnectSettings, bw *ProxyBandwidth, proxyAddr string, session bool) connect.ConnectSettings {
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
			if !session {
				return NewConn(conn, bw, proxyAddr), nil
			}
			// Each relay-egress connection carries a client's flow, so it
			// counts as a live client session for this proxy.
			return NewSessionConn(conn, bw, proxyAddr), nil
		},
	}
	return cs
}
