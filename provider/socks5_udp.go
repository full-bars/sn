package provider

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// SOCKS5 UDP relay (RFC 1928 section 7), so that QUIC for a proxied identity
// leaves through that identity's proxy instead of from the host.
//
// The platform transport's H3 modes open a UDP socket, and left alone that is
// the host's own socket, so an identity that wins the H3 race reaches the
// platform from the host's address and every proxy identity on the host looks
// like one address. upstream connect exposes PlatformTransportSettings.
// H3PacketConnFactory for exactly this ("headless multi-provider hosts use it
// to preserve distinct source identities"); this is the packet conn the
// factory returns for a proxied identity.
//
// Design rules, each pinned by a test:
//   - a proxy that will not relay UDP is an error, never a fallback to a socket
//     on the host (the platform transport then falls back to a TCP mode, which
//     already goes through the proxy)
//   - the relay is the PROXY's address plus the port it reports. The address in
//     the reply is never trusted: a hostile proxy could name an internal host
//   - only datagrams from the relay are accepted, so nobody else who can reach
//     the client socket can inject packets into the connection
//   - the association ends with the control connection, so the packet conn
//     stops working when the proxy drops it

const (
	socks5Version         = 5
	socks5CmdUdpAssociate = 3
	socks5AtypIpv4        = 1
	socks5AtypIpv6        = 4
	// the largest header a datagram we parse can carry (ipv6 address)
	socks5UdpMaxHeader       = 4 + 16 + 2
	socks5HandshakeTimeout   = 15 * time.Second
	socks5ControlKeepAlive   = 30 * time.Second
	socks5UdpReplyBufferSize = 65535
)

// encodeSocks5UdpHeader is the header a datagram to addr carries on its way to
// the relay: RSV RSV FRAG ATYP DST.ADDR DST.PORT.
func encodeSocks5UdpHeader(addr *net.UDPAddr) []byte {
	return appendSocks5UdpHeader(make([]byte, 0, socks5UdpMaxHeader), addr)
}

// appendSocks5UdpHeader appends the header to dst, so the packet path can build
// header and payload in one pooled buffer with no allocation.
func appendSocks5UdpHeader(dst []byte, addr *net.UDPAddr) []byte {
	if ip4 := addr.IP.To4(); ip4 != nil {
		dst = append(dst, 0, 0, 0, socks5AtypIpv4)
		dst = append(dst, ip4...)
		return binary.BigEndian.AppendUint16(dst, uint16(addr.Port))
	}
	dst = append(dst, 0, 0, 0, socks5AtypIpv6)
	dst = append(dst, addr.IP.To16()...)
	return binary.BigEndian.AppendUint16(dst, uint16(addr.Port))
}

// socks5PacketBuffers holds the scratch buffers of the packet path. QUIC sends
// a datagram at a time at a high rate, and a buffer per read or write is
// garbage per packet. A buffer is only ever used inside one ReadFrom or
// WriteTo call, so a plain pool is enough; an oversized datagram falls back to
// a one-off allocation.
const socks5PacketBufferSize = 2048

var socks5PacketBuffers = sync.Pool{
	New: func() any {
		buffer := make([]byte, socks5PacketBufferSize)
		return &buffer
	},
}

// parseSocks5UdpPacket splits a datagram from the relay into its payload and
// the address it came from. Fragmented datagrams, domain-name addresses and
// truncated headers are rejected: QUIC never needs them.
func parseSocks5UdpPacket(packet []byte) (payload []byte, from *net.UDPAddr, ok bool) {
	if len(packet) < 4 || packet[2] != 0 {
		return nil, nil, false
	}
	var ip net.IP
	var offset int
	switch packet[3] {
	case socks5AtypIpv4:
		if len(packet) < 4+4+2 {
			return nil, nil, false
		}
		ip = net.IP(append([]byte(nil), packet[4:8]...))
		offset = 8
	case socks5AtypIpv6:
		if len(packet) < 4+16+2 {
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

// socks5UdpAssociate runs the handshake on an open control connection and
// returns the port of the relay the proxy opened.
func socks5UdpAssociate(conn net.Conn, auth *proxy.Auth) (int, error) {
	methods := []byte{0x00}
	if auth != nil && auth.User != "" {
		methods = []byte{0x00, 0x02}
	}
	if _, err := conn.Write(append([]byte{socks5Version, byte(len(methods))}, methods...)); err != nil {
		return 0, err
	}
	choice := make([]byte, 2)
	if _, err := io.ReadFull(conn, choice); err != nil {
		return 0, err
	}
	if choice[0] != socks5Version {
		return 0, fmt.Errorf("socks5: unexpected version %d", choice[0])
	}
	switch choice[1] {
	case 0x00:
	case 0x02:
		if auth == nil || auth.User == "" {
			return 0, errors.New("socks5: the proxy requires credentials")
		}
		if len(auth.User) > 255 || len(auth.Password) > 255 {
			return 0, errors.New("socks5: credentials too long")
		}
		request := []byte{1, byte(len(auth.User))}
		request = append(request, auth.User...)
		request = append(request, byte(len(auth.Password)))
		request = append(request, auth.Password...)
		if _, err := conn.Write(request); err != nil {
			return 0, err
		}
		status := make([]byte, 2)
		if _, err := io.ReadFull(conn, status); err != nil {
			return 0, err
		}
		if status[1] != 0 {
			return 0, errors.New("socks5: authentication failed")
		}
	default:
		return 0, fmt.Errorf("socks5: no acceptable authentication method (%d)", choice[1])
	}

	// the client address is unknown until the first datagram, so it is left
	// unspecified, which RFC 1928 allows
	request := []byte{socks5Version, socks5CmdUdpAssociate, 0, socks5AtypIpv4, 0, 0, 0, 0, 0, 0}
	if _, err := conn.Write(request); err != nil {
		return 0, err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return 0, err
	}
	if head[1] != 0 {
		return 0, fmt.Errorf("socks5: the proxy refused UDP ASSOCIATE (reply %d)", head[1])
	}
	var addressLength int
	switch head[3] {
	case socks5AtypIpv4:
		addressLength = 4
	case socks5AtypIpv6:
		addressLength = 16
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return 0, err
		}
		addressLength = int(length[0])
	default:
		return 0, fmt.Errorf("socks5: unknown address type %d", head[3])
	}
	if _, err := io.ReadFull(conn, make([]byte, addressLength)); err != nil {
		return 0, err
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return 0, err
	}
	relayPort := int(binary.BigEndian.Uint16(port))
	if relayPort == 0 {
		return 0, errors.New("socks5: the proxy reported no relay port")
	}
	return relayPort, nil
}

// dialSocks5UDP opens a UDP association on the proxy and returns a packet conn
// whose datagrams are relayed by it.
func dialSocks5UDP(ctx context.Context, proxyNetwork string, proxyAddress string, auth *proxy.Auth) (net.PacketConn, error) {
	if proxyNetwork == "" {
		proxyNetwork = "tcp"
	}
	var dialer net.Dialer
	control, err := dialer.DialContext(ctx, proxyNetwork, proxyAddress)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(socks5HandshakeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	control.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { control.Close() })
	relayPort, err := socks5UdpAssociate(control, auth)
	if !stop() {
		control.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		control.Close()
		return nil, err
	}
	control.SetDeadline(time.Time{})
	if tcp, ok := control.(*net.TCPConn); ok {
		tcp.SetKeepAlive(true)
		tcp.SetKeepAlivePeriod(socks5ControlKeepAlive)
	}

	tcpAddr, ok := control.RemoteAddr().(*net.TCPAddr)
	if !ok {
		control.Close()
		return nil, errors.New("socks5: the control connection has no tcp address")
	}
	relay := &net.UDPAddr{IP: tcpAddr.IP, Port: relayPort}
	network := "udp4"
	if relay.IP.To4() == nil {
		network = "udp6"
	}
	local, err := net.ListenUDP(network, nil)
	if err != nil {
		control.Close()
		return nil, err
	}
	pc := &socks5PacketConn{
		local:   local,
		control: control,
		relay:   relay,
		closed:  make(chan struct{}),
	}
	go func() {
		// the association lives as long as the control connection
		io.Copy(io.Discard, control)
		pc.Close()
	}()
	return pc, nil
}

type socks5PacketConn struct {
	local   *net.UDPConn
	control net.Conn
	relay   *net.UDPAddr

	closeOnce sync.Once
	closed    chan struct{}
}

func (self *socks5PacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	need := len(p) + socks5UdpMaxHeader
	var buffer []byte
	if need <= socks5PacketBufferSize {
		pooled := socks5PacketBuffers.Get().(*[]byte)
		defer socks5PacketBuffers.Put(pooled)
		buffer = (*pooled)[:need]
	} else {
		buffer = make([]byte, need)
	}
	for {
		n, from, err := self.local.ReadFromUDP(buffer)
		if err != nil {
			select {
			case <-self.closed:
				return 0, nil, net.ErrClosed
			default:
			}
			return 0, nil, err
		}
		if !from.IP.Equal(self.relay.IP) || from.Port != self.relay.Port {
			continue
		}
		payload, source, ok := parseSocks5UdpPacket(buffer[:n])
		if !ok {
			continue
		}
		return copy(p, payload), source, nil
	}
}

func (self *socks5PacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	select {
	case <-self.closed:
		return 0, net.ErrClosed
	default:
	}
	destination, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, fmt.Errorf("socks5 udp: cannot send to %T", addr)
	}
	var packet []byte
	if need := len(p) + socks5UdpMaxHeader; need <= socks5PacketBufferSize {
		pooled := socks5PacketBuffers.Get().(*[]byte)
		defer socks5PacketBuffers.Put(pooled)
		packet = (*pooled)[:0]
	} else {
		packet = make([]byte, 0, len(p)+socks5UdpMaxHeader)
	}
	packet = appendSocks5UdpHeader(packet, destination)
	packet = append(packet, p...)
	if _, err := self.local.WriteToUDP(packet, self.relay); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (self *socks5PacketConn) Close() error {
	var err error
	self.closeOnce.Do(func() {
		close(self.closed)
		err = self.local.Close()
		self.control.Close()
	})
	return err
}

func (self *socks5PacketConn) LocalAddr() net.Addr           { return self.local.LocalAddr() }
func (self *socks5PacketConn) SetDeadline(t time.Time) error { return self.local.SetDeadline(t) }
func (self *socks5PacketConn) SetReadDeadline(t time.Time) error {
	return self.local.SetReadDeadline(t)
}
func (self *socks5PacketConn) SetWriteDeadline(t time.Time) error {
	return self.local.SetWriteDeadline(t)
}

// quic-go asks for larger socket buffers through these
func (self *socks5PacketConn) SetReadBuffer(bytes int) error { return self.local.SetReadBuffer(bytes) }
func (self *socks5PacketConn) SetWriteBuffer(bytes int) error {
	return self.local.SetWriteBuffer(bytes)
}

// binaryRandomFill fills b with random bytes (STUN transaction ids).
func binaryRandomFill(b []byte) {
	if _, err := cryptorand.Read(b); err != nil {
		panic(err)
	}
}
