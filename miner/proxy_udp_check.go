package miner

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// proxy_udp_check.go ports the 3.23-fix v33 provider's UDP path check for a
// proxied identity (provider/proxy_udp_check.go).
//
// The proxy grade and the MiTM probes look at the TCP and TLS path only. QUIC
// takes a different path through the same proxy: a UDP relay that can be dead,
// can be silent, or can exit from a different address than the proxy's TCP
// side. The first two only cost the identity its H3 option (the transport falls
// back to TCP). The last one puts the identity at an address the grade never
// looked at, so H3 is not used for it either.
//
// The check runs for the proxies an h3 setting admits, and is remembered for a
// while:
//
//	ok        the relay works and its exit address equals the TCP exit address
//	no-relay  the proxy refuses UDP ASSOCIATE, or the relay never answers
//	mismatch  the relay works but exits from another address than TCP does
//	unknown   the relay works but the TCP exit could not be learned to compare
//
// H3 is allowed for ok and unknown (the relay is the proxy either way) and
// refused for no-relay and mismatch. Unknown is re-checked sooner. The verdict
// is what h3ProxyEligible reads (see h3_proxy_set.go), so a proxy whose UDP
// relay is dead keeps its identity on H1 instead of failing QUIC.
//
// The dialers are fields of proxyUDPCheckDeps so tests can drive a fake proxy
// without a network.

type proxyUDPStatus int

const (
	proxyUDPUnknown proxyUDPStatus = iota
	proxyUDPOK
	proxyUDPNoRelay
	proxyUDPExitMismatch
)

func (self proxyUDPStatus) String() string {
	switch self {
	case proxyUDPOK:
		return "ok"
	case proxyUDPNoRelay:
		return "no-relay"
	case proxyUDPExitMismatch:
		return "exit-mismatch"
	default:
		return "unknown"
	}
}

type proxyUDPResult struct {
	Status  proxyUDPStatus
	TCPExit string
	UDPExit string
	Err     error
}

// proxyUDPAllowsH3 is the H3 consequence of a verdict. ok is a relay that works
// and exits where TCP does; unknown is a relay that works but whose TCP exit
// could not be learned, which is still the proxy's own address. A relay that
// does not work, or works from somewhere else, takes the identity's H3 option
// away.
func proxyUDPAllowsH3(status proxyUDPStatus) bool {
	return status == proxyUDPOK || status == proxyUDPUnknown
}

type udpCheckTcpExitServer struct {
	address string
	host    string
}

// proxyUDPCheckDeps is the check's outside world. The zero value is not
// usable: use defaultProxyUDPCheckDeps, and override a field in a test.
type proxyUDPCheckDeps struct {
	// dialUDP opens a datagram path through the proxy (the relay)
	dialUDP func(ctx context.Context, settings *connect.ProxySettings) (net.PacketConn, error)
	// dialTCP opens the connection the TCP exit is measured over
	dialTCP func(ctx context.Context, network, address string) (net.Conn, error)
	// servers the check asks "what address do you see me as": STUN over UDP
	// through the relay, plain HTTP over TCP through the same proxy
	stunServers    []string
	tcpExitServers []udpCheckTcpExitServer

	replyTimeout time.Duration
	stunAttempts int
	overallLimit time.Duration
}

const (
	udpCheckStunAttempts = 2
	// how many checks may hold sockets at once
	udpCheckMaxConcurrent = 8
	udpCheckOverallLimit  = 15 * time.Second
	// how long a check is remembered, and how soon an unverifiable one is
	// repeated
	proxyUDPResultTTL  = 30 * time.Minute
	proxyUDPUnknownTTL = 5 * time.Minute

	stunMagicCookie      = 0x2112A442
	stunBindingRequest   = 0x0001
	stunBindingSuccess   = 0x0101
	stunAttrMapped       = 0x0001
	stunAttrXorMapped    = 0x0020
	stunHeaderByteCount  = 20
	stunFamilyIpv4       = 1
	stunFamilyIpv6       = 2
	tcpExitReplyMaxBytes = 4096
)

// udpCheckReplyTimeout is a variable, like the relay's, so a test can shorten
// it.
var udpCheckReplyTimeout = 2 * time.Second

// public services the check asks "what address do you see me as": STUN over
// UDP through the relay, plain HTTP over TCP through the same proxy
var (
	udpCheckStunServers    = []string{"stun.l.google.com:19302", "stun.cloudflare.com:3478"}
	udpCheckTcpExitServers = []udpCheckTcpExitServer{
		{address: "api.ipify.org:80", host: "api.ipify.org"},
		{address: "icanhazip.com:80", host: "icanhazip.com"},
	}
)

func defaultProxyUDPCheckDeps() proxyUDPCheckDeps {
	return proxyUDPCheckDeps{
		dialUDP: func(ctx context.Context, settings *connect.ProxySettings) (net.PacketConn, error) {
			network := settings.Network
			if network == "" {
				network = "tcp"
			}
			return dialSocks5UDP(ctx, network, settings.Address, settings.Auth)
		},
		dialTCP: func(ctx context.Context, network, address string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, address)
		},
		stunServers:    udpCheckStunServers,
		tcpExitServers: udpCheckTcpExitServers,
		replyTimeout:   udpCheckReplyTimeout,
		stunAttempts:   udpCheckStunAttempts,
		overallLimit:   udpCheckOverallLimit,
	}
}

func newStunBindingRequest() ([]byte, [12]byte) {
	var transaction [12]byte
	binaryRandomFill(transaction[:])
	request := make([]byte, 0, stunHeaderByteCount)
	request = binary.BigEndian.AppendUint16(request, stunBindingRequest)
	request = binary.BigEndian.AppendUint16(request, 0)
	request = binary.BigEndian.AppendUint32(request, stunMagicCookie)
	request = append(request, transaction[:]...)
	return request, transaction
}

// parseStunMappedAddress returns the address a STUN server saw the request
// from, for the response to the given transaction.
func parseStunMappedAddress(response []byte, transaction [12]byte) (net.IP, bool) {
	if len(response) < stunHeaderByteCount ||
		binary.BigEndian.Uint16(response[0:2]) != stunBindingSuccess ||
		binary.BigEndian.Uint32(response[4:8]) != stunMagicCookie ||
		!bytes.Equal(response[8:20], transaction[:]) {
		return nil, false
	}
	length := int(binary.BigEndian.Uint16(response[2:4]))
	if len(response) < stunHeaderByteCount+length {
		return nil, false
	}
	body := response[stunHeaderByteCount : stunHeaderByteCount+length]
	cookie := binary.BigEndian.AppendUint32(nil, stunMagicCookie)
	for len(body) >= 4 {
		attributeType := binary.BigEndian.Uint16(body[0:2])
		attributeLength := int(binary.BigEndian.Uint16(body[2:4]))
		if len(body) < 4+attributeLength {
			return nil, false
		}
		value := body[4 : 4+attributeLength]
		if (attributeType == stunAttrXorMapped || attributeType == stunAttrMapped) && len(value) >= 4 {
			xor := attributeType == stunAttrXorMapped
			switch value[1] {
			case stunFamilyIpv4:
				if len(value) >= 8 {
					ip := make(net.IP, 4)
					for i := range ip {
						ip[i] = value[4+i]
						if xor {
							ip[i] ^= cookie[i]
						}
					}
					return ip, true
				}
			case stunFamilyIpv6:
				if len(value) >= 20 {
					mask := append(append([]byte(nil), cookie...), transaction[:]...)
					ip := make(net.IP, 16)
					for i := range ip {
						ip[i] = value[4+i]
						if xor {
							ip[i] ^= mask[i]
						}
					}
					return ip, true
				}
			}
		}
		padded := attributeLength + (4-attributeLength%4)%4
		if len(body) < 4+padded {
			break
		}
		body = body[4+padded:]
	}
	return nil, false
}

// udpExitViaProxy asks a STUN server, through the proxy's UDP relay, which
// address it sees the datagram from.
func udpExitViaProxy(deps proxyUDPCheckDeps, ctx context.Context, settings *connect.ProxySettings) (string, error) {
	pc, err := deps.dialUDP(ctx, settings)
	if err != nil {
		return "", err
	}
	defer pc.Close()
	buffer := make([]byte, 2048)
	for _, server := range deps.stunServers {
		destination, err := net.ResolveUDPAddr("udp4", server)
		if err != nil {
			continue
		}
		request, transaction := newStunBindingRequest()
		for attempt := 0; attempt < deps.stunAttempts; attempt++ {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if _, err := pc.WriteTo(request, destination); err != nil {
				return "", err
			}
			pc.SetReadDeadline(time.Now().Add(deps.replyTimeout))
			for {
				n, _, err := pc.ReadFrom(buffer)
				if err != nil {
					break
				}
				if ip, ok := parseStunMappedAddress(buffer[:n], transaction); ok {
					return ip.String(), nil
				}
			}
		}
	}
	return "", errors.New("the relay never answered a STUN request")
}

// tcpExitViaProxy asks a plain-HTTP address service, through the same proxy
// over TCP, which address it sees the connection from.
func tcpExitViaProxy(deps proxyUDPCheckDeps, ctx context.Context, settings *connect.ProxySettings) (string, error) {
	var lastErr error
	for _, server := range deps.tcpExitServers {
		ip, err := tcpExitFrom(deps, ctx, settings, server)
		if err == nil {
			return ip, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no address service configured")
	}
	return "", lastErr
}

func tcpExitFrom(deps proxyUDPCheckDeps, ctx context.Context, settings *connect.ProxySettings, server udpCheckTcpExitServer) (string, error) {
	network := settings.Network
	if network == "" {
		network = "tcp"
	}
	conn, err := deps.dialTCP(ctx, network, settings.Address)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	deadline := time.Now().Add(8 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	conn.SetDeadline(deadline)

	user, password := "", ""
	if settings.Auth != nil {
		user, password = settings.Auth.User, settings.Auth.Password
	}
	if !socks5Greet(conn, user, password) {
		return "", errors.New("socks5 greeting failed")
	}
	host, portText, err := net.SplitHostPort(server.address)
	if err != nil {
		return "", err
	}
	port, err := net.LookupPort("tcp", portText)
	if err != nil {
		return "", err
	}
	request := []byte{socks5Version, 1, 0, 3, byte(len(host))}
	request = append(request, host...)
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err := conn.Write(request); err != nil {
		return "", err
	}
	if !readSocks5ConnectReply(conn) {
		return "", errors.New("socks5 CONNECT refused")
	}
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.0\r\nHost: %s\r\nUser-Agent: curl/8\r\nConnection: close\r\n\r\n", server.host); err != nil {
		return "", err
	}
	response, err := io.ReadAll(io.LimitReader(conn, tcpExitReplyMaxBytes))
	if err != nil && len(response) == 0 {
		return "", err
	}
	_, body, found := bytes.Cut(response, []byte("\r\n\r\n"))
	if !found {
		return "", errors.New("malformed reply from the address service")
	}
	ip := net.ParseIP(strings.TrimSpace(string(body)))
	if ip == nil {
		return "", errors.New("the address service did not answer with an address")
	}
	return ip.String(), nil
}

// checkProxyUDPWith runs the whole check for one identity with the given
// dialers and address services.
func checkProxyUDPWith(deps proxyUDPCheckDeps, ctx context.Context, settings *connect.ProxySettings) proxyUDPResult {
	if 0 < deps.overallLimit {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deps.overallLimit)
		defer cancel()
	}
	udpExit, err := udpExitViaProxy(deps, ctx, settings)
	if err != nil {
		return proxyUDPResult{Status: proxyUDPNoRelay, Err: err}
	}
	tcpExit, err := tcpExitViaProxy(deps, ctx, settings)
	if err != nil {
		return proxyUDPResult{Status: proxyUDPUnknown, UDPExit: udpExit, Err: err}
	}
	if tcpExit != udpExit {
		return proxyUDPResult{Status: proxyUDPExitMismatch, TCPExit: tcpExit, UDPExit: udpExit}
	}
	return proxyUDPResult{Status: proxyUDPOK, TCPExit: tcpExit, UDPExit: udpExit}
}

// checkProxyUDP runs the check with the real dialers and public address
// services.
func checkProxyUDP(ctx context.Context, settings *connect.ProxySettings) proxyUDPResult {
	return checkProxyUDPWith(defaultProxyUDPCheckDeps(), ctx, settings)
}

type proxyUDPCheckEntry struct {
	result proxyUDPResult
	at     time.Time
	// done is non-nil while a check for the identity is running
	done chan struct{}
}

// proxyUDPCheckStore remembers the check per proxy IDENTITY (address plus
// user), runs at most one check per identity at a time, and repeats it when
// the remembered result is old.
type proxyUDPCheckStore struct {
	mu      sync.Mutex
	probe   func(context.Context, *connect.ProxySettings) proxyUDPResult
	entries map[string]*proxyUDPCheckEntry
	// slots bounds how many checks hold sockets at once: a check holds a
	// control connection and a UDP socket
	slots     chan struct{}
	lastPrune time.Time
}

func newProxyUDPCheckStore(probe func(context.Context, *connect.ProxySettings) proxyUDPResult) *proxyUDPCheckStore {
	return &proxyUDPCheckStore{
		probe:   probe,
		entries: map[string]*proxyUDPCheckEntry{},
		slots:   make(chan struct{}, udpCheckMaxConcurrent),
	}
}

// prune drops identities nobody has asked about for two lifetimes of a result,
// at most once per lifetime, so the store follows the proxy lists instead of
// growing with every proxy ever seen. Called with the lock held.
func (self *proxyUDPCheckStore) prune(now time.Time) {
	if now.Sub(self.lastPrune) < proxyUDPResultTTL {
		return
	}
	self.lastPrune = now
	for key, entry := range self.entries {
		if entry.done == nil && now.Sub(entry.at) > 2*proxyUDPResultTTL {
			delete(self.entries, key)
		}
	}
}

var globalProxyUDPCheck = newProxyUDPCheckStore(checkProxyUDP)

func proxyUDPTTL(status proxyUDPStatus) time.Duration {
	if status == proxyUDPUnknown {
		return proxyUDPUnknownTTL
	}
	return proxyUDPResultTTL
}

// cached is the last verdict for an identity, whatever its age. The H3 gate
// reads this without blocking or touching the network, so an eligibility
// decision never waits on a probe. A verdict keeps its force until a fresh
// check replaces it; that is what makes an excluded proxy stay excluded instead
// of flapping back to eligible when its result ages out.
func (self *proxyUDPCheckStore) cached(key string) (proxyUDPResult, bool) {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.entries[key]
	if entry == nil || entry.done != nil {
		return proxyUDPResult{}, false
	}
	return entry.result, true
}

// inFlight reports whether a check for the identity is running.
func (self *proxyUDPCheckStore) inFlight(key string) bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.entries[key]
	return entry != nil && entry.done != nil
}

func (self *proxyUDPCheckStore) Check(ctx context.Context, settings *connect.ProxySettings, now time.Time) proxyUDPResult {
	key := settings.Key()
	for {
		self.mu.Lock()
		entry := self.entries[key]
		if entry != nil && entry.done != nil {
			done := entry.done
			self.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return proxyUDPResult{Status: proxyUDPUnknown, Err: ctx.Err()}
			}
		}
		if entry != nil && now.Sub(entry.at) < proxyUDPTTL(entry.result.Status) {
			result := entry.result
			self.mu.Unlock()
			return result
		}
		var previous *proxyUDPResult
		if entry != nil {
			copyOf := entry.result
			previous = &copyOf
		}
		self.prune(now)
		running := &proxyUDPCheckEntry{done: make(chan struct{})}
		self.entries[key] = running
		self.mu.Unlock()

		var result proxyUDPResult
		select {
		case self.slots <- struct{}{}:
			result = self.probe(ctx, settings)
			<-self.slots
		case <-ctx.Done():
			result = proxyUDPResult{Status: proxyUDPUnknown, Err: ctx.Err()}
		}

		self.mu.Lock()
		if ctx.Err() != nil {
			// an interrupted check says nothing: forget it
			delete(self.entries, key)
			if previous != nil {
				self.entries[key] = &proxyUDPCheckEntry{result: *previous, at: now.Add(-proxyUDPTTL(previous.Status))}
			}
			close(running.done)
			self.mu.Unlock()
			return proxyUDPResult{Status: proxyUDPUnknown, Err: ctx.Err()}
		}
		running.result = result
		running.at = now
		close(running.done)
		running.done = nil
		self.mu.Unlock()

		logProxyUDPResult(settings, previous, result)
		noteProxyUDPVerdict(settings, previous, result)
		return result
	}
}

// proxyUDPAdmitsH3 is the H3 gate for an identity, read from the remembered
// verdict. An identity that has not been checked yet is admitted: an unchecked
// proxy is no worse off than one whose TCP exit could not be learned, and
// blocking every H3 attempt on a probe would stall a pool of hundreds. See
// h3ProxyEligible.
func proxyUDPAdmitsH3(key string) bool {
	result, ok := globalProxyUDPCheck.cached(key)
	if !ok {
		return true
	}
	return proxyUDPAllowsH3(result.Status)
}

// fresh reports whether the remembered verdict for the identity is still
// within its lifetime and so does not need re-checking yet.
func (self *proxyUDPCheckStore) fresh(key string, now time.Time) bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.entries[key]
	return entry != nil && entry.done == nil && now.Sub(entry.at) < proxyUDPTTL(entry.result.Status)
}

// refreshProxyUDPChecks checks the UDP path of the candidates an h3 setting
// admits, in the background, so the verdict is there when eligibility is asked
// for. Candidates with a fresh verdict, or a check already running, are left
// alone.
func refreshProxyUDPChecks(ctx context.Context, candidates []*connect.ProxySettings, setting h3Setting) int {
	if !setting.proxiesMayUseTransports() {
		return 0
	}
	now := time.Now()
	started := 0
	for _, settings := range candidates {
		if settings == nil {
			continue
		}
		key := settings.Key()
		if globalProxyUDPCheck.fresh(key, now) || globalProxyUDPCheck.inFlight(key) {
			continue
		}
		settings := settings
		started++
		go globalProxyUDPCheck.Check(ctx, settings, time.Now())
	}
	return started
}

// noteProxyUDPVerdict applies a fresh verdict to a running identity: a proxy
// that just lost its H3 option and was launched with it is relaunched on H1,
// or it would keep failing QUIC until an unrelated setting change.
func noteProxyUDPVerdict(settings *connect.ProxySettings, previous *proxyUDPResult, result proxyUDPResult) {
	if proxyUDPAllowsH3(result.Status) {
		return
	}
	if previous != nil && !proxyUDPAllowsH3(previous.Status) {
		// already excluded, nothing moved
		return
	}
	if !h3ProxyLaunchedEligible(settings.Key()) {
		return
	}
	relaunchH3ProxiesNow(resolveH3Mode(globalControlState, currentProxyResolution()))
}

// logProxyUDPResult says so once per change of answer for an identity, so a
// fleet of proxies produces one line each, not one per attempt. The slot is
// named in the same `slot=proxy:<addr>` form every other provider line uses,
// so one grep for a slot finds its UDP verdict next to its auth and control
// lines, and each line that disables h3 says what to do about it.
func logProxyUDPResult(settings *connect.ProxySettings, previous *proxyUDPResult, result proxyUDPResult) {
	if previous != nil && previous.Status == result.Status {
		return
	}
	slot := providerSlotDisplay(settings)
	switch result.Status {
	case proxyUDPOK:
		tlog("[proxy][udp] slot=%s udp relay ok, exit %s matches tcp; h3 allowed\n", slot, result.UDPExit)
	case proxyUDPExitMismatch:
		tlog("[proxy][udp] slot=%s udp relay exits from %s but tcp from %s; h3 disabled for this slot, tcp only; action: check where udp and tcp egress diverge on this proxy\n", slot, result.UDPExit, result.TCPExit)
	case proxyUDPNoRelay:
		tlog("[proxy][udp] slot=%s udp relay unavailable (%v); h3 disabled for this slot, tcp only; action: check proxy credentials\n", slot, result.Err)
	default:
		tlog("[proxy][udp] slot=%s udp relay works (exit %s) but the tcp exit could not be learned (%v); h3 allowed, checking again soon\n", slot, result.UDPExit, result.Err)
	}
}

// size is how many identities the store currently remembers.
func (self *proxyUDPCheckStore) size() int {
	self.mu.Lock()
	defer self.mu.Unlock()
	return len(self.entries)
}
