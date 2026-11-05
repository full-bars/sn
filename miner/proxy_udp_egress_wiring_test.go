package miner

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/miner/bandwidth"
)

// In this tree the exit nat is built by the SDK, which hands it the slot's
// ProviderDialContextSettings for both its tcp and udp sockets. So the seam
// that matters is providerSlotDialSettings: a udp dial through the settings a
// proxied slot is built with must open an association on the proxy and round
// trip through its relay, and the bytes must reach the slot's counters.
func TestProxiedSlotDialSettingsCarryClientUdpThroughTheRelay(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	echo, seen := startEchoUdp(t)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	stubH3UDPVerdict(t, proxySettings, proxyUDPOK)
	bw := &bandwidth.ProxyBandwidth{}

	settings := providerSlotDialSettings(nil, proxySettings, bw, server.address())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := settings.DialContext(ctx, "udp", echo.LocalAddr().String())
	if err != nil {
		t.Fatalf("udp dial through the proxied slot: %v", err)
	}
	relayPort := server.waitRelayPort(t)
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case from := <-seen:
		if from.Port != relayPort {
			t.Fatalf("the destination saw the datagram from port %d, not the relay's %d", from.Port, relayPort)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the datagram never reached the destination")
	}
	buf := make([]byte, 64)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("read = %q, %v", buf[:n], err)
	}
	if bw.TotalTx.Load() == 0 || bw.TotalRx.Load() == 0 {
		t.Fatalf("slot counters tx/rx = %d/%d, want both counted", bw.TotalTx.Load(), bw.TotalRx.Load())
	}
	conn.Close()
	waitForAssociations(t, server, 0)
}

// A proxy whose udp check says no relay refuses the flow before any connection
// to the proxy, and never falls back to a socket on the host.
func TestProxiedSlotDialSettingsRefuseClientUdpWithoutARelay(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	echo, _ := startEchoUdp(t)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	stubH3UDPVerdict(t, proxySettings, proxyUDPNoRelay)

	settings := providerSlotDialSettings(nil, proxySettings, &bandwidth.ProxyBandwidth{}, server.address())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if conn, err := settings.DialContext(ctx, "udp", echo.LocalAddr().String()); err == nil {
		conn.Close()
		t.Fatalf("a slot with no udp relay carried a client udp flow")
	}
	if got := server.associations.Load(); got != 0 {
		t.Fatalf("a refused flow opened %d associations", got)
	}
}
