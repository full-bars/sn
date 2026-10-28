package miner

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/miner/bandwidth"
)

// h3_packet_conn_test.go pins the proxied-identity H3 endpoint port
// (h3_packet_conn.go).
//
// The property under test is a privacy one, not a performance one: a proxied
// identity's QUIC must leave through its proxy's UDP relay, and when the proxy
// cannot relay UDP the transport must fail over to a TCP carrier instead of
// falling back to a socket on this host.

// stubH3UDPVerdict remembers a UDP-path verdict for one identity, so the
// factory's gate reads a settled answer without probing a STUN server. The
// entries are seeded directly (not via a probe function) for the same reason
// h3TestUDPAdmitsAll seeds them: a probe would also fire the verdict side
// effects (log line, relaunch pass), which belong to other tests.
func stubH3UDPVerdict(t *testing.T, settings *connect.ProxySettings, status proxyUDPStatus) {
	t.Helper()
	previous := globalProxyUDPCheck
	t.Cleanup(func() { globalProxyUDPCheck = previous })
	store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{Status: status}
	})
	store.entries[settings.Key()] = &proxyUDPCheckEntry{
		at:     time.Now(),
		result: proxyUDPResult{Status: status},
	}
	globalProxyUDPCheck = store
}

// A proxied identity's H3 endpoint is the proxy's relay: a datagram written to
// the packet conn reaches the proxy with a SOCKS5 UDP header addressed to the
// QUIC destination, and the reply comes back through it. Without this the
// identity's QUIC is a socket on this host.
func TestH3FactoryProxiedSlotRelaysThroughItsProxy(t *testing.T) {
	proxy := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.7", "203.0.113.7")
	settings := proxy.settings()
	stubH3UDPVerdict(t, settings, proxyUDPOK)

	bw := &bandwidth.ProxyBandwidth{}
	factory := newH3PacketConnFactory(settings, bw, settings.Address)
	if factory == nil {
		t.Fatal("a proxied identity got no H3 factory, so its QUIC would egress from the host socket")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pc, err := factory(ctx)
	if err != nil {
		t.Fatalf("the factory refused a proxy whose relay works: %v", err)
	}
	defer pc.Close()

	// The fake relay only answers a well-formed relayed datagram of at least
	// its STUN minimum, so a reply proves the datagram went through the proxy
	// rather than straight to the destination.
	destination := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 20), Port: 443}
	if _, err := pc.WriteTo(make([]byte, 24), destination); err != nil {
		t.Fatalf("WriteTo through the proxied packet conn: %v", err)
	}
	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, from, err := pc.ReadFrom(make([]byte, 2048)); err != nil {
		t.Fatalf("no reply through the proxy relay: %v", err)
	} else if from == nil {
		t.Fatal("a relayed reply came back with no source address")
	}
	if n := proxy.associateCount(); n == 0 {
		t.Fatal("the packet conn was built without a UDP ASSOCIATE on the proxy")
	}
	if bw.TotalTx.Load() == 0 || bw.TotalRx.Load() == 0 {
		t.Fatalf("proxied QUIC bytes were not counted (tx=%d rx=%d)", bw.TotalTx.Load(), bw.TotalRx.Load())
	}
}

// connect asks the factory once per H3 carrier dial and closes what it gets.
// Each call is its own association, so one dead relay does not poison the
// identity's next dial, and nothing is left open on the proxy when a transport
// generation is retired.
func TestH3FactoryIsConsultedPerDial(t *testing.T) {
	proxy := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.7", "203.0.113.7")
	settings := proxy.settings()
	stubH3UDPVerdict(t, settings, proxyUDPOK)

	factory := newH3PacketConnFactory(settings, nil, settings.Address)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first, err := factory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := factory(ctx)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("the factory returned the same endpoint twice; connect closes each one, so a shared socket would be closed under the other dial")
	}
	if n := proxy.associateCount(); n != 2 {
		t.Fatalf("UDP ASSOCIATE count = %d, want 2 (one per dial)", n)
	}
	first.Close()
	second.Close()
}

// The fail-closed half: when the proxy cannot relay UDP, the factory returns an
// error and no packet conn, so the transport takes a TCP carrier that already
// goes through the proxy. A host socket here would be the leak.
func TestH3FactoryProxiedSlotNeverFallsBackToAHostSocket(t *testing.T) {
	t.Run("udp check says no relay", func(t *testing.T) {
		proxy := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.7", "203.0.113.7")
		settings := proxy.settings()
		stubH3UDPVerdict(t, settings, proxyUDPNoRelay)

		factory := newH3PacketConnFactory(settings, nil, settings.Address)
		if factory == nil {
			t.Fatal("a proxied identity got no factory")
		}
		pc, err := factory(context.Background())
		if err == nil {
			pc.Close()
			t.Fatal("the factory returned an endpoint for a proxy with no usable UDP relay")
		}
		if pc != nil {
			t.Fatalf("the factory returned a non-nil endpoint alongside its error: %T", pc)
		}
	})

	t.Run("udp check says exit mismatch", func(t *testing.T) {
		proxy := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.7", "198.51.100.9")
		settings := proxy.settings()
		stubH3UDPVerdict(t, settings, proxyUDPExitMismatch)

		if pc, err := newH3PacketConnFactory(settings, nil, settings.Address)(context.Background()); err == nil {
			pc.Close()
			t.Fatal("the factory returned an endpoint for a proxy whose UDP exit is not its TCP exit")
		}
	})

	t.Run("proxy refuses the association", func(t *testing.T) {
		// The verdict is the optimistic one (a relay that could not be
		// checked yet is admitted), so this is the case where the check is
		// not what saves us: the dial itself must fail closed.
		proxy := newFakeSocks5Proxy(t, fakeUDPRelayDead, "203.0.113.7", "203.0.113.7")
		settings := proxy.settings()
		stubH3UDPVerdict(t, settings, proxyUDPUnknown)

		if pc, err := newH3PacketConnFactory(settings, nil, settings.Address)(context.Background()); err == nil {
			pc.Close()
			t.Fatal("the factory returned an endpoint although the proxy refused UDP ASSOCIATE")
		}
	})
}

// The direct identity must keep the engine's own socket: it is the box's real
// address, and connect binds that socket to the physical egress interface. A
// factory here would replace it with an unpinned wildcard socket, which on a
// node that provides a tunnel can loop the platform's QUIC back into the
// tunnel. The miner's direct slot always has a bandwidth tracker, so "no proxy"
// alone has to decide this.
func TestH3FactoryLeavesTheDirectSlotAlone(t *testing.T) {
	if factory := newH3PacketConnFactory(nil, nil, ""); factory != nil {
		t.Fatal("the direct identity got an H3 factory; the engine's own socket should be kept")
	}
	if factory := newH3PacketConnFactory(nil, &bandwidth.ProxyBandwidth{}, ""); factory != nil {
		t.Fatal("the direct identity got an H3 factory because it had a byte counter; its egress must stay the engine's pinned socket")
	}
}

// The wiring at the seam the SDK reads: DeviceLocalSettings
// .ProviderDialContextSettings.PacketConnFactory becomes the provider carrier's
// H3PacketConnFactory (urnetwork/sdk device_local.go,
// newDeviceLocalPlatformTransportSettingsForProfile). So the settings object a
// proxied slot is built with must carry the factory, and a direct slot's must
// not — and both must survive the bandwidth wrap, which rebuilds the struct.
func TestProviderSlotDialSettingsCarriesTheH3FactoryThroughTheWrap(t *testing.T) {
	direct := providerSlotDialSettings(nil, nil, &bandwidth.ProxyBandwidth{}, "")
	if direct == nil || direct.DialContext == nil {
		t.Fatal("the direct slot's dial settings were not wrapped")
	}
	if direct.PacketConnFactory != nil {
		t.Fatal("the direct slot's dial settings carry an H3 endpoint factory, so its QUIC would leave from a wildcard socket instead of the engine's pinned one")
	}

	proxy := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.7", "203.0.113.7")
	setVerdict := proxy.settings()
	stubH3UDPVerdict(t, setVerdict, proxyUDPOK)
	proxied := providerSlotDialSettings(nil, setVerdict, &bandwidth.ProxyBandwidth{}, proxy.address())
	if proxied == nil || proxied.DialContext == nil {
		t.Fatal("the proxied slot's dial settings were not wrapped")
	}
	if proxied.PacketConnFactory == nil {
		t.Fatal("the bandwidth wrap dropped the proxied slot's H3 endpoint factory, so the identity's QUIC would egress from the host socket")
	}
	if pc, err := proxied.PacketConnFactory(context.Background()); err != nil {
		// The relay is real here; a refusal would mean the seam carried
		// something other than the proxy factory.
		t.Fatalf("the factory carried through the wrap did not dial the proxy: %v", err)
	} else {
		pc.Close()
	}

	// A proxied slot must ignore the caller's base dial so it cannot bypass
	// the configured proxy.
	called := false
	base := &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			called = true
			return nil, nil
		},
	}
	conn, err := providerSlotDialSettings(base, setVerdict, nil, proxy.address()).DialContext(context.Background(), "tcp", "destination.invalid:443")
	if err != nil {
		t.Fatalf("proxied dial failed: %v", err)
	}
	conn.Close()
	if called {
		t.Fatal("the proxied slot used the base dial, bypassing its proxy")
	}
}

func TestProviderSlotDialSettingsRoutesStreamsThroughProxy(t *testing.T) {
	proxy := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.7", "203.0.113.7")
	settings := proxy.settings()
	stubH3UDPVerdict(t, settings, proxyUDPOK)
	proxied := providerSlotDialSettings(nil, settings, nil, proxy.address())
	if proxied.DialContext == nil {
		t.Fatal("proxied slot has no stream dial")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := proxied.DialContext(ctx, "tcp", "destination.invalid:443")
	if err != nil {
		t.Fatalf("SOCKS CONNECT through proxy failed: %v", err)
	}
	conn.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := listener.Accept(); accepted <- c }()
	direct := providerSlotDialSettings(nil, nil, nil, "")
	conn, err = direct.DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("direct slot did not dial directly: %v", err)
	}
	conn.Close()
	select {
	case c := <-accepted:
		if c == nil {
			t.Fatal("direct listener accepted no connection")
		}
		c.Close()
	case <-ctx.Done():
		t.Fatal("direct connection was not accepted")
	}
}

// A direct slot keeps whatever H3 endpoint its caller supplied. The test-egress
// seam is such a caller (testEgressDialContextForIP binds every path of a
// testnet identity to one loopback source), and the wrap used to drop that
// field, so those identities' H3 carriers left from the default source instead
// of the bound one. The proxied slot replaces it: its proxy must win.
func TestProviderSlotDialSettingsKeepsADirectSlotsOwnEndpoint(t *testing.T) {
	egress, err := testEgressDialContextForIP("127.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	direct := providerSlotDialSettings(egress, nil, &bandwidth.ProxyBandwidth{}, "")
	if direct.PacketConnFactory == nil {
		t.Fatal("the wrap dropped the test-egress slot's own H3 endpoint, so its QUIC would egress from the default source address")
	}
	pc, err := direct.PacketConnFactory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	address, ok := pc.LocalAddr().(*net.UDPAddr)
	if !ok || !address.IP.Equal(net.IPv4(127, 0, 0, 2)) {
		t.Fatalf("the direct slot's H3 endpoint bound %v, want the test-egress source 127.0.0.2", pc.LocalAddr())
	}

	fake := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.7", "203.0.113.7")
	settings := fake.settings()
	stubH3UDPVerdict(t, settings, proxyUDPOK)
	if n := fake.associateCount(); n != 0 {
		t.Fatalf("the fake proxy saw %d associations before the proxied endpoint was built", n)
	}
	proxied := providerSlotDialSettings(egress, settings, nil, fake.address())
	ppc, err := proxied.PacketConnFactory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer ppc.Close()
	if n := fake.associateCount(); n != 1 {
		t.Fatalf("the proxied slot's endpoint did not go to the proxy (associations = %d); it kept the test-egress endpoint", n)
	}
}

// The network space's own ConnectSettings are a slot's second QUIC path.
// connect registers the api's alt-h3 and alt-whodis carriers for every space
// unconditionally (net_http.go, newAltDialers) and they take their endpoint
// from ConnectSettings.DialContextSettings.PacketConnFactory, never from
// ProxySettings (net_extender.go, openExtenderPacketConn falls through to
// net.ListenUDP on the host). So a proxied slot without this seam sends its
// api request, and the jwt that rides it, from this node's address while the
// proxy carries none of it.
//
// This builds the slot the way run.go does — DefaultClientStrategySettings,
// ProxySettings set, no DialContextSettings for a proxied slot — and asserts
// at the object the sdk is handed.
func TestNetworkSpaceConnectSettingsPutsAProxiedSlotOnItsProxy(t *testing.T) {
	proxy := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.7", "203.0.113.7")
	settings := proxy.settings()
	stubH3UDPVerdict(t, settings, proxyUDPOK)
	bw := &bandwidth.ProxyBandwidth{}

	proxied := connect.DefaultClientStrategySettings()
	proxied.ProxySettings = settings
	if proxied.ConnectSettings.DialContextSettings != nil {
		t.Fatal("the proxied slot starts with a dial context settings; run.go installs none for a proxy")
	}
	proxied.ConnectSettings = networkSpaceConnectSettings(proxied.ConnectSettings, settings, bw, settings.Address)
	if proxied.ConnectSettings.DialContextSettings == nil || proxied.ConnectSettings.DialContextSettings.PacketConnFactory == nil {
		t.Fatal("the proxied slot's network space has no QUIC endpoint factory, so its alt-h3/alt-whodis api dials would leave from the host socket")
	}
	if proxied.ConnectSettings.ProxySettings != settings {
		t.Fatal("the network space lost the slot's proxy")
	}
	pc, err := proxied.ConnectSettings.DialContextSettings.PacketConnFactory(context.Background())
	if err != nil {
		t.Fatalf("the network space's QUIC endpoint did not dial the proxy: %v", err)
	}
	pc.Close()
	if n := proxy.associateCount(); n != 1 {
		t.Fatalf("UDP associations on the proxy = %d, want 1: the network space's QUIC endpoint is not the proxy's relay", n)
	}

	// The stream half must stay on the proxy too. 203.0.113.9 is unroutable
	// here, so a successful dial is only possible through the fake proxy.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := proxied.ConnectSettings.DialContext(ctx, "tcp", "203.0.113.9:443")
	if err != nil {
		t.Fatalf("the network space's dial did not go through the proxy: %v", err)
	}
	conn.Close()

	// The direct slot keeps the engine's own dial. WrapConnectSettingsTotal
	// leaves a setting with neither a proxy nor a dial of its own alone, which
	// is deliberate: installing a dial context there would replace connect's
	// own dual-stack address race with a plain dial. Nothing on a direct
	// network space is a client either, so the only cost is that this node's
	// own control-plane bytes go uncounted on that path — the same trade the
	// provider makes for its direct identity.
	direct := connect.DefaultClientStrategySettings()
	direct.ConnectSettings = networkSpaceConnectSettings(direct.ConnectSettings, nil, bw, "")
	if direct.ConnectSettings.DialContextSettings != nil {
		t.Fatal("the direct slot's network space grew a dial context, replacing the engine's own dual-stack dial")
	}
	if direct.ConnectSettings.ProxySettings != nil {
		t.Fatal("the direct slot's network space has a proxy")
	}

	// The provider-device path is unchanged by this seam: the same proxy gets
	// the same kind of factory, and the direct slot still gets none.
	if got := providerSlotDialSettings(nil, settings, bw, settings.Address); got == nil || got.PacketConnFactory == nil {
		t.Fatal("the provider-device path lost the proxied slot's H3 endpoint factory")
	}
	if got := providerSlotDialSettings(nil, nil, bw, ""); got == nil || got.PacketConnFactory != nil {
		t.Fatal("the provider-device path grew an H3 endpoint factory for the direct slot")
	}

	// The network token renewal dials the api on the first provider's route,
	// so a proxied renewal is the same api-under-a-proxy case and takes the
	// same seam.
	renewal := networkTokenRenewalStrategySettings(settings, nil)
	if renewal.ConnectSettings.DialContextSettings == nil || renewal.ConnectSettings.DialContextSettings.PacketConnFactory == nil {
		t.Fatal("the proxied network token renewal has no QUIC endpoint factory, so its api call could leave from the host socket")
	}
	if got := networkTokenRenewalStrategySettings(nil, nil); got.ConnectSettings.DialContextSettings != nil {
		t.Fatal("the direct network token renewal lost the engine's own dial path")
	}
	// An explicit dialer (the test-egress source) is the run's egress for this
	// request and is left exactly as the caller set it.
	egress := &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return nil, nil
		},
	}
	explicit := networkTokenRenewalStrategySettings(settings, egress)
	if explicit.ConnectSettings.DialContextSettings != egress {
		t.Fatal("the renewal replaced the caller's explicit dialer")
	}
}

// TestNetworkSpaceControlPlaneIsNotAClient pins the accounting class of every
// dial the slot's network space makes: the api, auth and H1 tunnel are the
// node's OWN traffic to the platform. They are bytes on this node's link (so
// they belong in Total) but no client is behind them, so they must never move
// Clients — that is what made a freshly authenticated, unserving slot report
// clients=1/serving=1 and fire the traffic marker.
//
// The client half is the session wrap on the relay-egress path
// (bandwidth.WrapConnectSettings with session=true), proven here on the same
// proxy so the test also shows the two wraps differ in exactly one counter.
func TestNetworkSpaceControlPlaneIsNotAClient(t *testing.T) {
	proxy := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.7", "203.0.113.7")
	settings := proxy.settings()
	stubH3UDPVerdict(t, settings, proxyUDPOK)
	bw := &bandwidth.ProxyBandwidth{}

	// The slot's network space, built exactly as run.go builds it.
	space := connect.DefaultClientStrategySettings()
	space.ProxySettings = settings
	space.ConnectSettings = networkSpaceConnectSettings(space.ConnectSettings, settings, bw, settings.Address)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := space.ConnectSettings.DialContext(ctx, "tcp", "203.0.113.9:443")
	if err != nil {
		t.Fatalf("the control-plane dial did not go through the proxy: %v", err)
	}
	if _, err := conn.Write([]byte("auth")); err != nil {
		t.Fatalf("writing on the control-plane connection: %v", err)
	}

	// While the dial is OPEN — which is how a slot's api/tunnel connection
	// spends its life — it must still not read as a client.
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("Clients = %d with one control-plane dial open, want 0: the node's own auth dial must not read as a client", got)
	}
	conn.Close()
	if got := bw.TotalTx.Load(); got == 0 {
		t.Fatal("the control-plane dial was not counted into Total")
	}
	if got := bw.BillableRx.Load() + bw.BillableTx.Load(); got != 0 {
		t.Fatalf("billable = %d after a control-plane dial, want 0: billable is relay accounting, not this node's link", got)
	}

	// A real client flow on the relay-egress seam is still a session: one live
	// client while the connection is open, none after it closes.
	relay := connect.DefaultClientStrategySettings()
	relay.ProxySettings = settings
	relay.ConnectSettings = bandwidth.WrapConnectSettings(relay.ConnectSettings, bw, settings.Address, newH3PacketConnFactory)
	clientConn, err := relay.ConnectSettings.DialContext(ctx, "tcp", "203.0.113.9:443")
	if err != nil {
		t.Fatalf("the relay-egress dial did not go through the proxy: %v", err)
	}
	if got := bw.Clients.Load(); got != 1 {
		t.Fatalf("Clients = %d while a client flow is open, want 1", got)
	}
	clientConn.Close()
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("Clients = %d after the client flow closed, want 0", got)
	}
}

// h3TestRelayedDatagram sends one datagram from a slot's QUIC endpoint to an
// address nothing on this host can reach and requires a reply. The fake relay
// only answers a well-formed relayed datagram, so the reply is proof the
// datagram left through the proxy: a host socket would have sent it straight to
// the destination and heard nothing.
func h3TestRelayedDatagram(t *testing.T, pc net.PacketConn) {
	t.Helper()
	destination := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 20), Port: 443}
	if _, err := pc.WriteTo(make([]byte, 24), destination); err != nil {
		t.Fatalf("WriteTo through the slot's QUIC endpoint: %v", err)
	}
	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, from, err := pc.ReadFrom(make([]byte, 2048)); err != nil {
		t.Fatalf("no reply through the proxy relay: %v", err)
	} else if from == nil {
		t.Fatal("a relayed reply came back with no source address")
	}
}

// The slot the run actually builds.
//
// run.go used to assemble a slot's settings inline in its provider worker,
// where no test could reach them: the tests around it called
// providerSlotDialSettings or the wrap directly, so a call site could hand
// connect a proxied slot with no QUIC endpoint and nothing failed. The
// assembly is now slotClientStrategySettings, and this is the slot it produces
// — ProxySettings set, no DialContextSettings of its own (a non-nil one
// bypasses ProxySettings in connect's dial path), the counter keyed by proxy
// identity, and, through the wrap, a QUIC endpoint that is the proxy's relay.
func TestRunBuiltProxiedSlotSendsItsQuicThroughTheProxy(t *testing.T) {
	proxy := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.7", "203.0.113.7")
	proxySettings := proxy.settings()
	stubH3UDPVerdict(t, proxySettings, proxyUDPOK)

	// The run's test-egress seam. A proxied slot must never install it: a
	// non-nil DialContextSettings bypasses ProxySettings in connect's dial
	// path, so reaching this dialer is the leak the seam exists to prevent.
	egressCalls := 0
	egress := &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			egressCalls++
			return nil, errors.New("the run used its test-egress dialer for a proxied slot")
		},
	}

	registry := bandwidth.NewRegistry()
	strategy, bw, proxyAddr := slotClientStrategySettings(proxySettings, registry, egress)
	if strategy.ConnectSettings.ProxySettings != proxySettings {
		t.Fatal("the slot lost its proxy")
	}
	if proxyAddr != proxySettings.Address {
		t.Fatalf("proxyAddr = %q, want %q", proxyAddr, proxySettings.Address)
	}
	if bw == nil || bw != registry.Register(proxyIdentityBandwidthIndex(proxySettings)) {
		t.Fatal("the slot's counter is not the registry's, keyed by proxy identity")
	}
	dialSettings := strategy.ConnectSettings.DialContextSettings
	if dialSettings == nil {
		t.Fatal("the slot's network space has no dial settings at all")
	}
	if dialSettings.PacketConnFactory == nil {
		t.Fatal("the proxied slot's network space has no QUIC endpoint, so its alt-h3/alt-whodis dials would come from a host socket")
	}
	pc, err := dialSettings.PacketConnFactory(context.Background())
	if err != nil {
		t.Fatalf("the slot's QUIC endpoint did not dial the proxy: %v", err)
	}
	h3TestRelayedDatagram(t, pc)
	pc.Close()
	if n := proxy.associateCount(); n != 1 {
		t.Fatalf("UDP associations on the slot's proxy = %d, want 1", n)
	}
	if bw.TotalTx.Load() == 0 || bw.TotalRx.Load() == 0 {
		t.Fatalf("the slot's QUIC bytes were not counted (tx=%d rx=%d)", bw.TotalTx.Load(), bw.TotalRx.Load())
	}

	// The stream half goes through the same proxy, never the egress seam.
	// 203.0.113.9 is unroutable here, so only the proxy can complete the dial.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialSettings.DialContext(ctx, "tcp", "203.0.113.9:443")
	if err != nil {
		t.Fatalf("the slot's stream dial did not go through its proxy: %v", err)
	}
	conn.Close()
	if egressCalls != 0 {
		t.Fatalf("the proxied slot dialled the run's test-egress seam %d times", egressCalls)
	}

	// The same function for the direct slot: the egress seam is its egress,
	// the direct counter is its counter, and it keeps the engine's own QUIC
	// socket.
	direct, directBW, directAddr := slotClientStrategySettings(nil, registry, egress)
	if directAddr != "" || direct.ConnectSettings.ProxySettings != nil {
		t.Fatal("the direct slot came out with a proxy")
	}
	if directBW != registry.Register(0) {
		t.Fatal("the direct slot is not on the registry's direct counter")
	}
	if direct.ConnectSettings.DialContextSettings == nil {
		t.Fatal("the direct slot's network space lost the bandwidth wrap")
	}
	if direct.ConnectSettings.DialContextSettings.PacketConnFactory != nil {
		t.Fatal("the direct slot's network space grew a QUIC endpoint, so its QUIC would leave from a wildcard socket instead of the engine's pinned one")
	}
	if _, err := direct.ConnectSettings.DialContext(ctx, "tcp", "203.0.113.9:443"); err == nil {
		t.Fatal("the direct slot's dial did not go through the run's test-egress seam")
	}
	if egressCalls != 1 {
		t.Fatalf("the direct slot dialled the test-egress seam %d times, want 1", egressCalls)
	}
}

// The regression this seam exists for: a proxied slot's QUIC endpoint must not
// be a host socket.
//
// connect opens one only when it finds no endpoint on the settings it dials
// with (net_extender.go, openExtenderPacketConn: a nil
// PacketConnFactory falls through to net.ListenUDP on the wildcard), and the
// sdk hands the same field to the platform carrier (device_local.go,
// newDeviceLocalPlatformTransportSettingsForProfile copies
// ProviderDialContextSettings.PacketConnFactory into
// PlatformTransportSettings.H3PacketConnFactory). One slot has two such
// settings objects, so one missing seam leaks: the network space's api traffic
// and the provider carrier's tunnel.
//
// WHAT THIS TEST IS, HONESTLY: an assertion on the branch condition, not an
// observation that no socket was opened. Both functions that read the
// condition are unexported in the pinned modules (connect, urnetwork/sdk), so
// this package cannot call them and cannot watch for the syscall; and the
// endpoint that replaces the fallback is itself a local socket (the relay's,
// provider/socks5_udp.go), so counting open sockets would not separate the two
// either. What is asserted is the field those functions read — non-nil on both
// settings objects, which makes the fallback branch unreachable — and that
// invoking it is the proxy's association rather than a wildcard socket that
// would send straight to the platform.
func TestProxiedSlotQuicEndpointIsNotAHostSocket(t *testing.T) {
	proxy := newFakeSocks5Proxy(t, fakeUDPRelayWorking, "203.0.113.7", "203.0.113.7")
	proxySettings := proxy.settings()
	stubH3UDPVerdict(t, proxySettings, proxyUDPOK)

	bw := &bandwidth.ProxyBandwidth{}
	proxied := []struct {
		name     string
		settings *connect.DialContextSettings
	}{
		{
			// run.go's network space: the api's alt-h3/alt-whodis carriers.
			name: "network space",
			settings: networkSpaceConnectSettings(
				connect.DefaultClientStrategySettings().ConnectSettings, proxySettings, bw, proxySettings.Address,
			).DialContextSettings,
		},
		{
			// run.go's provider device: the sdk copies this field into the
			// platform carrier's H3PacketConnFactory.
			name:     "provider device",
			settings: providerSlotDialSettings(nil, proxySettings, bw, proxySettings.Address),
		},
	}
	for _, seam := range proxied {
		if seam.settings == nil || seam.settings.PacketConnFactory == nil {
			t.Fatalf("%s: the proxied slot has no QUIC endpoint, so connect would open a host socket for it", seam.name)
		}
		pc, err := seam.settings.PacketConnFactory(context.Background())
		if err != nil {
			t.Fatalf("%s: the slot's QUIC endpoint: %v", seam.name, err)
		}
		h3TestRelayedDatagram(t, pc)
		pc.Close()
	}
	if n, want := proxy.associateCount(), len(proxied); n != want {
		t.Fatalf("UDP associations on the slot's proxy = %d, want %d (one per QUIC seam); an endpoint that is not the proxy leaves the host socket path open", n, want)
	}

	// The direct slot keeps both of the engine's own endpoints: a factory here
	// would replace its pinned socket with an unpinned wildcard one, and a dial
	// context would replace the engine's own dual-stack dial. The total-only
	// wrap leaves a direct setting with no proxy and no dial untouched.
	if settings := providerSlotDialSettings(nil, nil, bw, ""); settings == nil || settings.PacketConnFactory != nil {
		t.Fatal("the direct slot's provider device grew a QUIC endpoint factory")
	}
	if settings := networkSpaceConnectSettings(connect.DefaultClientStrategySettings().ConnectSettings, nil, bw, "").DialContextSettings; settings != nil {
		t.Fatal("the direct slot's network space grew a dial context, replacing the engine's own dial path")
	}
}
