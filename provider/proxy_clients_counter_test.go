package provider

import (
	"context"
	"net"
	"testing"

	"github.com/urfoundation/sn/provider/bandwidth"
	"github.com/urnetwork/connect"
)

// Per-proxy Clients was never incremented: the engine's NAT no longer takes
// a bandwidth tracker, so the [profit] earning flag, [traffic] start/stop
// markers, serving/idle counts and urnet_clients_active all read 0 while
// billable bytes flowed. The relay-egress dial wrapper provideWithProxy
// installs now counts each live egress connection as a client session.
func TestRelayEgressDial_DrivesActiveClientCounts(t *testing.T) {
	ResetProxyHealthForTesting()
	t.Cleanup(ResetProxyHealthForTesting)
	const key = "10.2.2.2:1080"
	RegisterProxy(21, "10.2.2.2:1080", key)
	proxyBandwidth := RegisterProxyBandwidth(21)

	base := connect.ConnectSettings{
		DialContextSettings: &connect.DialContextSettings{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, _ := net.Pipe()
				return c, nil
			},
		},
	}
	// Same wrapping provideWithProxy applies to the LocalUserNat settings.
	relay := bandwidth.WrapConnectSettings(base, proxyBandwidth, key)

	a, err := relay.DialContextSettings.DialContext(context.Background(), "tcp", "1.2.3.4:443")
	if err != nil {
		t.Fatal(err)
	}
	b, err := relay.DialContextSettings.DialContext(context.Background(), "udp", "1.2.3.4:53")
	if err != nil {
		t.Fatal(err)
	}
	if got := activeConnectionCount(); got != 2 {
		t.Fatalf("activeConnectionCount = %d, want 2", got)
	}
	if got := activeProxyConnections(); got != 1 {
		t.Fatalf("activeProxyConnections = %d, want 1", got)
	}

	a.Close()
	b.Close()
	if got := activeConnectionCount(); got != 0 {
		t.Fatalf("activeConnectionCount after close = %d, want 0", got)
	}
	if got := activeProxyConnections(); got != 0 {
		t.Fatalf("activeProxyConnections after close = %d, want 0", got)
	}
}
