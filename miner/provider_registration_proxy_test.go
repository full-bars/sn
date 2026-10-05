//go:build linux || darwin

// A synthetic SOCKS5 relay lets the provider tests configure real, reachable
// proxies. The slots fixture previously used unroutable addresses; with dials
// correctly routed through ProxySettings those can no longer carry a flow, so
// the tests relay through this local stand-in instead.
package miner

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect"
)

// newSyntheticSocksRelay starts a minimal SOCKS5 relay on loopback and returns
// its address plus a count of completed CONNECT handshakes. Both the no-auth
// and the user/pass greeting are accepted; the relay forwards to the target
// the client asks for.
func newSyntheticSocksRelay(t *testing.T) (string, *int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	relayed := new(int64)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				serveSyntheticSocksConnect(conn, relayed)
			}(conn)
		}
	}()
	return listener.Addr().String(), relayed
}

// serveSyntheticSocksConnect performs one SOCKS5 greeting, CONNECT and relay.
// It records a completed handshake only once the CONNECT reply is written.
func serveSyntheticSocksConnect(conn net.Conn, relayed *int64) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 5 {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	noAuth, userPass := false, false
	for _, method := range methods {
		switch method {
		case 0x00:
			noAuth = true
		case 0x02:
			userPass = true
		}
	}
	switch {
	case noAuth:
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			return
		}
	case userPass:
		if _, err := conn.Write([]byte{5, 2}); err != nil {
			return
		}
		authHead := make([]byte, 2)
		if _, err := io.ReadFull(conn, authHead); err != nil || authHead[0] != 1 {
			return
		}
		if _, err := io.ReadFull(conn, make([]byte, int(authHead[1]))); err != nil {
			return
		}
		plen := make([]byte, 1)
		if _, err := io.ReadFull(conn, plen); err != nil {
			return
		}
		if _, err := io.ReadFull(conn, make([]byte, int(plen[0]))); err != nil {
			return
		}
		if _, err := conn.Write([]byte{1, 0}); err != nil {
			return
		}
	default:
		_, _ = conn.Write([]byte{5, 0xff})
		return
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil || request[0] != 5 || request[1] != 1 {
		return
	}
	var host string
	switch request[3] {
	case 1:
		address := make([]byte, 4)
		if _, err := io.ReadFull(conn, address); err != nil {
			return
		}
		host = net.IP(address).String()
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return
		}
		name := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, name); err != nil {
			return
		}
		host = string(name)
	case 4:
		address := make([]byte, 16)
		if _, err := io.ReadFull(conn, address); err != nil {
			return
		}
		host = net.IP(address).String()
	default:
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return
	}
	target := net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(port)))
	upstream, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = conn.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	atomic.AddInt64(relayed, 1)
	go func() {
		io.Copy(upstream, conn)
		upstream.(*net.TCPConn).CloseWrite()
	}()
	io.Copy(conn, upstream)
}

// The provider's own strategy dials must pass through a configured proxy; the
// bandwidth wrap must not short-circuit ProxySettings (bandwidth.WrapConnectSettings).
func TestProviderStrategyDialsGoThroughTheProxy(t *testing.T) {
	fixture := newProviderRegistrationFixture(t)
	relayAddress, relayed := newSyntheticSocksRelay(t)
	member := &connect.ProxySettings{Network: "tcp", Address: relayAddress}
	stop := errors.New("synthetic proxied authentication handoff")
	err := fixture.run(t.Context(), true, providerRegistrationHooks{afterAuthenticated: func(string, connect.Id, []byte) error { return stop }}, member)
	if !errors.Is(err, stop) {
		t.Fatalf("proxied provider flow did not reach authentication: %v", err)
	}
	if atomic.LoadInt64(relayed) == 0 {
		t.Fatal("provider strategy dials bypassed the configured proxy")
	}
}
