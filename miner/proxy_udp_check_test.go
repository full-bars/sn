package miner

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// proxy_udp_check_test.go covers the UDP path check ported from the 3.23-fix
// provider (provider/proxy_udp_check.go) and its consequence for H3
// eligibility.
//
// The proxies are real SOCKS5 servers on loopback so the check runs its real
// handshake and datagram path. Three kinds are enough for the outcomes the
// relay distinguishes:
//
//	dead    refuses UDP ASSOCIATE (the relay does not exist)
//	silent  accepts the association and then never answers a datagram
//	relay   answers a STUN binding request, from the address it is told to use
//
// The TCP side always answers the plain-HTTP address service, so the check can
// compare the address QUIC would exit from with the address TCP does.

type fakeUDPRelayMode int

const (
	fakeUDPRelayDead fakeUDPRelayMode = iota
	fakeUDPRelaySilent
	fakeUDPRelayWorking
)

type fakeSocks5Proxy struct {
	t    *testing.T
	mode fakeUDPRelayMode
	// tcpExit and udpExit are the addresses the fake reports to the TCP and
	// UDP address services
	tcpExit string
	udpExit string

	ln  net.Listener
	udp *net.UDPConn

	mu            sync.Mutex
	udpAssociates int
	closed        bool
}

func newFakeSocks5Proxy(t *testing.T, mode fakeUDPRelayMode, tcpExit, udpExit string) *fakeSocks5Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		ln.Close()
		t.Fatalf("listen udp: %v", err)
	}
	p := &fakeSocks5Proxy{t: t, mode: mode, tcpExit: tcpExit, udpExit: udpExit, ln: ln, udp: udp}
	go p.serve()
	go p.serveUDP()
	t.Cleanup(p.close)
	return p
}

func (p *fakeSocks5Proxy) address() string { return p.ln.Addr().String() }

func (p *fakeSocks5Proxy) settings() *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: p.address()}
}

func (p *fakeSocks5Proxy) associateCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.udpAssociates
}

func (p *fakeSocks5Proxy) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	p.ln.Close()
	p.udp.Close()
}

func (p *fakeSocks5Proxy) serve() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.serveConn(conn)
	}
}

// serveConn answers one SOCKS5 connection: a CONNECT is answered as the plain
// HTTP address service, a UDP ASSOCIATE per the fake's mode.
func (p *fakeSocks5Proxy) serveConn(conn net.Conn) {
	defer conn.Close()
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 0x05 {
		return
	}
	if _, err := io.ReadFull(conn, make([]byte, int(head[1]))); err != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return
	}
	var addressLength int
	switch request[3] {
	case 0x01:
		addressLength = 4
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return
		}
		addressLength = int(length[0])
	case 0x04:
		addressLength = 16
	default:
		return
	}
	if _, err := io.ReadFull(conn, make([]byte, addressLength+2)); err != nil {
		return
	}
	switch request[1] {
	case 0x01: // CONNECT: act as the address service
		if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 80}); err != nil {
			return
		}
		if !readUntilHeaderEnd(conn) {
			return
		}
		fmt.Fprintf(conn, "HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
			len(p.tcpExit), p.tcpExit)
	case 0x03: // UDP ASSOCIATE
		p.mu.Lock()
		p.udpAssociates++
		p.mu.Unlock()
		if p.mode == fakeUDPRelayDead {
			// RFC 1928 reply 0x07: command not supported
			conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			return
		}
		relayPort := p.udp.LocalAddr().(*net.UDPAddr).Port
		reply := []byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0}
		binary.BigEndian.PutUint16(reply[8:10], uint16(relayPort))
		if _, err := conn.Write(reply); err != nil {
			return
		}
		// the association lives as long as the control connection
		io.Copy(io.Discard, conn)
	}
}

func readUntilHeaderEnd(conn net.Conn) bool {
	seen := 0
	buffer := make([]byte, 1)
	for {
		if _, err := conn.Read(buffer); err != nil {
			return false
		}
		switch {
		case buffer[0] == '\r' && seen == 0:
			seen = 1
		case buffer[0] == '\n' && seen == 1:
			seen = 2
		case buffer[0] == '\r' && seen == 2:
			seen = 3
		case buffer[0] == '\n' && seen == 3:
			return true
		default:
			seen = 0
		}
	}
}

// serveUDP is the relay's datagram side. It answers every datagram with a STUN
// binding success for the fake's udpExit, unless the relay is silent or dead.
func (p *fakeSocks5Proxy) serveUDP() {
	buffer := make([]byte, 2048)
	for {
		n, from, err := p.udp.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		if p.mode != fakeUDPRelayWorking {
			continue
		}
		payload, destination, ok := fakeSocks5UdpRequest(buffer[:n])
		if !ok || len(payload) < 20 {
			continue
		}
		response := fakeStunBindingSuccess(payload[4:8], payload[8:20], p.udpExit)
		packet := append(fakeSocks5UdpHeader(nil, destination), response...)
		p.udp.WriteToUDP(packet, from)
	}
}

// fakeSocks5UdpRequest splits a datagram from the client into payload and the
// destination it is addressed to.
func fakeSocks5UdpRequest(packet []byte) ([]byte, *net.UDPAddr, bool) {
	if len(packet) < 4 || packet[2] != 0 {
		return nil, nil, false
	}
	var ip net.IP
	var offset int
	switch packet[3] {
	case 0x01:
		if len(packet) < 10 {
			return nil, nil, false
		}
		ip = net.IP(append([]byte(nil), packet[4:8]...))
		offset = 8
	case 0x04:
		if len(packet) < 22 {
			return nil, nil, false
		}
		ip = net.IP(append([]byte(nil), packet[4:20]...))
		offset = 20
	default:
		return nil, nil, false
	}
	port := int(binary.BigEndian.Uint16(packet[offset : offset+2]))
	return packet[offset+2:], &net.UDPAddr{IP: ip, Port: port}, true
}

func fakeSocks5UdpHeader(dst []byte, addr *net.UDPAddr) []byte {
	if ip4 := addr.IP.To4(); ip4 != nil {
		dst = append(dst, 0, 0, 0, 0x01)
		dst = append(dst, ip4...)
	} else {
		dst = append(dst, 0, 0, 0, 0x04)
		dst = append(dst, addr.IP.To16()...)
	}
	return binary.BigEndian.AppendUint16(dst, uint16(addr.Port))
}

// fakeStunBindingSuccess builds the reply a STUN server sends: the mapped
// address of the sender, xored with the magic cookie (RFC 5389 section 15.2).
func fakeStunBindingSuccess(cookie []byte, transaction []byte, mapped string) []byte {
	ip := net.ParseIP(mapped).To4()
	response := make([]byte, 0, 32)
	response = binary.BigEndian.AppendUint16(response, 0x0101)
	response = binary.BigEndian.AppendUint16(response, 12)
	response = append(response, cookie...)
	response = append(response, transaction...)
	response = binary.BigEndian.AppendUint16(response, 0x0020)
	response = binary.BigEndian.AppendUint16(response, 8)
	response = append(response, 0, 0x01)
	port := uint16(40000) ^ binary.BigEndian.Uint16(cookie[0:2])
	response = binary.BigEndian.AppendUint16(response, port)
	for i := 0; i < 4; i++ {
		response = append(response, ip[i]^cookie[i])
	}
	return response
}

func waitFor(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A proxy whose SOCKS5 UDP relay refuses UDP ASSOCIATE must be classified
// UDP-unusable and excluded from H3, so its identity stays on H1 instead of
// failing QUIC on a relay that does not exist.
//
// Nothing in the miner performs that check today: the proxy is eligible for H3
// the moment the mode admits it, whatever its UDP path does.
func TestH3EligibilityExcludesDeadUDPRelay(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	oldState := globalControlState
	t.Cleanup(func() {
		globalControlState = oldState
		h3RankingValue.Store(nil)
		h3LaunchMutex.Lock()
		h3LaunchEligible = map[string]bool{}
		h3LaunchMutex.Unlock()
	})

	dead := newFakeSocks5Proxy(t, fakeUDPRelayDead, "203.0.113.7", "203.0.113.7")
	working := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.8", "203.0.113.8")
	candidates := []*connect.ProxySettings{dead.settings(), working.settings()}

	setting := h3Setting{Mode: h3ModeAll}
	noteH3Candidates(candidates, setting)

	// the dead relay must be asked about and then excluded
	waitFor(t, 5*time.Second, "the dead relay to be checked and excluded from h3", func() bool {
		return !h3ProxyEligible(setting, dead.settings().Key())
	})
	if n := dead.associateCount(); n == 0 {
		t.Fatal("nothing asked the proxy for a UDP association: no UDP path check ran")
	}
	// the proxy whose relay works keeps its H3 option
	waitFor(t, 5*time.Second, "the working relay to keep h3", func() bool {
		return h3ProxyEligible(setting, working.settings().Key())
	})
}

