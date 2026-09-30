package provider

import (
	"context"
	"fmt"
	"net"

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
			relayed, err := dialSocks5UDP(ctx, proxySettings.Network, proxySettings.Address, proxySettings.Auth)
			if err != nil {
				return nil, fmt.Errorf("h3 through proxy %s: %w", proxySettings.Address, err)
			}
			pc = relayed
		} else {
			raw, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
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
