package miner

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Fixtures for proxy_udp_egress_test.go, lifted from the meso-miner
// socks5_udp_test.go (this tree has no such file).
//
// fakeSocks5 is a minimal SOCKS5 server that speaks UDP ASSOCIATE, with a real
// UDP relay, so the tests can prove that datagrams reach their destination
// through the relay and not from the client's own socket.
type fakeSocks5 struct {
	listener  net.Listener
	user      string
	password  string
	refuseUDP bool
	// reportIP, when set, is the relay address the proxy claims in its reply
	// (a hostile or NATed proxy may name any host)
	reportIP net.IP
	// tcpExitBody, when set, is what a CONNECT-then-GET through this proxy
	// answers (a stand-in for an "what is my address" service)
	tcpExitBody string

	// accepts counts control connections; associations counts the UDP
	// associations that are open right now
	accepts      atomic.Int64
	associations atomic.Int64

	mu        sync.Mutex
	relayPort int
	relayOnce chan struct{}
	dropAssoc chan struct{}
}

func startFakeSocks5(t *testing.T, user, password string, refuseUDP bool) *fakeSocks5 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeSocks5{
		listener:  listener,
		user:      user,
		password:  password,
		refuseUDP: refuseUDP,
		relayOnce: make(chan struct{}),
		dropAssoc: make(chan struct{}),
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			server.accepts.Add(1)
			go server.serve(conn)
		}
	}()
	return server
}

func (self *fakeSocks5) address() string { return self.listener.Addr().String() }

func (self *fakeSocks5) waitRelayPort(t *testing.T) int {
	t.Helper()
	select {
	case <-self.relayOnce:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy never opened a relay")
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.relayPort
}

func (self *fakeSocks5) serve(conn net.Conn) {
	defer conn.Close()
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 5 {
		return
	}
	if _, err := io.ReadFull(conn, make([]byte, head[1])); err != nil {
		return
	}
	if self.user != "" {
		conn.Write([]byte{5, 2})
		auth := make([]byte, 2)
		if _, err := io.ReadFull(conn, auth); err != nil {
			return
		}
		user := make([]byte, auth[1])
		io.ReadFull(conn, user)
		plen := make([]byte, 1)
		io.ReadFull(conn, plen)
		password := make([]byte, plen[0])
		io.ReadFull(conn, password)
		if string(user) != self.user || string(password) != self.password {
			conn.Write([]byte{1, 1})
			return
		}
		conn.Write([]byte{1, 0})
	} else {
		conn.Write([]byte{5, 0})
	}

	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return
	}
	switch request[3] {
	case 1:
		io.ReadFull(conn, make([]byte, 4+2))
	case 3:
		length := make([]byte, 1)
		io.ReadFull(conn, length)
		io.ReadFull(conn, make([]byte, int(length[0])+2))
	case 4:
		io.ReadFull(conn, make([]byte, 16+2))
	}
	if request[1] == 1 && self.tcpExitBody != "" {
		conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		reader := make([]byte, 1)
		var seen []byte
		for !(len(seen) >= 4 && string(seen[len(seen)-4:]) == "\r\n\r\n") {
			if _, err := io.ReadFull(conn, reader); err != nil {
				return
			}
			seen = append(seen, reader[0])
		}
		conn.Write([]byte("HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\n\r\n" + self.tcpExitBody + "\n"))
		return
	}
	if request[1] != 3 || self.refuseUDP {
		conn.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}

	relay, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		conn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer relay.Close()
	self.associations.Add(1)
	defer self.associations.Add(-1)
	port := relay.LocalAddr().(*net.UDPAddr).Port
	self.mu.Lock()
	self.relayPort = port
	self.mu.Unlock()
	reply := []byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}
	if ip4 := self.reportIP.To4(); ip4 != nil {
		copy(reply[4:8], ip4)
	}
	binary.BigEndian.PutUint16(reply[8:], uint16(port))
	conn.Write(reply)
	select {
	case <-self.relayOnce:
	default:
		close(self.relayOnce)
	}

	done := make(chan struct{})
	go func() {
		// the association lives as long as the control connection
		io.Copy(io.Discard, conn)
		close(done)
	}()
	go func() {
		select {
		case <-self.dropAssoc:
			conn.Close()
		case <-done:
		}
	}()

	var client *net.UDPAddr
	buf := make([]byte, 65535)
	for {
		relay.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, from, err := relay.ReadFromUDP(buf)
		select {
		case <-done:
			return
		default:
		}
		if err != nil {
			continue
		}
		if client == nil || from.String() == client.String() {
			client = from
			if n < 10 || buf[0] != 0 || buf[1] != 0 || buf[2] != 0 || buf[3] != 1 {
				continue
			}
			destination := &net.UDPAddr{IP: net.IP(append([]byte(nil), buf[4:8]...)), Port: int(binary.BigEndian.Uint16(buf[8:10]))}
			relay.WriteToUDP(buf[10:n], destination)
			continue
		}
		// a datagram from a destination: wrap it and hand it to the client
		ip4 := from.IP.To4()
		wrapped := append([]byte{0, 0, 0, 1}, ip4...)
		wrapped = binary.BigEndian.AppendUint16(wrapped, uint16(from.Port))
		wrapped = append(wrapped, buf[:n]...)
		relay.WriteToUDP(wrapped, client)
	}
}

// startEchoUdp echoes datagrams and reports the source it saw them from.
func startEchoUdp(t *testing.T) (*net.UDPConn, chan *net.UDPAddr) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan *net.UDPAddr, 16)
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			seen <- from
			conn.WriteToUDP(buf[:n], from)
		}
	}()
	return conn, seen
}
