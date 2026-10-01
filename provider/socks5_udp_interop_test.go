package provider

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	upstreamproxy "github.com/urnetwork/proxy"
)

// Interop: our SOCKS5 UDP client (dialSocks5UDP) against the SOCKS5 server the
// URnetwork proxy package ships, which is an independent implementation written
// from the RFC. The unit tests in socks5_udp_test.go use a fake relay written
// by the same hand as the client, so they can only prove the two agree with
// each other. This proves the client works against a server it did not grow up
// with: the handshake, the relay address rule (proxy IP plus the reported
// port), the datagram header in both directions, and the sizes QUIC sends.

// startUpstreamSocks5 runs the upstream server on a loopback port, dialing
// every UDP flow and TCP connect directly. validUser nil means no auth.
func startUpstreamSocks5(t *testing.T, validUser func(user, password, userAddr string) bool) (address string, server *upstreamproxy.SocksProxy) {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address = probe.Addr().String()
	probe.Close()

	server = upstreamproxy.NewSocksProxyWithDefaults()
	server.ValidUser = validUser
	server.ConnectDialWithRequest = func(ctx context.Context, _ upstreamproxy.SocksRequest, network string, addr string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, addr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.ListenAndServe(ctx, "tcp", address)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	// wait until it accepts
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("tcp", address)
		if err == nil {
			conn.Close()
			return address, server
		}
		if time.Now().After(deadline) {
			t.Fatalf("the upstream socks5 server never started on %s: %v", address, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startUdpEchoServer echoes every datagram back to its sender.
func startUdpEchoServer(t *testing.T) *net.UDPAddr {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buffer := make([]byte, 65535)
		for {
			n, from, err := conn.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			conn.WriteToUDP(buffer[:n], from)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr)
}

func echoOnce(t *testing.T, pc net.PacketConn, echo *net.UDPAddr, payload []byte) {
	t.Helper()
	if _, err := pc.WriteTo(payload, echo); err != nil {
		t.Fatalf("write %d bytes: %v", len(payload), err)
	}
	pc.SetReadDeadline(time.Now().Add(3 * time.Second))
	buffer := make([]byte, 65535)
	n, from, err := pc.ReadFrom(buffer)
	if err != nil {
		t.Fatalf("read the echo of %d bytes: %v", len(payload), err)
	}
	if udp, ok := from.(*net.UDPAddr); !ok || !udp.IP.Equal(echo.IP) || udp.Port != echo.Port {
		t.Fatalf("echo came from %v, want %v", from, echo)
	}
	if !bytes.Equal(buffer[:n], payload) {
		t.Fatalf("echo of %d bytes came back as %d different bytes", len(payload), n)
	}
}

func TestSocks5UdpInteropRoundTripAgainstUpstreamServer(t *testing.T) {
	address, _ := startUpstreamSocks5(t, nil)
	echo := startUdpEchoServer(t)

	pc, err := dialSocks5UDP(context.Background(), "tcp", address, nil)
	if err != nil {
		t.Fatalf("the upstream server refused our UDP ASSOCIATE: %v", err)
	}
	defer pc.Close()

	// The sizes QUIC actually sends: a 1200 byte initial, the 1280 floor, a
	// full path-MTU packet, and tiny acks. All must survive both headers.
	for _, size := range []int{1, 40, 1200, 1280, 1452} {
		payload := bytes.Repeat([]byte{byte(size)}, size)
		echoOnce(t, pc, echo, payload)
	}
}

func TestSocks5UdpInteropAuthenticatesAgainstUpstreamServer(t *testing.T) {
	address, _ := startUpstreamSocks5(t, func(user, password, _ string) bool {
		return user == "identity-7" && password == "s3cret"
	})
	echo := startUdpEchoServer(t)

	pc, err := dialSocks5UDP(context.Background(), "tcp", address, &proxy.Auth{User: "identity-7", Password: "s3cret"})
	if err != nil {
		t.Fatalf("valid credentials were refused by the upstream server: %v", err)
	}
	defer pc.Close()
	echoOnce(t, pc, echo, []byte("through an authenticated association"))

	if bad, err := dialSocks5UDP(context.Background(), "tcp", address, &proxy.Auth{User: "identity-7", Password: "wrong"}); err == nil {
		bad.Close()
		t.Fatal("a wrong password was accepted")
	}
	if none, err := dialSocks5UDP(context.Background(), "tcp", address, nil); err == nil {
		none.Close()
		t.Fatal("an association with no credentials was accepted by a server that requires them")
	}
}

// Many datagrams on one association, the way a QUIC connection uses it, with a
// reply read for each: nothing may be lost or reordered on loopback.
func TestSocks5UdpInteropSustainedExchange(t *testing.T) {
	address, _ := startUpstreamSocks5(t, nil)
	echo := startUdpEchoServer(t)
	pc, err := dialSocks5UDP(context.Background(), "tcp", address, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	for i := 0; i < 500; i++ {
		payload := bytes.Repeat([]byte{byte(i)}, 100+i%1200)
		echoOnce(t, pc, echo, payload)
	}
}

// Closing our packet conn closes the control connection, and the upstream
// server must then release the association. A leak here is a socket and a
// goroutine per QUIC connection for the life of the provider.
func TestSocks5UdpInteropCloseReleasesTheUpstreamAssociation(t *testing.T) {
	address, server := startUpstreamSocks5(t, nil)
	echo := startUdpEchoServer(t)

	pc, err := dialSocks5UDP(context.Background(), "tcp", address, nil)
	if err != nil {
		t.Fatal(err)
	}
	echoOnce(t, pc, echo, []byte("open"))
	if server.ActiveCount() == 0 {
		t.Fatal("the server reports no active association while one is open")
	}
	pc.Close()

	deadline := time.Now().Add(5 * time.Second)
	for server.ActiveCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the server still holds %d connection(s) 5s after our close", server.ActiveCount())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A datagram larger than the upstream server's MaxDatagramSize is dropped, not
// truncated. QUIC never sends one that big, so this pins what the limit does to
// us rather than asserting we cross it: the next normal datagram must still
// work, and the oversize one must not come back mangled.
func TestSocks5UdpInteropOversizeDatagramIsDroppedNotMangled(t *testing.T) {
	address, server := startUpstreamSocks5(t, nil)
	server.Settings().MaxDatagramSize = 1500
	echo := startUdpEchoServer(t)
	pc, err := dialSocks5UDP(context.Background(), "tcp", address, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	pc.WriteTo(bytes.Repeat([]byte{0xAB}, 3000), echo)
	pc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buffer := make([]byte, 65535)
	if n, _, err := pc.ReadFrom(buffer); err == nil {
		t.Fatalf("an oversize datagram came back (%d bytes) instead of being dropped", n)
	}
	echoOnce(t, pc, echo, []byte("still works after the drop"))
}
