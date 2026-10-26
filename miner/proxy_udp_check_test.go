package miner

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
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
	// refuseConnect makes the TCP side refuse CONNECT, as a proxy with a
	// broken TCP path would, so the TCP exit cannot be learned
	refuseConnect bool

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
		if p.refuseConnect {
			// RFC 1928 reply 0x05: connection refused
			conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			return
		}
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
	previousStore := globalProxyUDPCheck
	t.Cleanup(func() {
		globalControlState = oldState
		globalProxyUDPCheck = previousStore
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

// fakeProxyUDPCheckDeps points the check at one fake proxy: the fake answers
// the STUN exchange itself, and accepts any destination the TCP exit service
// names.
func fakeProxyUDPCheckDeps(fake *fakeSocks5Proxy) proxyUDPCheckDeps {
	deps := defaultProxyUDPCheckDeps()
	deps.stunServers = []string{fake.udp.LocalAddr().String()}
	deps.tcpExitServers = []udpCheckTcpExitServer{{address: "exit.test:80", host: "exit.test"}}
	deps.replyTimeout = 100 * time.Millisecond
	deps.stunAttempts = 1
	deps.overallLimit = 3 * time.Second
	return deps
}

// A relay that refuses UDP ASSOCIATE is UDP-unusable: H3 must not be attempted
// through it, and the refusal is the reason reported.
func TestCheckProxyUDPClassifiesDeadRelay(t *testing.T) {
	fake := newFakeSocks5Proxy(t, fakeUDPRelayDead, "203.0.113.7", "203.0.113.7")
	deps := fakeProxyUDPCheckDeps(fake)
	udpDials := 0
	dialUDP := deps.dialUDP
	deps.dialUDP = func(ctx context.Context, settings *connect.ProxySettings) (net.PacketConn, error) {
		udpDials++
		return dialUDP(ctx, settings)
	}

	result := checkProxyUDPWith(deps, context.Background(), fake.settings())
	if udpDials != 1 {
		t.Fatalf("the injected udp dialer was used %d times, want 1", udpDials)
	}
	if result.Status != proxyUDPNoRelay {
		t.Fatalf("status = %s, want no-relay (err %v)", result.Status, result.Err)
	}
	if result.Err == nil {
		t.Fatal("a dead relay must say why")
	}
	if n := fake.associateCount(); n != 1 {
		t.Fatalf("UDP ASSOCIATE attempted %d times, want 1", n)
	}
	if proxyUDPAllowsH3(result.Status) {
		t.Fatal("a dead relay must not allow h3")
	}
}

// A relay that accepts the association and then never answers is UDP-unusable
// too: the datagrams go nowhere, which is what QUIC would see.
func TestCheckProxyUDPClassifiesSilentRelay(t *testing.T) {
	fake := newFakeSocks5Proxy(t, fakeUDPRelaySilent, "203.0.113.7", "203.0.113.7")
	deps := fakeProxyUDPCheckDeps(fake)

	result := checkProxyUDPWith(deps, context.Background(), fake.settings())
	if result.Status != proxyUDPNoRelay {
		t.Fatalf("status = %s, want no-relay (err %v)", result.Status, result.Err)
	}
	if n := fake.associateCount(); n != 1 {
		t.Fatalf("UDP ASSOCIATE attempted %d times, want 1", n)
	}
	if proxyUDPAllowsH3(result.Status) {
		t.Fatal("a silent relay must not allow h3")
	}
}

// A relay that works but exits from another address than the proxy's TCP side
// is the mismatch case: H3 is not used, and both addresses are reported.
func TestCheckProxyUDPClassifiesExitMismatch(t *testing.T) {
	fake := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.7", "198.51.100.9")
	deps := fakeProxyUDPCheckDeps(fake)

	result := checkProxyUDPWith(deps, context.Background(), fake.settings())
	if result.Status != proxyUDPExitMismatch {
		t.Fatalf("status = %s, want exit-mismatch (err %v)", result.Status, result.Err)
	}
	if result.TCPExit != "203.0.113.7" || result.UDPExit != "198.51.100.9" {
		t.Fatalf("exits = tcp %q udp %q, want tcp 203.0.113.7 udp 198.51.100.9", result.TCPExit, result.UDPExit)
	}
	if proxyUDPAllowsH3(result.Status) {
		t.Fatal("an exit mismatch must not allow h3")
	}
}

// A relay that works and exits where TCP does leaves H3 alone.
func TestCheckProxyUDPAcceptsMatchingExit(t *testing.T) {
	fake := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.7", "203.0.113.7")
	deps := fakeProxyUDPCheckDeps(fake)

	result := checkProxyUDPWith(deps, context.Background(), fake.settings())
	if result.Status != proxyUDPOK {
		t.Fatalf("status = %s, want ok (err %v tcp %q udp %q)", result.Status, result.Err, result.TCPExit, result.UDPExit)
	}
	if result.TCPExit != result.UDPExit || result.UDPExit != "203.0.113.7" {
		t.Fatalf("exits = tcp %q udp %q, want both 203.0.113.7", result.TCPExit, result.UDPExit)
	}
	if !proxyUDPAllowsH3(result.Status) {
		t.Fatal("a working relay that exits where tcp does must allow h3")
	}
}

// A relay that works but whose TCP exit cannot be learned is the unknown case:
// the relay is still the proxy, so H3 is allowed and the check is repeated
// sooner.
func TestCheckProxyUDPAllowsH3WhenTCPExitIsUnknown(t *testing.T) {
	fake := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.7", "203.0.113.7")
	fake.refuseConnect = true
	deps := fakeProxyUDPCheckDeps(fake)

	result := checkProxyUDPWith(deps, context.Background(), fake.settings())
	if result.Status != proxyUDPUnknown {
		t.Fatalf("status = %s, want unknown (err %v)", result.Status, result.Err)
	}
	if result.UDPExit != "203.0.113.7" {
		t.Fatalf("udp exit = %q, want the working relay's exit", result.UDPExit)
	}
	if !proxyUDPAllowsH3(result.Status) {
		t.Fatal("an unverifiable tcp exit must still allow h3")
	}
	if proxyUDPTTL(proxyUDPUnknown) >= proxyUDPTTL(proxyUDPOK) {
		t.Fatal("an unknown verdict must be re-checked sooner than a settled one")
	}
}

// The verdict is remembered per identity, so a second ask does not dial the
// proxy again, and the h3 gate reads it.
func TestProxyUDPCheckStoreRemembersTheVerdict(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	previous := globalProxyUDPCheck
	t.Cleanup(func() { globalProxyUDPCheck = previous })

	fake := newFakeSocks5Proxy(t, fakeUDPRelayDead, "203.0.113.7", "203.0.113.7")
	deps := fakeProxyUDPCheckDeps(fake)
	probes := 0
	store := newProxyUDPCheckStore(func(ctx context.Context, settings *connect.ProxySettings) proxyUDPResult {
		probes++
		return checkProxyUDPWith(deps, ctx, settings)
	})
	globalProxyUDPCheck = store

	now := time.Now()
	first := store.Check(context.Background(), fake.settings(), now)
	second := store.Check(context.Background(), fake.settings(), now.Add(time.Minute))
	if first.Status != proxyUDPNoRelay || second.Status != proxyUDPNoRelay {
		t.Fatalf("statuses = %s, %s, want no-relay twice", first.Status, second.Status)
	}
	if probes != 1 {
		t.Fatalf("the store probed %d times within the result lifetime, want 1", probes)
	}
	if size := store.size(); size != 1 {
		t.Fatalf("the store remembers %d identities, want 1", size)
	}
	if proxyUDPAdmitsH3(fake.settings().Key()) {
		t.Fatal("a remembered no-relay verdict must keep the identity off h3")
	}

	// once the verdict is old the identity is checked again
	store.Check(context.Background(), fake.settings(), now.Add(2*proxyUDPResultTTL))
	if probes != 2 {
		t.Fatalf("the store probed %d times after the verdict aged out, want 2", probes)
	}
}

// A proxy that had H3 and then turns out to have a dead UDP relay is relaunched
// so its identity comes back on H1, instead of failing QUIC until some
// unrelated setting change.
func TestDeadUDPRelayRelaunchesAnH3IdentityOntoH1(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	oldState := globalControlState
	previous := globalProxyUDPCheck
	t.Cleanup(func() {
		globalControlState = oldState
		globalProxyUDPCheck = previous
		setH3IdentityRelauncher(nil)
		h3LaunchMutex.Lock()
		h3LaunchEligible = map[string]bool{}
		h3LaunchMutex.Unlock()
		h3RankingValue.Store(nil)
	})

	state := newControlState()
	if err := state.set("h3", "all"); err != nil {
		t.Fatal(err)
	}
	globalControlState = state

	fake := newFakeSocks5Proxy(t, fakeUDPRelayDead, "203.0.113.7", "203.0.113.7")
	settings := fake.settings()
	key := settings.Key()
	recordH3Launch(key, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	launcher := &fakeH3Relauncher{ctx: ctx, busy: map[string]bool{}}
	setH3IdentityRelauncher(launcher)

	deps := fakeProxyUDPCheckDeps(fake)
	globalProxyUDPCheck = newProxyUDPCheckStore(func(ctx context.Context, settings *connect.ProxySettings) proxyUDPResult {
		return checkProxyUDPWith(deps, ctx, settings)
	})

	// the provider run notes its candidates on every reload; that is what
	// sweeps the admitted proxies
	noteH3Candidates([]*connect.ProxySettings{settings}, h3Setting{Mode: h3ModeAll})

	waitFor(t, 5*time.Second, "the dead relay to be checked", func() bool {
		_, ok := globalProxyUDPCheck.cached(key)
		return ok
	})

	waitFor(t, 5*time.Second, "the identity to be relaunched onto H1", func() bool {
		launcher.mu.Lock()
		defer launcher.mu.Unlock()
		for _, detached := range launcher.detached {
			if detached == key {
				return true
			}
		}
		return false
	})

	// the transport policy for the identity is H1 only
	preferences := h3ModePreferences(h3Setting{Mode: h3ModeAll}, false, h3ProxyEligible(h3Setting{Mode: h3ModeAll}, key))
	if len(preferences) != 1 || preferences[connect.TransportModeH1] != 1 {
		t.Fatalf("preferences = %v, want H1 only", preferences)
	}
}

// A sweep only starts checks for identities the setting admits that have no
// fresh verdict, so a reload of an unchanged pool does not probe again.
func TestRefreshProxyUDPChecksSkipsFreshAndUnadmitted(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	previous := globalProxyUDPCheck
	t.Cleanup(func() { globalProxyUDPCheck = previous })

	fake := newFakeSocks5Proxy(t, fakeUDPRelayDead, "203.0.113.7", "203.0.113.7")
	deps := fakeProxyUDPCheckDeps(fake)
	globalProxyUDPCheck = newProxyUDPCheckStore(func(ctx context.Context, settings *connect.ProxySettings) proxyUDPResult {
		return checkProxyUDPWith(deps, ctx, settings)
	})
	settings := fake.settings()
	key := settings.Key()

	if n := refreshProxyUDPChecks(context.Background(), nil, h3Setting{Mode: h3ModeAll}); n != 0 {
		t.Fatalf("a sweep over no candidates started %d checks", n)
	}
	if n := refreshProxyUDPChecks(context.Background(), []*connect.ProxySettings{settings}, h3Setting{Mode: h3ModeDirect}); n != 0 {
		t.Fatalf("direct admits no proxy, but the sweep started %d checks", n)
	}
	if n := refreshProxyUDPChecks(context.Background(), []*connect.ProxySettings{settings}, h3Setting{Mode: h3ModeAll}); n != 1 {
		t.Fatalf("the sweep started %d checks, want 1", n)
	}
	waitFor(t, 5*time.Second, "the sweep's check to land", func() bool {
		_, ok := globalProxyUDPCheck.cached(key)
		return ok
	})
	if n := refreshProxyUDPChecks(context.Background(), []*connect.ProxySettings{settings}, h3Setting{Mode: h3ModeAll}); n != 0 {
		t.Fatalf("a fresh verdict was checked again: %d checks started", n)
	}
}

// TestProxyUDPLogNamesSlotAndNextAction pins the operator text: the slot is
// named in the same slot=proxy:<addr> form as the auth and control lines, the
// loss of h3 is stated as a consequence for this slot, and a verdict that
// disables h3 names what to check. The relay-ok line stays one short sentence.
func TestProxyUDPLogNamesSlotAndNextAction(t *testing.T) {
	previous := proxyManagerLogWriter
	buffer := &providerBringUpLogBuffer{}
	proxyManagerLogWriter = buffer
	t.Cleanup(func() { proxyManagerLogWriter = previous })

	settings := &connect.ProxySettings{Network: "socks5", Address: "65.111.8.178:1081"}
	logProxyUDPResult(settings, nil, proxyUDPResult{Status: proxyUDPNoRelay, Err: errors.New("socks5: authentication failed")})
	logProxyUDPResult(settings, nil, proxyUDPResult{Status: proxyUDPOK, UDPExit: "65.111.8.178"})

	text := buffer.String()
	want := "[proxy][udp] slot=proxy:65.111.8.178:1081 udp relay unavailable (socks5: authentication failed); h3 disabled for this slot, tcp only; action: check proxy credentials\n"
	if !strings.Contains(text, want) {
		t.Fatalf("missing the no-relay line:\n%s", text)
	}
	ok := "[proxy][udp] slot=proxy:65.111.8.178:1081 udp relay ok, exit 65.111.8.178 matches tcp; h3 allowed\n"
	if !strings.Contains(text, ok) {
		t.Fatalf("missing the concise ok line:\n%s", text)
	}
	if strings.Count(text, "action:") != 1 {
		t.Fatalf("an ok verdict should not carry an action clause:\n%s", text)
	}
	if strings.Contains(text, "tcp modes only") {
		t.Fatalf("old wording survived:\n%s", text)
	}
}
