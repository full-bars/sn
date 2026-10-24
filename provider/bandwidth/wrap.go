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

	// NOTE: UDP/QUIC wrapping of the platform tunnel is NOT done here: a
	// PlatformTransportSettings.H3PacketConnFactory wired at the provider
	// level (see provide.go) is the mechanism. What this wrapper must do is
	// carry that factory through unchanged. A caller that sets
	// PacketConnFactory on the settings it passes in is asking for its H3/UDP
	// endpoint to survive the wrap; dropping it here would silently send that
	// traffic out of the engine's own host socket instead. That wrapper feeds
	// the TOTAL counters only. Billable is not counted on any socket: it comes
	// from the remote provider's relay accounting.
	return &connect.DialContextSettings{
		DialContext:       wrappedDial,
		PacketConnFactory: ds.PacketConnFactory,
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
// A proxied ConnectSettings comes back carrying the H3/QUIC endpoint factory
// that build makes from that proxy's settings. connect consults ProxySettings
// for the stream dial only: its QUIC carriers read
// DialContextSettings.PacketConnFactory instead, so without the factory the
// identity's QUIC (the api's alt-h3 and alt-whodis dialers among them) opens a
// socket on this host. See wrapConnectSettings.
//
// Returns cs unchanged when there is neither a tracker nor a proxied identity
// to install a factory for.
func WrapConnectSettings(cs connect.ConnectSettings, bw *ProxyBandwidth, proxyAddr string, build PacketConnFactoryBuilder) connect.ConnectSettings {
	return wrapConnectSettings(cs, bw, proxyAddr, true, build)
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
func WrapConnectSettingsTotal(cs connect.ConnectSettings, bw *ProxyBandwidth, proxyAddr string, build PacketConnFactoryBuilder) connect.ConnectSettings {
	if cs.ProxySettings == nil && cs.DialContextSettings == nil {
		return cs
	}
	return wrapConnectSettings(cs, bw, proxyAddr, false, build)
}

// PacketConnFactoryBuilder builds the unconnected UDP endpoint factory for one
// proxied identity: the seam connect uses instead of opening a host socket for
// its QUIC carriers. Both callers pass their own newH3PacketConnFactory
// (miner/h3_packet_conn.go, provider/h3_packet_conn.go) — this package cannot
// build it, because the factory relays through the proxy's SOCKS5 UDP relay
// and reads the proxy UDP verdict, which live above it.
//
// Nil proxy settings must return nil: a direct identity keeps the engine's own
// socket, and a factory there would replace it with an unpinned wildcard one
// (see miner/h3_packet_conn.go). A nil bw is allowed to yield an untracked
// endpoint: the proxy route is the point, the counter is not.
//
// The wrap itself tolerates a nil builder, but a nil builder leaves a proxied
// setting's QUIC path on the host socket, so every caller here passes one.
type PacketConnFactoryBuilder func(proxySettings *connect.ProxySettings, bw *ProxyBandwidth, proxyAddr string) func(context.Context) (net.PacketConn, error)

func wrapConnectSettings(cs connect.ConnectSettings, bw *ProxyBandwidth, proxyAddr string, session bool, build PacketConnFactoryBuilder) connect.ConnectSettings {
	var baseDial connect.DialContextFunction
	// The H3/UDP endpoint seam is not a dial and is not wrapped here, but it
	// must survive, and a proxied identity has none of its own to supply: a
	// caller that set one is asking for its QUIC traffic to go out of its own
	// endpoint, and rebuilding these settings without one would put that
	// traffic back on the engine's host socket.
	var packetConnFactory func(context.Context) (net.PacketConn, error)
	switch {
	case cs.DialContextSettings != nil:
		baseDial = cs.DialContextSettings.DialContext
		packetConnFactory = cs.DialContextSettings.PacketConnFactory
	case cs.ProxySettings != nil:
		baseDial = cs.ProxySettings.NewDialContext(context.Background(), cs.NetDialer())
		// connect's stream dial honors ProxySettings; its QUIC carriers do
		// not. openExtenderPacketConn reads DialContextSettings
		// .PacketConnFactory and, when that is nil, opens net.ListenUDP on the
		// host wildcard (connect net_extender.go). The api's alt-h3 and
		// alt-whodis dialers are registered for every network space
		// (net_http.go, newAltDialers, no ProxySettings gate) and dial
		// through exactly this settings, so a proxied identity without the
		// seam here sends its api request, and the jwt that rides it, from
		// this node's address while the proxy carries none of it.
		//
		// The builder is the caller's own H3 factory constructor, so this is
		// the same endpoint the provider carrier uses. bw goes to it as it
		// is, tracker or not: the factory relays through the proxy either
		// way, and only the accounting is missing when bw is nil.
		if build != nil {
			packetConnFactory = build(cs.ProxySettings, bw, proxyAddr)
		}
	default:
		baseDial = cs.NetDialer().DialContext
	}

	// Nothing to count into. A proxied identity still gets the seam below:
	// returning here would not mean "no instrumentation", it would mean the
	// identity's QUIC going back out of a host socket.
	if bw == nil && packetConnFactory == nil {
		return cs
	}
	if bw == nil {
		// Keep the dial connect would have used (the proxy's, for a proxied
		// identity) and install the seam alone.
		cs.DialContextSettings = &connect.DialContextSettings{
			DialContext:       baseDial,
			PacketConnFactory: packetConnFactory,
		}
		return cs
	}

	cs.DialContextSettings = &connect.DialContextSettings{
		PacketConnFactory: packetConnFactory,
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
