package provider

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/provider/bandwidth"
)

// newH3PacketConnFactory returns the PlatformTransportSettings.H3PacketConnFactory
// for an identity, or nil when the engine's default socket is right.
//
// A proxied identity (proxySettings != nil) always gets a factory, even with no
// bandwidth tracker: without one the engine opens a socket on the host and the
// identity's QUIC would leave from the host's address instead of its proxy's.
// The factory relays through the proxy and, if the proxy cannot, returns an
// error rather than a host socket, so the platform transport falls back to a
// TCP mode that already goes through the proxy.
//
// A direct identity keeps the engine's default socket (and its physical-egress
// binding) unless there is a byte counter to put around a plain one.
func newH3PacketConnFactory(proxySettings *connect.ProxySettings, bw *bandwidth.ProxyBandwidth, identityKey string) func(context.Context) (net.PacketConn, error) {
	if proxySettings == nil && bw == nil {
		return nil
	}
	return func(ctx context.Context) (net.PacketConn, error) {
		var pc net.PacketConn
		if proxySettings != nil {
			// the grade only saw this proxy's TCP path; before QUIC goes
			// through its UDP relay make sure the relay works and exits from
			// the address TCP does
			switch result := globalProxyUDPCheck.Check(ctx, proxySettings, time.Now()); result.Status {
			case proxyUDPNoRelay, proxyUDPExitMismatch:
				return nil, fmt.Errorf("h3 not used through proxy %s: udp check %s", proxySettings.Address, result.Status)
			}
			relayed, err := dialSocks5UDP(ctx, proxySettings.Network, proxySettings.Address, proxySettings.Auth)
			if err != nil {
				return nil, fmt.Errorf("h3 through proxy %s: %w", proxySettings.Address, err)
			}
			pc = relayed
		} else {
			raw, err := listenDirectH3Udp(connect.PinnedIpFamilyFromContext(ctx))
			if err != nil {
				return nil, err
			}
			pc = raw
		}
		if bw != nil {
			return bandwidth.NewPacketConn(pc, bw, identityKey), nil
		}
		return pc, nil
	}
}

// listenDirectH3Udp opens the direct identity's QUIC socket. A transport pinned
// to IPv6 by the dual-stack group needs a v6 socket and one pinned to IPv4 a v4
// socket; an unpinned transport keeps the socket this factory has always opened
// ("udp" with the v4 wildcard, which Go binds dual-stack). The
// family is explicit rather than a dual-stack wildcard because a v6 wildcard is
// v6-only by default on some hosts (FreeBSD).
func listenDirectH3Udp(pinnedIpFamily int) (*net.UDPConn, error) {
	switch pinnedIpFamily {
	case 6:
		return net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified, Port: 0})
	case 4:
		// "udp" with the v4 wildcard would bind a dual-stack [::] socket
		return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	}
	return net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
}
