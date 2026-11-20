package miner

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// stunPacketConn is a relay that answers a STUN binding request with a fixed
// mapped address, or with nothing.
type stunPacketConn struct {
	mu      sync.Mutex
	mapped  string
	silent  bool
	pending [][]byte
}

func (self *stunPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	if len(p) < 20 || self.silent {
		return len(p), nil
	}
	cookie := p[4:8]
	transaction := p[8:20]
	ip := net.ParseIP(self.mapped)
	response := binary.BigEndian.AppendUint16(nil, 0x0101)
	if ip.To4() != nil {
		response = binary.BigEndian.AppendUint16(response, 12)
		response = append(response, cookie...)
		response = append(response, transaction...)
		response = binary.BigEndian.AppendUint16(response, 0x0020)
		response = binary.BigEndian.AppendUint16(response, 8)
		response = append(response, 0, 0x01, 0, 0)
		for i, b := range ip.To4() {
			response = append(response, b^cookie[i])
		}
	} else {
		response = binary.BigEndian.AppendUint16(response, 24)
		response = append(response, cookie...)
		response = append(response, transaction...)
		response = binary.BigEndian.AppendUint16(response, 0x0020)
		response = binary.BigEndian.AppendUint16(response, 20)
		response = append(response, 0, 0x02, 0, 0)
		mask := append(append([]byte(nil), cookie...), transaction...)
		for i, b := range ip.To16() {
			response = append(response, b^mask[i])
		}
	}
	self.mu.Lock()
	self.pending = append(self.pending, response)
	self.mu.Unlock()
	return len(p), nil
}

func (self *stunPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	if len(self.pending) == 0 {
		return 0, nil, errors.New("deadline")
	}
	n := copy(p, self.pending[0])
	self.pending = self.pending[1:]
	return n, &net.UDPAddr{IP: net.ParseIP("2001:db8::53"), Port: 3478}, nil
}

func (self *stunPacketConn) Close() error                     { return nil }
func (self *stunPacketConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (self *stunPacketConn) SetDeadline(time.Time) error      { return nil }
func (self *stunPacketConn) SetReadDeadline(time.Time) error  { return nil }
func (self *stunPacketConn) SetWriteDeadline(time.Time) error { return nil }

func udp6TestDeps(fake *fakeSocks5Proxy, relay *stunPacketConn) proxyUDPCheckDeps {
	deps := defaultProxyUDPCheckDeps()
	deps.dialUDP = func(context.Context, *connect.ProxySettings) (net.PacketConn, error) { return relay, nil }
	deps.stunServers = []string{"[2001:db8::53]:3478"}
	deps.replyTimeout = 50 * time.Millisecond
	deps.stunAttempts = 1
	deps.overallLimit = 3 * time.Second
	return deps
}

func TestCheckProxyUDP6With(t *testing.T) {
	for _, test := range []struct {
		name    string
		tcpExit string
		mapped  string
		silent  bool
		refuse  bool
		want    proxyUDP6Status
	}{
		{"both exits ipv6 and equal", "2001:db8::1", "2001:db8::1", false, false, proxyUDP6Yes},
		{"udp exit differs from the tcp exit", "2001:db8::1", "2001:db8::2", false, false, proxyUDP6Mismatch},
		{"the tcp answer came over ipv4", "203.0.113.7", "2001:db8::1", false, false, proxyUDP6Unknown},
		{"the udp answer came over ipv4", "2001:db8::1", "203.0.113.7", false, false, proxyUDP6Unknown},
		{"the relay never answers", "2001:db8::1", "2001:db8::1", true, false, proxyUDP6Unknown},
		{"the proxy refuses the tcp connect", "2001:db8::1", "2001:db8::1", false, true, proxyUDP6Unknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeSocks5Proxy(t, fakeUDPRelayWorking, test.tcpExit, test.mapped)
			fake.refuseConnect = test.refuse
			relay := &stunPacketConn{mapped: test.mapped, silent: test.silent}
			result := checkProxyUDP6With(udp6TestDeps(fake, relay), context.Background(), fake.settings())
			if result.Status != test.want {
				t.Fatalf("status = %q, want %q (%+v)", result.Status, test.want, result)
			}
		})
	}
}

func udp6Settings() *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
}

func TestProxyUDP6StoreKickRunsOncePerProxyAndKeepsTheAnswer(t *testing.T) {
	var runs atomic.Int32
	release := make(chan struct{})
	store := newProxyUDP6Store(func(context.Context, *connect.ProxySettings) proxyUDP6Result {
		runs.Add(1)
		<-release
		return proxyUDP6Result{Status: proxyUDP6Yes, TCPExit: "2001:db8::1", UDPExit: "2001:db8::1"}
	})
	settings := udp6Settings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	if !store.kick(settings, now) {
		t.Fatal("the first kick did not start a check")
	}
	if store.kick(settings, now) {
		t.Fatal("a second kick started a second check for the same proxy")
	}
	if store.allowed(settings.Key(), now) {
		t.Fatal("allowed before the check finished")
	}
	close(release)
	store.wait.Wait()
	if runs.Load() != 1 {
		t.Fatalf("the probe ran %d times", runs.Load())
	}
	if !store.allowed(settings.Key(), now.Add(time.Hour)) {
		t.Fatal("a fresh yes was not allowed")
	}
	// a fresh answer is not asked again, a stale one is
	if store.kick(settings, now.Add(time.Hour)) {
		t.Fatal("a fresh answer was probed again")
	}
	if !store.kick(settings, now.Add(udp6YesTTL+time.Minute)) {
		t.Fatal("a stale answer was not probed again")
	}
	store.wait.Wait()
}

func TestProxyUDP6StoreTrustWindow(t *testing.T) {
	store := newProxyUDP6Store(nil)
	key := "k"
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	set := func(r proxyUDP6Result) {
		store.mu.Lock()
		store.entries[key] = &proxyUDP6Entry{result: r}
		store.mu.Unlock()
	}
	for _, test := range []struct {
		name string
		r    proxyUDP6Result
		at   time.Time
		want bool
	}{
		{"fresh yes", proxyUDP6Result{Status: proxyUDP6Yes, At: now.Add(-time.Hour)}, now, true},
		{"yes inside twice its window", proxyUDP6Result{Status: proxyUDP6Yes, At: now.Add(-23 * time.Hour)}, now, true},
		{"yes older than twice its window", proxyUDP6Result{Status: proxyUDP6Yes, At: now.Add(-25 * time.Hour)}, now, false},
		{"yes from the future", proxyUDP6Result{Status: proxyUDP6Yes, At: now.Add(time.Hour)}, now, false},
		{"mismatch", proxyUDP6Result{Status: proxyUDP6Mismatch, At: now.Add(-time.Hour)}, now, false},
		{"unknown", proxyUDP6Result{Status: proxyUDP6Unknown, At: now.Add(-time.Minute)}, now, false},
	} {
		set(test.r)
		if got := store.allowed(key, now); got != test.want {
			t.Fatalf("%s: allowed = %v, want %v", test.name, got, test.want)
		}
	}
	if store.allowed("missing", now) {
		t.Fatal("an unknown identity was allowed")
	}
}

func TestProxyUDP6StoreLimitsConcurrentChecks(t *testing.T) {
	release := make(chan struct{})
	store := newProxyUDP6Store(func(context.Context, *connect.ProxySettings) proxyUDP6Result {
		<-release
		return proxyUDP6Result{Status: proxyUDP6Unknown}
	})
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	started := 0
	for i := 0; i < udp6MaxConcurrent+3; i++ {
		settings := &connect.ProxySettings{Network: "tcp", Address: net.JoinHostPort("192.0.2.1", string(rune('1'+i))+"080")}
		if store.kick(settings, now) {
			started++
		}
	}
	if started != udp6MaxConcurrent {
		t.Fatalf("%d checks started at once, want %d", started, udp6MaxConcurrent)
	}
	close(release)
	store.wait.Wait()
}

func withIPv6Gate(t *testing.T, verdict v6Verdict, store *proxyUDP6Store) {
	t.Helper()
	previousVerdict, previousStore := proxyIPv6TCPVerdict, proxyUDP6
	proxyIPv6TCPVerdict = func(string) v6Verdict { return verdict }
	proxyUDP6 = store
	t.Cleanup(func() { proxyIPv6TCPVerdict, proxyUDP6 = previousVerdict, previousStore })
}

func TestProxiedUdpIPv6AllowedDecision(t *testing.T) {
	settings := udp6Settings()
	now := time.Now()
	yes := func() *proxyUDP6Store {
		s := newProxyUDP6Store(nil)
		s.entries[settings.Key()] = &proxyUDP6Entry{result: proxyUDP6Result{Status: proxyUDP6Yes, At: now.Add(-time.Hour)}}
		return s
	}
	for _, test := range []struct {
		name    string
		verdict v6Verdict
		udp     proxyUDPStatus
		store   *proxyUDP6Store
		want    bool
	}{
		{"all four hold", v6Yes, proxyUDPOK, yes(), true},
		{"tcp verdict no", v6No, proxyUDPOK, yes(), false},
		{"tcp verdict unknown", v6Unknown, proxyUDPOK, yes(), false},
		{"ipv4 relay has no udp", v6Yes, proxyUDPNoRelay, yes(), false},
		{"ipv4 relay exit mismatch", v6Yes, proxyUDPExitMismatch, yes(), false},
		{"ipv4 check unknown", v6Yes, proxyUDPUnknown, yes(), false},
		{"no ipv6 udp result", v6Yes, proxyUDPOK, newProxyUDP6Store(nil), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			withIPv6Gate(t, test.verdict, test.store)
			check := stubUdpCheckStore(test.udp)
			check.Check(context.Background(), settings, now)
			if got := proxiedUdpIPv6Allowed(settings, check, now); got != test.want {
				t.Fatalf("allowed = %v, want %v", got, test.want)
			}
		})
	}
}

func TestProxiedUdpIPv6SettingOffRefusesEveryProxy(t *testing.T) {
	settings := udp6Settings()
	now := time.Now()
	store := newProxyUDP6Store(nil)
	store.entries[settings.Key()] = &proxyUDP6Entry{result: proxyUDP6Result{Status: proxyUDP6Yes, At: now.Add(-time.Hour)}}
	withIPv6Gate(t, v6Yes, store)
	check := stubUdpCheckStore(proxyUDPOK)
	check.Check(context.Background(), settings, now)
	if !proxiedUdpIPv6Allowed(settings, check, now) {
		t.Fatal("refused with the default setting")
	}
	state := newControlState()
	if !proxyUDPIPv6EnabledIn(state) || !proxyUDPIPv6EnabledIn(nil) {
		t.Fatal("the setting is not on by default")
	}
	if err := state.set("proxy_udp_ipv6", "off"); err != nil {
		t.Fatal(err)
	}
	previous := globalControlState
	globalControlState = state
	defer func() { globalControlState = previous }()
	if proxiedUdpIPv6Allowed(settings, check, now) || proxiedUdpIPv6Candidate(settings, check) {
		t.Fatal("allowed or a candidate with the setting off")
	}
	if err := validateControlValue("proxy_udp_ipv6", "maybe"); err == nil {
		t.Fatal("an invalid value was accepted")
	}
}

// An ipv6 destination through a proxy that has not passed the checks fails at
// once, never reaches the proxy, and starts the check when the proxy is proven
// for ipv6 TCP.
func TestProxiedIdentityUdpRefusesIpv6AndKicksTheCheck(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	var probes atomic.Int32
	store := newProxyUDP6Store(func(context.Context, *connect.ProxySettings) proxyUDP6Result {
		probes.Add(1)
		return proxyUDP6Result{Status: proxyUDP6Unknown}
	})
	withIPv6Gate(t, v6Yes, store)
	check := stubUdpCheckStore(proxyUDPOK)
	natSettings := proxiedIdentityNatSettings(proxySettings, check)
	dial := func() {
		conn, err := natSettings.UdpBufferSettings.DialContext(context.Background(), "udp", "[2001:db8::1]:443")
		if conn != nil {
			conn.Close()
		}
		if !errors.Is(err, errProxiedUdpIpv6Refused) {
			t.Fatalf("err = %v, want the ipv6 refusal", err)
		}
	}
	// the first refused flow starts the IPv4 check, which fills the cache
	dial()
	waitFor(t, 5*time.Second, "the ipv4 check to be cached", func() bool {
		_, ok := check.cached(proxySettings.Key())
		return ok
	})
	// the second one finds the cached ok result and starts the IPv6 UDP check
	dial()
	store.wait.Wait()
	if probes.Load() == 0 {
		t.Fatal("a refused flow of a proxy proven for ipv6 TCP did not start the ipv6 UDP check")
	}
	if got := server.accepts.Load(); got != 0 {
		t.Fatalf("a refused ipv6 flow connected to the proxy %d times", got)
	}
	if proxiedUdpRefusals.count(proxySettings, proxiedUdpRefusalIpv6) != 2 {
		t.Fatal("the ipv6 refusals were not counted")
	}
}

// The decision is made again after the wait for the check result.
func TestProxiedIdentityUdpRechecksIpv6AfterTheCheckWait(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	store := newProxyUDP6Store(nil)
	store.entries[proxySettings.Key()] = &proxyUDP6Entry{result: proxyUDP6Result{Status: proxyUDP6Yes, At: time.Now().Add(-time.Hour)}}
	withIPv6Gate(t, v6Yes, store)
	state := newControlState()
	previous := globalControlState
	globalControlState = state
	defer func() { globalControlState = previous }()

	var probes atomic.Int32
	check := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		// while the flow waits for this check, an operator turns the feature off
		probes.Add(1)
		state.set("proxy_udp_ipv6", "off")
		return proxyUDPResult{Status: proxyUDPOK}
	})
	// a cached ok result lets the first decision pass, a stale one forces the wait
	check.Check(context.Background(), proxySettings, time.Now().Add(-2*proxyUDPResultTTL))
	// priming turned the feature off, so turn it on again for the dial
	state.set("proxy_udp_ipv6", "on")
	primed := probes.Load()
	natSettings := proxiedIdentityNatSettings(proxySettings, check)
	conn, err := natSettings.UdpBufferSettings.DialContext(context.Background(), "udp", "[2001:db8::1]:443")
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, errProxiedUdpIpv6Refused) {
		t.Fatalf("err = %v, want the ipv6 refusal after the recheck", err)
	}
	if probes.Load() == primed {
		t.Fatal("the dial did not wait for a new check, so the recheck after the wait was not exercised")
	}
	if got := server.accepts.Load(); got != 0 {
		t.Fatalf("the flow reached the proxy %d times", got)
	}
}

// A proxy that passed every check relays an ipv6 datagram and its reply.
func TestProxiedIdentityUdpRelaysIpv6ThroughAProxyThatPassedTheChecks(t *testing.T) {
	echo, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("no ipv6 loopback: %v", err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			echo.WriteToUDP(append([]byte("echo:"), buf[:n]...), from)
		}
	}()
	server := startFakeSocks5(t, "", "", false)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	store := newProxyUDP6Store(nil)
	store.entries[proxySettings.Key()] = &proxyUDP6Entry{result: proxyUDP6Result{Status: proxyUDP6Yes, At: time.Now().Add(-time.Hour)}}
	withIPv6Gate(t, v6Yes, store)
	natSettings := proxiedIdentityNatSettings(proxySettings, stubUdpCheckStore(proxyUDPOK))
	// the first dial fills the check store, an ipv4 flow does it without a relay
	prime, _ := natSettings.UdpBufferSettings.DialContext(context.Background(), "udp", "192.0.2.1:53")
	if prime != nil {
		prime.Close()
	}
	conn, err := natSettings.UdpBufferSettings.DialContext(context.Background(), "udp", echo.LocalAddr().String())
	if err != nil {
		t.Fatalf("a proxy that passed the checks refused an ipv6 flow: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no reply through the relay: %v", err)
	}
	if got := string(buf[:n]); got != "echo:ping" {
		t.Fatalf("reply = %q", got)
	}
}

// An expired IPv4 result is not trusted as a negative answer: the flow starts a
// new check and is refused for now.
func TestProxiedUdpIPv6CandidateRefreshesAnExpiredIPv4Result(t *testing.T) {
	settings := udp6Settings()
	var probes atomic.Int32
	check := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		probes.Add(1)
		return proxyUDPResult{Status: proxyUDPOK}
	})
	storeWithStaleResult := func() {
		check.mu.Lock()
		check.entries[settings.Key()] = &proxyUDPCheckEntry{result: proxyUDPResult{Status: proxyUDPNoRelay}, at: time.Now().Add(-2 * proxyUDPResultTTL)}
		check.mu.Unlock()
	}
	storeWithStaleResult()
	withIPv6Gate(t, v6Yes, newProxyUDP6Store(nil))
	if proxiedUdpIPv6Candidate(settings, check) {
		t.Fatal("an expired result was read as a current one")
	}
	waitFor(t, 5*time.Second, "the expired ipv4 result to be checked again", func() bool { return probes.Load() == 1 })
	waitFor(t, 5*time.Second, "the new ipv4 result to be stored", func() bool { return check.fresh(settings.Key(), time.Now()) })
	if !proxiedUdpIPv6Candidate(settings, check) {
		t.Fatal("the refreshed ok result did not make the proxy a candidate")
	}
}
