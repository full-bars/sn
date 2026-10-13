package provider

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/urnetwork/connect"
)

func TestFamilyServiceUrl(t *testing.T) {
	cases := []struct {
		in   string
		ver  int
		want string
	}{
		{"wss://connect.bringyour.com", 4, "wss://connect-v4.bringyour.com"},
		{"wss://connect.bringyour.com", 6, "wss://connect-v6.bringyour.com"},
		{"https://api.example.com:8443/path?x=1", 6, "https://api-v6.example.com:8443/path?x=1"},
		{"connect.example.com", 4, "connect-v4.example.com"},
		{"wss://127.0.0.1", 4, ""},
		{"wss://127.0.0.1:8080", 4, ""},
		{"wss://[::1]:8080", 6, ""},
		{"wss://localhost", 4, ""},
		{"wss://connect-v4.example.com", 4, ""},
		{"wss://connect-v6.example.com", 6, ""},
		{"wss://connect.example.com", 5, ""},
		{"wss://connect.example.com", 0, ""},
		{"", 4, ""},
		{"wss://", 4, ""},
		{"wss://user@connect.example.com", 4, ""},
	}
	for _, c := range cases {
		if got := familyServiceUrl(c.in, c.ver); got != c.want {
			t.Errorf("familyServiceUrl(%q, %d) = %q, want %q", c.in, c.ver, got, c.want)
		}
	}
}

func TestFamilyTransportKillSwitch(t *testing.T) {
	for value, wantKilled := range map[string]bool{
		"": false, "1": false, "on": false, "true": false, "yes": false, "dualstack": false,
		"0": true, "off": true, "false": true, "no": true, " OFF ": true, "False": true,
	} {
		t.Setenv(familyTransportKillEnv, value)
		if got := familyTransportsKilled(); got != wantKilled {
			t.Errorf("%s=%q killed = %t, want %t", familyTransportKillEnv, value, got, wantKilled)
		}
	}
	os.Unsetenv(familyTransportKillEnv)
	if familyTransportsKilled() {
		t.Errorf("an unset %s must leave the feature on", familyTransportKillEnv)
	}
}

func TestIsUsableGlobalIpv6(t *testing.T) {
	for addr, want := range map[string]bool{
		"2001:4860::1":        true,
		"2a01::1":             true,
		"2606:65c0:40:277::1": true,
		"fd7a:115c:a1e0::1":   false, // Tailscale ULA
		"fe80::1":             false,
		"::1":                 false,
		"2001:db8::1":         false,
		"2002::1":             false,
		"2001:0::1":           false,
		"64:ff9b::102:304":    false,
		"64:ff9b:1::1":        false,
		"::ffff:1.2.3.4":      false,
		"::102:304":           false,
		"100.64.0.1":          false, // CGNAT is a v4 range
		"10.0.0.1":            false,
	} {
		if got := isUsableGlobalIpv6(net.ParseIP(addr)); got != want {
			t.Errorf("isUsableGlobalIpv6(%s) = %t, want %t", addr, got, want)
		}
	}
	if isUsableGlobalIpv6(nil) {
		t.Error("nil must not be usable")
	}
}

func TestProbeIpv6With(t *testing.T) {
	good := func() (net.IP, error) { return net.ParseIP("2001:4860::1"), nil }
	for name, c := range map[string]struct {
		supported bool
		source    func() (net.IP, error)
		wantOk    bool
		wantWhy   string
	}{
		"unsupported": {false, good, false, "no-global-v6-address"},
		"no route": {true, func() (net.IP, error) { return nil, net.UnknownNetworkError("x") },
			false, "no-v6-route"},
		"ula source": {true, func() (net.IP, error) { return net.ParseIP("fd7a:115c:a1e0::1"), nil },
			false, "v6-source-not-global"},
		"good": {true, good, true, "ok"},
	} {
		ok, why := probeIpv6With(func(int) bool { return c.supported }, c.source)
		if ok != c.wantOk || why != c.wantWhy {
			t.Errorf("%s: probe = (%t, %q), want (%t, %q)", name, ok, why, c.wantOk, c.wantWhy)
		}
	}
}

func TestUseFamilyTransportsDecisionOrder(t *testing.T) {
	const allowed = "wss://connect.bringyour.com"
	proxy := &connect.ProxySettings{Address: "127.0.0.1:1"}
	probeOk := func() (bool, string) { return true, "ok" }

	defer func(saved func() (bool, string)) { hostIpv6Probe = saved }(hostIpv6Probe)
	for name, c := range map[string]struct {
		kill    string
		proxy   *connect.ProxySettings
		native  bool
		url     string
		probe   func() (bool, string)
		wantUse bool
		reason  string
	}{
		"all good": {"", nil, true, allowed, probeOk, true, "dualstack"},
		// the kill switch wins over every other condition
		"kill wins":   {"0", proxy, false, "wss://x", func() (bool, string) { return false, "no-v6-route" }, false, "kill-switch"},
		"proxied":     {"", proxy, true, allowed, probeOk, false, "proxied-identity"},
		"not native":  {"", nil, false, allowed, probeOk, false, "not-native"},
		"ip literal":  {"", nil, true, "wss://127.0.0.1", probeOk, false, "no-family-urls"},
		"custom host": {"", nil, true, "wss://connect.example.com", probeOk, false, "connect-host-not-allowlisted"},
		"probe says no": {"", nil, true, allowed, func() (bool, string) { return false, "v6-source-not-global" },
			false, "v6-source-not-global"},
	} {
		t.Setenv(familyTransportKillEnv, c.kill)
		hostIpv6Probe = c.probe
		use, reason := useFamilyTransports(c.proxy, c.native, c.url)
		if use != c.wantUse || reason != c.reason {
			t.Errorf("%s: got (%t, %q), want (%t, %q)", name, use, reason, c.wantUse, c.reason)
		}
	}
}

// The H3 socket of a transport pinned to IPv6 must be a v6 socket and any other
// a v4 socket, so the direct identity's H3 works on both pins of the group.
func TestListenDirectH3UdpFamily(t *testing.T) {
	conn4, err := listenDirectH3Udp(4)
	if err != nil {
		t.Fatal(err)
	}
	defer conn4.Close()
	if conn4.LocalAddr().(*net.UDPAddr).IP.To4() == nil {
		t.Errorf("pin 4 socket is not v4: %s", conn4.LocalAddr())
	}
	conn0, err := listenDirectH3Udp(0)
	if err != nil {
		t.Fatal(err)
	}
	conn0.Close() // the unpinned socket is unchanged: just prove it still opens
	conn6, err := listenDirectH3Udp(6)
	if err != nil {
		t.Skipf("no IPv6 UDP in this environment: %s", err)
	}
	defer conn6.Close()
	if conn6.LocalAddr().(*net.UDPAddr).IP.To4() != nil || conn6.LocalAddr().(*net.UDPAddr).IP == nil {
		t.Errorf("pin 6 socket is not v6: %s", conn6.LocalAddr())
	}
}

// Exactly one of the legacy transport and the group exists, and the interface
// returned is never a typed nil: the renewal watcher nil-checks it.
func TestNewProviderPlatformTransportExactlyOneConcrete(t *testing.T) {
	for _, useFamily := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		clientStrategySettings := connect.DefaultClientStrategySettings()
		clientStrategy := connect.NewClientStrategy(ctx, clientStrategySettings)
		settings := connect.DefaultPlatformTransportSettings()
		defaultBudget := settings.PlatformTransportBudget
		transport, legacy, group := newProviderPlatformTransport(
			ctx, clientStrategySettings, clientStrategy, connect.NewRouteManager(ctx, "test"),
			"wss://connect.bringyour.com",
			&connect.ClientAuth{ByJwt: "testing", InstanceId: connect.NewId(), AppVersion: "testing"},
			settings, useFamily,
		)
		if transport == nil {
			t.Fatalf("useFamily=%t: nil transport interface", useFamily)
		}
		if (legacy != nil) == (group != nil) {
			t.Fatalf("useFamily=%t: legacy=%v group=%v, want exactly one", useFamily, legacy != nil, group != nil)
		}
		// only the group gets a budget sized for three transports
		if useFamily == (settings.PlatformTransportBudget == defaultBudget) {
			t.Fatalf("useFamily=%t: the default budget must be replaced for the group and kept otherwise", useFamily)
		}
		if useFamily {
			if group == nil || providerPlatformTransport(group) != transport {
				t.Fatal("the group must be the returned transport")
			}
		} else if legacy == nil || providerPlatformTransport(legacy) != transport {
			t.Fatal("the legacy transport must be the returned transport")
		}
		transport.Close()
		cancel()
		clientStrategy.Close()
	}
}

// The renewal watcher keeps working with an unset transport.
func TestRenewalTransportAuthSetterNilIsNil(t *testing.T) {
	var cfg proxyJWTWatcherConfig
	if cfg.Transport != nil {
		t.Fatal("an unset Transport must be a nil interface")
	}
}

type familyTestPlatform struct {
	server *httptest.Server
	mu     sync.Mutex
	intent []string
	conns  []*websocket.Conn
}

// newFamilyTestPlatform is a websocket platform on one loopback family that
// echoes the first frame (the auth frame) so a transport reads as connected.
func newFamilyTestPlatform(t *testing.T, network string, hostPort string) *familyTestPlatform {
	t.Helper()
	listener, err := net.Listen(network, hostPort)
	if err != nil {
		return nil
	}
	platform := &familyTestPlatform{}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	platform.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		platform.mu.Lock()
		platform.intent = append(platform.intent, r.Header.Get(connect.HeaderIpFamily))
		platform.conns = append(platform.conns, ws)
		platform.mu.Unlock()
		first := true
		for {
			messageType, message, err := ws.ReadMessage()
			if err != nil {
				ws.Close()
				return
			}
			if first {
				first = false
				if err := ws.WriteMessage(messageType, message); err != nil {
					ws.Close()
					return
				}
			}
		}
	}))
	platform.server.Listener = listener
	platform.server.Start()
	t.Cleanup(func() {
		platform.mu.Lock()
		for _, ws := range platform.conns {
			ws.Close()
		}
		platform.mu.Unlock()
		platform.server.Close()
	})
	return platform
}

func (self *familyTestPlatform) url() string {
	return "ws://" + self.server.Listener.Addr().String()
}

func (self *familyTestPlatform) intents() []string {
	self.mu.Lock()
	defer self.mu.Unlock()
	return append([]string(nil), self.intent...)
}

func familyTestWait(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// The group, built the way the provider builds it, connects one transport per
// family and declares each, stays up when only one family is left, and the
// status reporter publishes then clears its snapshot.
func TestFamilyGroupLoopbackEndToEnd(t *testing.T) {
	platform4 := newFamilyTestPlatform(t, "tcp4", "127.0.0.1:0")
	platform6 := newFamilyTestPlatform(t, "tcp6", "[::1]:0")
	if platform4 == nil || platform6 == nil {
		t.Skip("needs IPv4 and IPv6 loopback")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategy := connect.NewClientStrategy(ctx, clientStrategySettings)
	defer clientStrategy.Close()

	settings := connect.DefaultPlatformTransportSettings()
	settings.PinnedReconnectMaxTimeout = 200 * time.Millisecond
	// H3 is on for the direct identity in production; the fake platform speaks
	// only websocket, so keep the loopback test to H1
	applyH3ModeToSettings(settings, false)
	groupSettings := connect.DefaultFamilyPlatformTransportGroupSettings()
	groupSettings.StandbyDelay = time.Hour

	group := newProviderFamilyGroup(
		ctx, clientStrategySettings, clientStrategy, connect.NewRouteManager(ctx, "test"),
		platform4.url(), platform4.url(), platform6.url(),
		&connect.ClientAuth{ByJwt: "testing", InstanceId: connect.NewId(), AppVersion: "testing"},
		settings, groupSettings,
	)
	defer group.Close()
	stop := startFamilyStatusReporter(ctx, group)

	if !familyTestWait(15*time.Second, func() bool {
		return 0 < len(platform4.intents()) && 0 < len(platform6.intents())
	}) {
		t.Fatalf("each family server must see a pinned connection: v4=%v v6=%v",
			platform4.intents(), platform6.intents())
	}
	// a platform transport opens several connections (a plain one does too), so
	// check that every one a pin makes declares that pin's family and that the
	// parked standby, which would declare none, has not dialed
	for _, intent := range platform4.intents() {
		if intent != "4" {
			t.Errorf("a connection to the v4 platform declared family %q", intent)
		}
	}
	for _, intent := range platform6.intents() {
		if intent != "6" {
			t.Errorf("a connection to the v6 platform declared family %q", intent)
		}
	}
	if !familyTestWait(15*time.Second, group.IsConnected) {
		t.Fatal("the group never reported connected")
	}
	if !familyTestWait(5*time.Second, func() bool {
		snapshot := familyTransportLast.Load()
		return snapshot != nil && snapshot.HasIpv4 && snapshot.HasIpv6
	}) {
		t.Fatal("the status reporter never published a snapshot")
	}

	// lose the v6 platform: the group stays connected on v4
	platform6.mu.Lock()
	for _, ws := range platform6.conns {
		ws.Close()
	}
	platform6.mu.Unlock()
	platform6.server.Close()
	time.Sleep(500 * time.Millisecond)
	if !group.IsConnected() {
		t.Error("the group must stay connected while the v4 pin is up")
	}

	stop()
	if familyTransportLast.Load() != nil {
		t.Error("stopping the reporter must clear the published snapshot")
	}
}

func TestFamilyTransportMetricsLint(t *testing.T) {
	familyTransportDecisionLast.Store(&familyTransportDecision{Use: true, Reason: "dualstack"})
	familyTransportLast.Store(&familyTransportSnapshot{HasIpv4: true, HasIpv6: true})
	defer familyTransportDecisionLast.Store(nil)
	defer familyTransportLast.Store(nil)

	var b strings.Builder
	writeFamilyTransportMetrics(&b)
	body := b.String()
	for _, want := range []string{
		`urnet_dualstack_mode{mode="group",reason="dualstack"} 1`,
		`urnet_dualstack_transport_state{transport="ipv4",state=`,
		`urnet_dualstack_transport_state{transport="standby",state=`,
		`urnet_dualstack_standby_active 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
	if errs := Lint(body); len(errs) != 0 {
		t.Errorf("promtext lint: %v\n%s", errs, body)
	}

	familyTransportDecisionLast.Store(&familyTransportDecision{Use: false, Reason: "kill-switch"})
	familyTransportLast.Store(nil)
	b.Reset()
	writeFamilyTransportMetrics(&b)
	if !strings.Contains(b.String(), `urnet_dualstack_mode{mode="legacy",reason="kill-switch"} 1`) ||
		strings.Contains(b.String(), "urnet_dualstack_transport_state") {
		t.Errorf("legacy path metrics wrong:\n%s", b.String())
	}
}

// provideWithProxy must decide the transport shape after the H3 eligibility is
// registered, never build a single transport directly, and register the live
// H3 datagram toggle only for the single transport (the group's settings copies
// cannot take the by-value offer).
func TestProvideWithProxyBuildsPlatformTransportThroughFamilyDecision(t *testing.T) {
	src, err := os.ReadFile("provide.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	eligible := strings.Index(text, "registerH3RunningEligible(proxySettings, isNative)")
	decide := strings.Index(text, "useFamilyTransports(proxySettings, isNative, st.connectUrl)")
	if eligible < 0 || decide < 0 || decide < eligible {
		t.Fatal("useFamilyTransports must be called after registerH3RunningEligible")
	}
	if strings.Contains(text, "connect.NewPlatformTransport(") {
		t.Error("provideWithProxy must build through newProviderPlatformTransport")
	}
	if !strings.Contains(text, "if legacyTransport != nil {\n\t\t\tregisterH3DatagramTarget(platformSettings, legacyTransport)") {
		t.Error("the H3 datagram target must be registered only for the legacy transport")
	}
}
