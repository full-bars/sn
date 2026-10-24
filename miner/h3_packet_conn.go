package miner

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/provider/bandwidth"
)

// h3_packet_conn.go ports the 3.23-fix v33 provider's H3 packet conn factory
// (provider/h3_packet_conn.go) onto the miner's slot construction.
//
// The platform transport's H3 (QUIC) carriers open a UDP socket. Left alone,
// connect opens that socket on the host, bound to the wildcard and pinned to
// the host's physical egress interface. For the direct identity that is right.
// For a PROXIED identity it is a leak: the identity's QUIC would reach the
// platform from the node's own address instead of through the proxy its TCP
// traffic uses, so the platform sees one identity from two addresses, and the
// proxy the operator pays for never carries (nor is credited for) that path.
//
// The factory is how connect lets a caller own that endpoint: when
// PlatformTransportSettings.H3PacketConnFactory is set, connect calls it once
// per H3 dial and uses the returned net.PacketConn instead of opening a host
// socket. This file supplies the proxied identity's endpoint — the proxy's
// SOCKS5 UDP relay (socks5_udp.go), never a host socket — and leaves the
// direct identity with no factory at all.
//
// The miner's seam for the platform transport is
// DeviceLocalSettings.ProviderDialContextSettings: the SDK copies that struct's
// PacketConnFactory into the provider carrier's
// PlatformTransportSettings.H3PacketConnFactory when it builds the slot
// (urnetwork/sdk device_local.go, newDeviceLocalPlatformTransportSettingsForProfile).
// So providerSlotDialSettings sets the factory on the dial settings the
// bandwidth wrapper is given, and bandwidth.WrapDialContextSettings carries it
// through.
//
// The platform transport is not the only QUIC path. The network space's own
// ConnectSettings also dials QUIC: connect registers the api's alt-h3 and
// alt-whodis carriers for every space unconditionally (net_http.go,
// newAltDialers) and they read their endpoint from
// ConnectSettings.DialContextSettings.PacketConnFactory, not from
// ProxySettings (net_extender.go, openExtenderPacketConn). A proxied slot's
// network space therefore needs the same factory on its ConnectSettings, which
// networkSpaceConnectSettings installs through the wrap.

// newH3PacketConnFactory returns the provider carrier's H3PacketConnFactory for
// one slot, or nil when the slot must keep the engine's own socket.
//
// A proxied identity always gets a factory, even with no bandwidth tracker:
// without one connect opens a socket on the host and the identity's QUIC leaves
// from the node's address instead of its proxy's. The factory relays through
// the proxy and, if the proxy cannot relay UDP, returns an error rather than a
// host socket, so the transport falls back to a TCP carrier that already goes
// through the proxy.
//
// A direct identity gets nil. Port adaptation: the provider returns a factory
// for a direct identity when it has a byte counter to put around a host socket,
// because the provider's direct identity's counter is otherwise absent. The
// miner's direct slot always has a registry counter (bwRegistry.Register(0)),
// so keeping that rule would hand the direct identity a factory — and the
// provider's direct branch opens an UNPINNED wildcard socket, which on a node
// that provides a tunnel can loop the platform's QUIC back into the tunnel the
// node itself serves. The direct identity's H3 bytes are already counted by the
// engine, and its socket must stay the engine's own wildcard-plus-physical-egress
// one, so this function refuses to build a factory for it whatever the tracker
// says.
func newH3PacketConnFactory(proxySettings *connect.ProxySettings, bw *bandwidth.ProxyBandwidth, proxyAddr string) func(context.Context) (net.PacketConn, error) {
	if proxySettings == nil {
		return nil
	}
	return func(ctx context.Context) (net.PacketConn, error) {
		// The grade only saw this proxy's TCP path; before QUIC goes through
		// its UDP relay make sure the relay works and exits from the address
		// TCP does. The verdict is remembered (proxy_udp_check.go), so the
		// check does not run on every dial. An unverifiable relay (unknown) is
		// the proxy either way and is allowed, exactly as the eligibility gate
		// reads it.
		switch result := globalProxyUDPCheck.Check(ctx, proxySettings, time.Now()); result.Status {
		case proxyUDPNoRelay, proxyUDPExitMismatch:
			return nil, fmt.Errorf("h3 not used through proxy %s: udp check %s", proxySettings.Address, result.Status)
		}
		relayed, err := dialSocks5UDP(ctx, proxySettings.Network, proxySettings.Address, proxySettings.Auth)
		if err != nil {
			return nil, fmt.Errorf("h3 through proxy %s: %w", proxySettings.Address, err)
		}
		if bw != nil {
			return bandwidth.NewPacketConn(relayed, bw, proxyAddr), nil
		}
		return relayed, nil
	}
}

// providerSlotDialSettings is the one place a slot's provider carrier dial
// settings are assembled: the bandwidth wrap around the slot's base dial, plus
// the H3 endpoint seam when the slot is proxied.
//
// It exists as a function rather than two statements at the call site so the
// wiring can be asserted without a live socket (see h3_packet_conn_test.go):
// the seam is exactly "does the settings object the SDK is handed carry a
// PacketConnFactory for this slot, or not".
//
// The factory is installed BEFORE the wrap, deliberately. The wrap rebuilds the
// struct to wrap DialContext, so a wrap that did not carry PacketConnFactory
// through would silently drop it here — and the identity's QUIC would go back
// out of the host socket with nothing failing. bandwidth.WrapDialContextSettings
// carries it; a test pins both halves.
//
// A proxied slot's factory replaces any base PacketConnFactory the caller
// supplied (the test-egress seam's bound-source endpoint): the identity has to
// leave through its proxy, and the base seam is for the direct identity. A
// direct slot's base factory is left exactly as the caller set it, which is how
// a test-egress run keeps its bound source address on the H3 carriers too.
func providerSlotDialSettings(base *connect.DialContextSettings, proxySettings *connect.ProxySettings, bw *bandwidth.ProxyBandwidth, proxyAddr string) *connect.DialContextSettings {
	if factory := newH3PacketConnFactory(proxySettings, bw, proxyAddr); factory != nil {
		next := &connect.DialContextSettings{PacketConnFactory: factory}
		if base != nil {
			next.DialContext = base.DialContext
		}
		base = next
	}
	return bandwidth.WrapDialContextSettings(base, bw, proxyAddr)
}

// networkSpaceConnectSettings is the ConnectSettings one slot's network space is
// built from (run.go, before sdk.NewNetworkSpaceWithUrls).
//
// It wraps the slot's dial path for bandwidth accounting as before, and it is
// also where the slot's QUIC endpoint is decided for every non-platform
// carrier. A proxied slot's ConnectSettings has ProxySettings set and no
// DialContextSettings (run.go deliberately leaves the test-egress seam off a
// proxied slot), so the wrap's ProxySettings branch builds the factory from
// that proxy and installs it. Without it the api's alt-h3 and alt-whodis
// dialers would open a host socket and the identity's api request — with its
// jwt — would reach the platform from this node's address while the proxy
// carried none of it.
//
// A direct slot's settings carry no ProxySettings, so no factory is built and
// the engine keeps its own socket, exactly as the direct device path does.
func networkSpaceConnectSettings(cs connect.ConnectSettings, proxySettings *connect.ProxySettings, bw *bandwidth.ProxyBandwidth, proxyAddr string) connect.ConnectSettings {
	// The slot's proxy is the authority for both halves of these settings: the
	// stream dial connect performs from them and the QUIC endpoint installed
	// below. Setting it here means a caller cannot hand the wrap a settings
	// whose ProxySettings was never set and get an unfactored QUIC path back.
	cs.ProxySettings = proxySettings
	// The builder is newH3PacketConnFactory itself: the network space and the
	// provider device of one slot must share one endpoint policy, not two that
	// can drift.
	return bandwidth.WrapConnectSettings(cs, bw, proxyAddr, newH3PacketConnFactory)
}
