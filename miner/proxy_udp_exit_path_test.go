package miner

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/miner/bandwidth"
	"github.com/urnetwork/connect"
)

// The sdk puts a proxied provider's exit on the proxy by setting the exit nat's
// ProxySettings and DialContextSettings to the device's dial seam, which is
// providerSlotDialSettings (sdk applyProviderExitEgress). connect then sends a
// datagram dial with ProxySettings to its packet endpoint (the factory) before
// it reads the injected dialer, so the gates in proxiedUdpDialContext must hold
// on that path too. These tests dial through connect's own DialContext with
// settings built the way the sdk builds them.

func exitNatConnectSettings(t *testing.T, proxySettings *connect.ProxySettings, status proxyUDPStatus) *connect.ConnectSettings {
	t.Helper()
	previous := globalProxyUDPCheck
	globalProxyUDPCheck = stubUdpCheckStore(status)
	t.Cleanup(func() { globalProxyUDPCheck = previous })
	connectSettings := connect.DefaultClientStrategySettings().ConnectSettings
	connectSettings.ProxySettings = proxySettings
	connectSettings.DialContextSettings = providerSlotDialSettings(nil, proxySettings, &bandwidth.ProxyBandwidth{}, proxySettings.Address)
	return &connectSettings
}

func TestExitPathRelaysIpv4UdpThroughTheProxy(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	echo, _ := startEchoUdp(t)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	connectSettings := exitNatConnectSettings(t, proxySettings, proxyUDPOK)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := connectSettings.DialContext(ctx, "udp", echo.LocalAddr().String())
	if err != nil {
		t.Fatalf("udp dial on the exit path: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buffer := make([]byte, 1500)
	n, err := conn.Read(buffer)
	if err != nil || string(buffer[:n]) != "ping" {
		t.Fatalf("read = %q, %v, want the echo through the relay", buffer[:n], err)
	}
	if got := server.associations.Load(); got != 1 {
		t.Fatalf("open associations = %d, want 1", got)
	}
}

func TestExitPathRefusesIpv6UdpBeforeTheProxyIsContacted(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	connectSettings := exitNatConnectSettings(t, proxySettings, proxyUDPOK)

	conn, err := connectSettings.DialContext(context.Background(), "udp", "[2001:db8::1]:443")
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, errProxiedUdpIpv6Refused) {
		t.Fatalf("err = %v, want the ipv6 refusal", err)
	}
	if proxiedUdpRefusals.count(proxySettings, proxiedUdpRefusalIpv6) != 1 {
		t.Fatal("the ipv6 refusal was not counted")
	}
	if got := server.accepts.Load(); got != 0 {
		t.Fatalf("an ipv6 flow contacted the proxy %d times", got)
	}
}

func TestExitPathRefusesUdpWhenTheProxyHasNoRelay(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	echo, _ := startEchoUdp(t)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	connectSettings := exitNatConnectSettings(t, proxySettings, proxyUDPNoRelay)

	conn, err := connectSettings.DialContext(context.Background(), "udp", echo.LocalAddr().String())
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, errProxiedUdpNoRelay) {
		t.Fatalf("err = %v, want the no relay refusal", err)
	}
	if proxiedUdpRefusals.count(proxySettings, proxiedUdpRefusalNoRelay) != 1 {
		t.Fatal("the no relay refusal was not counted")
	}
	if got := server.accepts.Load(); got != 0 {
		t.Fatalf("a refused flow contacted the proxy %d times", got)
	}
}

func TestExitPathRefusesUdpWhenTheProxyExitsFromAnotherAddress(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	echo, _ := startEchoUdp(t)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	connectSettings := exitNatConnectSettings(t, proxySettings, proxyUDPExitMismatch)

	conn, err := connectSettings.DialContext(context.Background(), "udp", echo.LocalAddr().String())
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, errProxiedUdpExitMismatch) {
		t.Fatalf("err = %v, want the exit mismatch refusal", err)
	}
	if proxiedUdpRefusals.count(proxySettings, proxiedUdpRefusalExitMismatch) != 1 {
		t.Fatal("the exit mismatch refusal was not counted")
	}
}

// Tcp on the exit path still goes through the proxy once, and reaches the
// destination: the injected dialer already carries the proxy, so connect must
// not wrap it in the proxy a second time.
func TestExitPathTcpGoesThroughTheProxyOnce(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	server.tcpExitBody = "198.51.100.7"
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	connectSettings := exitNatConnectSettings(t, proxySettings, proxyUDPOK)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := connectSettings.DialContext(ctx, "tcp", "192.0.2.1:80")
	if err != nil {
		t.Fatalf("tcp dial on the exit path: %v", err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	response, _ := io.ReadAll(conn)
	conn.Close()
	if !strings.Contains(string(response), "198.51.100.7") {
		t.Fatalf("response = %q, the dial did not reach the destination through the proxy", response)
	}
	if got := server.accepts.Load(); got != 1 {
		t.Fatalf("the proxy saw %d control connections for one tcp dial, want 1", got)
	}
}
