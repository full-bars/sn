package miner

// proxy_ipv6_udp.go - can the UDP relay of a proxy carry IPv6 datagrams?
//
// The pin controller (proxy_v6_pin_controller.go) proves IPv6 for TCP: a
// SOCKS5 CONNECT to the platform's IPv6 host and a TLS handshake over it. That
// says nothing about the proxy's UDP relay, and a relay without an IPv6 exit
// would swallow an IPv6 datagram instead of failing fast. So a client UDP flow
// to an IPv6 destination goes through a proxied identity only when ALL hold:
//
//   - the proxy_udp_ipv6 setting is on (default on),
//   - the pin controller's verdict for the proxy is yes,
//   - the IPv4 UDP check of the relay is ok,
//   - a fresh IPv6 UDP check of the relay says yes: a STUN request over IPv6
//     through the relay is answered, and the address it reports is the TCP exit
//     of the proxy over IPv6 (the same match rule as the IPv4 check, so the
//     flow can never leave from the host's own address).
//
// Any other proxy refuses an IPv6 destination at once, as before. The check
// runs on demand: the first refused flow for a proxy whose TCP verdict is yes
// starts it, at most one per proxy, two at a time overall, and the next flows
// see its answer.

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

type proxyUDP6Status string

const (
	proxyUDP6Unknown  proxyUDP6Status = "unknown"
	proxyUDP6Yes      proxyUDP6Status = "yes"
	proxyUDP6Mismatch proxyUDP6Status = "mismatch"
)

const (
	// a relay that carried IPv6 is unlikely to stop within hours
	udp6YesTTL      = 12 * time.Hour
	udp6MismatchTTL = 24 * time.Hour
	// an unverifiable check is repeated sooner
	udp6UnknownTTL = time.Hour
	// how many IPv6 checks may run at once
	udp6MaxConcurrent = 2
)

// udpCheckTcpExitServers6 are address services that have only IPv6 addresses,
// so a CONNECT by name through the proxy answers only when the proxy reached
// them over IPv6.
var udpCheckTcpExitServers6 = []udpCheckTcpExitServer{
	{address: "api6.ipify.org:80", host: "api6.ipify.org"},
	{address: "ipv6.icanhazip.com:80", host: "ipv6.icanhazip.com"},
}

type proxyUDP6Result struct {
	Status  proxyUDP6Status
	TCPExit string
	UDPExit string
	At      time.Time
}

func (self proxyUDP6Result) ttl() time.Duration {
	switch self.Status {
	case proxyUDP6Yes:
		return udp6YesTTL
	case proxyUDP6Mismatch:
		return udp6MismatchTTL
	}
	return udp6UnknownTTL
}

// resolveStunServer resolves a STUN host:port for one family with the check's
// context, so a slow lookup ends with the check. udp4 keeps the plain resolver
// the IPv4 check always used.
func resolveStunServer(ctx context.Context, network string, server string) (*net.UDPAddr, error) {
	if network != "udp6" {
		return net.ResolveUDPAddr(network, server)
	}
	host, portText, err := net.SplitHostPort(server)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return nil, errors.New("invalid STUN port")
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if addr.IP.To4() == nil {
			return &net.UDPAddr{IP: addr.IP, Port: port}, nil
		}
	}
	return nil, errors.New("the STUN host has no IPv6 address")
}

func ipv6Only(address string) bool {
	ip := net.ParseIP(address)
	return ip != nil && ip.To4() == nil
}

// checkProxyUDP6With asks over IPv6, through the proxy, which address an
// address service sees (TCP, a CONNECT by name) and which address a STUN server
// sees through the UDP relay. Both must be IPv6 and equal.
func checkProxyUDP6With(deps proxyUDPCheckDeps, ctx context.Context, settings *connect.ProxySettings) proxyUDP6Result {
	if 0 < deps.overallLimit {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deps.overallLimit)
		defer cancel()
	}
	tcpDeps := deps
	tcpDeps.tcpExitServers = udpCheckTcpExitServers6
	tcpExit, err := tcpExitViaProxy(tcpDeps, ctx, settings)
	if err != nil || !ipv6Only(tcpExit) {
		return proxyUDP6Result{Status: proxyUDP6Unknown}
	}
	udpExit, err := udpExitViaProxyOn(deps, ctx, settings, "udp6")
	if err != nil || !ipv6Only(udpExit) {
		return proxyUDP6Result{Status: proxyUDP6Unknown, TCPExit: tcpExit}
	}
	if !net.ParseIP(tcpExit).Equal(net.ParseIP(udpExit)) {
		return proxyUDP6Result{Status: proxyUDP6Mismatch, TCPExit: tcpExit, UDPExit: udpExit}
	}
	return proxyUDP6Result{Status: proxyUDP6Yes, TCPExit: tcpExit, UDPExit: udpExit}
}

type proxyUDP6Entry struct {
	result  proxyUDP6Result
	running bool
}

// proxyUDP6Store remembers the IPv6 UDP check per proxy identity.
type proxyUDP6Store struct {
	mu      sync.Mutex
	probe   func(context.Context, *connect.ProxySettings) proxyUDP6Result
	entries map[string]*proxyUDP6Entry
	slots   chan struct{}
	// wait joins the running checks; tests use it
	wait sync.WaitGroup
}

func newProxyUDP6Store(probe func(context.Context, *connect.ProxySettings) proxyUDP6Result) *proxyUDP6Store {
	return &proxyUDP6Store{
		probe:   probe,
		entries: map[string]*proxyUDP6Entry{},
		slots:   make(chan struct{}, udp6MaxConcurrent),
	}
}

var globalProxyUDP6 = newProxyUDP6Store(func(ctx context.Context, settings *connect.ProxySettings) proxyUDP6Result {
	return checkProxyUDP6With(defaultProxyUDPCheckDeps(), ctx, settings)
})

// proxyUDP6 is the store the egress reads; a test replaces it.
var proxyUDP6 = globalProxyUDP6

// allowed reports a fresh yes: the result is not from the future and is
// younger than twice its trust window, so a proxy that stopped being checked
// stops being trusted.
func (self *proxyUDP6Store) allowed(key string, now time.Time) bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.entries[key]
	if entry == nil || entry.result.Status != proxyUDP6Yes {
		return false
	}
	at := entry.result.At
	return !at.After(now) && now.Before(at.Add(2*entry.result.ttl()))
}

// result returns the last IPv6 UDP result of an identity.
func (self *proxyUDP6Store) result(key string) (proxyUDP6Result, bool) {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.entries[key]
	if entry == nil || entry.result.At.IsZero() {
		return proxyUDP6Result{}, false
	}
	return entry.result, true
}

// kick starts the IPv6 UDP check of one identity when its result is missing or
// older than its trust window. At most one runs per identity and udp6MaxConcurrent
// overall. It never blocks and returns whether a check started.
func (self *proxyUDP6Store) kick(settings *connect.ProxySettings, now time.Time) bool {
	key := settings.Key()
	self.mu.Lock()
	entry := self.entries[key]
	if entry == nil {
		entry = &proxyUDP6Entry{}
		self.entries[key] = entry
	}
	if entry.running || (!entry.result.At.IsZero() && now.Before(entry.result.At.Add(entry.result.ttl()))) {
		self.mu.Unlock()
		return false
	}
	select {
	case self.slots <- struct{}{}:
	default:
		self.mu.Unlock()
		return false
	}
	entry.running = true
	self.wait.Add(1)
	self.mu.Unlock()
	go connect.HandleError(func() {
		defer self.wait.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*udpCheckOverallLimit)
		result := self.probe(ctx, settings)
		cancel()
		result.At = now
		self.mu.Lock()
		entry.result = result
		entry.running = false
		self.mu.Unlock()
		<-self.slots
	})
	return true
}

// proxyUDPIPv6EnabledIn reports the proxy_udp_ipv6 setting. It is on by
// default, and off restores the refusal of every IPv6 destination.
func proxyUDPIPv6EnabledIn(state *controlState) bool {
	if state == nil {
		return true
	}
	value, found := state.get("proxy_udp_ipv6")
	return !found || strings.EqualFold(value, "on")
}

func proxyUDPIPv6Enabled() bool {
	return proxyUDPIPv6EnabledIn(globalControlState)
}

// proxyIPv6TCPVerdict is the pin controller's verdict for a proxy identity; a
// test replaces it.
var proxyIPv6TCPVerdict = func(key string) v6Verdict {
	controller := currentV6Pins()
	if controller == nil || controller.ledger == nil {
		return v6Unknown
	}
	return controller.ledger.Verdict(key)
}

// proxiedUdpIPv6Allowed decides whether a client UDP flow to an IPv6
// destination may use this identity's relay. It reads cached answers only.
func proxiedUdpIPv6Allowed(settings *connect.ProxySettings, checkStore *proxyUDPCheckStore, now time.Time) bool {
	if !proxyUDPIPv6Enabled() {
		return false
	}
	key := settings.Key()
	if proxyIPv6TCPVerdict(key) != v6Yes {
		return false
	}
	if result, ok := checkStore.cached(key); !ok || result.Status != proxyUDPOK {
		return false
	}
	return proxyUDP6.allowed(key, now)
}

// proxiedUdpIPv6Candidate reports whether a refused IPv6 flow should start the
// IPv6 UDP check: the feature is on and the proxy is already proven for IPv6 TCP
// with a working IPv4 UDP relay.
func proxiedUdpIPv6Candidate(settings *connect.ProxySettings, checkStore *proxyUDPCheckStore) bool {
	if !proxyUDPIPv6Enabled() || proxyIPv6TCPVerdict(settings.Key()) != v6Yes {
		return false
	}
	now := time.Now()
	result, ok := checkStore.cached(settings.Key())
	if !ok || !checkStore.fresh(settings.Key(), now) {
		// no IPv4 result, or an expired one: start the check without waiting, so
		// the next flow can tell. Flows of a proxy that only ever carries IPv6
		// would otherwise keep reading the stale answer
		checkStore.checkDetached(settings, now)
		return false
	}
	return result.Status == proxyUDPOK
}
