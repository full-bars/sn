package miner

// Per-identity resource guard (docs/design/per-identity-resource-guard.md).
//
// Builds N proxied provider slots through the device half of provideWithProxy
// (run.go), each behind its own in-process SOCKS5 proxy, against one
// in-process TLS server standing in for the api and the platform. Nothing
// leaves the process: the proxies map every name under the test domain to the
// server and refuse everything else, and the slots resolve no name locally.
//
// It then measures goroutines and heap per identity and asserts generous
// upper bounds, so a change that multiplies the per-identity platform
// transports or connection strategies fails here instead of on a host with
// hundreds of proxies.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/urfoundation/sn/miner/bandwidth"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// A dotted domain, so the sdk derives the family-pinned platform urls
// (connect-v4.<domain>, connect-v6.<domain>) exactly as it does for the
// production host. An ip-literal platform url derives none, and the transport
// group would degrade to a single transport, hiding the shape this guards.
const resourceGuardDomain = "resource-guard.test.invalid"

const resourceGuardIdentities = 50

// resourceGuardFakeServer is the api and the platform in one TLS server. It
// counts platform websocket upgrades by the client id of the bearer token and
// the host the transport dialed.
type resourceGuardFakeServer struct {
	server *httptest.Server
	roots  *x509.CertPool

	mu       sync.Mutex
	upgrades map[string]map[string]int // client id -> host -> upgrades
	open     map[string]map[string]int // client id -> host -> open now
	api      map[string]int            // path -> requests
	total    int
	wsConns  map[*websocket.Conn]struct{}
}

func newResourceGuardFakeServer(t *testing.T) *resourceGuardFakeServer {
	t.Helper()
	certificate, roots := resourceGuardCertificate(t)
	self := &resourceGuardFakeServer{
		roots:    roots,
		upgrades: map[string]map[string]int{},
		open:     map[string]map[string]int{},
		api:      map[string]int{},
		wsConns:  map[*websocket.Conn]struct{}{},
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	self.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			self.mu.Lock()
			self.api[r.URL.Path]++
			self.mu.Unlock()
			if r.URL.Path == "/hello" {
				w.WriteHeader(http.StatusOK)
				return
			}
			http.NotFound(w, r)
			return
		}
		clientId := resourceGuardBearerClientId(r.Header.Get("Authorization"))
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		self.mu.Lock()
		self.total++
		resourceGuardCount(self.upgrades, clientId, host, 1)
		resourceGuardCount(self.open, clientId, host, 1)
		self.wsConns[conn] = struct{}{}
		self.mu.Unlock()
		defer func() {
			conn.Close()
			self.mu.Lock()
			resourceGuardCount(self.open, clientId, host, -1)
			delete(self.wsConns, conn)
			self.mu.Unlock()
		}()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	self.server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	// the strategy races its dialers and drops the losers mid-handshake
	self.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	self.server.StartTLS()
	t.Cleanup(func() {
		self.mu.Lock()
		for conn := range self.wsConns {
			conn.Close()
		}
		self.mu.Unlock()
		self.server.CloseClientConnections()
		self.server.Close()
	})
	return self
}

func resourceGuardCount(counts map[string]map[string]int, clientId string, host string, delta int) {
	byHost := counts[clientId]
	if byHost == nil {
		byHost = map[string]int{}
		counts[clientId] = byHost
	}
	byHost[host] += delta
}

// The fixture tokens are unsigned (providerRegistrationTestToken); the server
// reads the client id only to attribute a connection to its identity.
func resourceGuardBearerClientId(authorization string) string {
	token, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok {
		return "(none)"
	}
	claims := gojwt.MapClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(token, claims); err != nil {
		return "(invalid)"
	}
	clientId, _ := claims["client_id"].(string)
	return clientId
}

type resourceGuardServerSnapshot struct {
	total    int
	open     int
	upgrades map[string]map[string]int
	openBy   map[string]map[string]int
	api      map[string]int
}

func (self *resourceGuardFakeServer) snapshot() resourceGuardServerSnapshot {
	self.mu.Lock()
	defer self.mu.Unlock()
	clone := func(in map[string]map[string]int) map[string]map[string]int {
		out := map[string]map[string]int{}
		for k, v := range in {
			byHost := map[string]int{}
			for h, n := range v {
				byHost[h] = n
			}
			out[k] = byHost
		}
		return out
	}
	open := 0
	for _, byHost := range self.open {
		for _, n := range byHost {
			open += n
		}
	}
	api := map[string]int{}
	for k, v := range self.api {
		api[k] = v
	}
	return resourceGuardServerSnapshot{total: self.total, open: open, upgrades: clone(self.upgrades), openBy: clone(self.open), api: api}
}

// A self-signed certificate for the test domain and every name one label
// below it (api., connect., connect-v4., connect-v6.).
func resourceGuardCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: resourceGuardDomain},
		DNSNames:              []string{resourceGuardDomain, "*." + resourceGuardDomain},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, roots
}

// resourceGuardFakeSocks is one identity's SOCKS5 proxy: no auth, CONNECT
// only, names under the test domain only. UDP ASSOCIATE is refused, so the
// slot's QUIC carriers fail closed at the proxy UDP check (h3_packet_conn.go).
type resourceGuardFakeSocks struct {
	listener net.Listener
	target   string

	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	refused   map[string]int
	requested map[string]int
	wg        sync.WaitGroup
}

func newResourceGuardFakeSocks(t *testing.T, target string) *resourceGuardFakeSocks {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	self := &resourceGuardFakeSocks{listener: listener, target: target, conns: map[net.Conn]struct{}{}, refused: map[string]int{}, requested: map[string]int{}}
	self.wg.Add(1)
	go func() {
		defer self.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if !self.track(conn) {
				conn.Close()
				return
			}
			self.wg.Add(1)
			go func() {
				defer self.wg.Done()
				defer self.untrack(conn)
				self.resourceGuardFakeServe(conn)
			}()
		}
	}()
	t.Cleanup(self.close)
	return self
}

func (self *resourceGuardFakeSocks) track(conn net.Conn) bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.conns == nil {
		return false
	}
	self.conns[conn] = struct{}{}
	return true
}

func (self *resourceGuardFakeSocks) untrack(conn net.Conn) {
	conn.Close()
	self.mu.Lock()
	defer self.mu.Unlock()
	delete(self.conns, conn)
}

func (self *resourceGuardFakeSocks) close() {
	self.listener.Close()
	self.mu.Lock()
	for conn := range self.conns {
		conn.Close()
	}
	self.conns = nil
	self.mu.Unlock()
	self.wg.Wait()
}

func (self *resourceGuardFakeSocks) refuse(what string) {
	self.mu.Lock()
	self.refused[what]++
	self.mu.Unlock()
}

func (self *resourceGuardFakeSocks) resourceGuardFakeServe(conn net.Conn) {
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, header[1])
	if _, err := io.ReadFull(reader, methods); err != nil {
		return
	}
	if !bytes.Contains(methods, []byte{0}) {
		conn.Write([]byte{5, 0xff})
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(reader, request); err != nil || request[0] != 5 {
		return
	}
	var host string
	switch request[3] {
	case 1:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(reader, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case 4:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(reader, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case 3:
		size, err := reader.ReadByte()
		if err != nil {
			return
		}
		name := make([]byte, size)
		if _, err := io.ReadFull(reader, name); err != nil {
			return
		}
		host = string(name)
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(reader, portBytes); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBytes)
	self.mu.Lock()
	self.requested[fmt.Sprintf("cmd %d %s", request[1], host)]++
	self.mu.Unlock()
	reply := func(code byte) {
		conn.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	}
	if request[1] != 1 {
		self.refuse(fmt.Sprintf("cmd %d", request[1]))
		reply(7) // command not supported
		return
	}
	if host != resourceGuardDomain && !strings.HasSuffix(host, "."+resourceGuardDomain) {
		self.refuse(fmt.Sprintf("%s:%d", host, port))
		reply(2) // not allowed by ruleset
		return
	}
	upstream, err := net.DialTimeout("tcp", self.target, 5*time.Second)
	if err != nil {
		reply(5)
		return
	}
	defer upstream.Close()
	reply(0)
	conn.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(upstream, reader)
		upstream.(*net.TCPConn).CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		io.Copy(conn, upstream)
		if tcp, ok := conn.(*net.TCPConn); ok {
			tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	conn.Close()
	upstream.Close()
	<-done
}

func (self *resourceGuardFakeSocks) requests() map[string]int {
	self.mu.Lock()
	defer self.mu.Unlock()
	out := map[string]int{}
	for k, v := range self.requested {
		out[k] = v
	}
	return out
}

func (self *resourceGuardFakeSocks) refusals() map[string]int {
	self.mu.Lock()
	defer self.mu.Unlock()
	out := map[string]int{}
	for k, v := range self.refused {
		out[k] = v
	}
	return out
}

// resourceGuardOfflineResolver answers no name: every dial of a proxied slot
// should name its host to the proxy, so a local lookup is a leak around it.
var resourceGuardOfflineResolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network string, address string) (net.Conn, error) {
		return nil, errors.New("the resource guard resolves no name locally")
	},
}

// resourceGuardSlot is one proxied identity's device half of provideWithProxy.
type resourceGuardSlot struct {
	device  *sdk.DeviceLocal
	cleanup func() error
}

// startResourceGuardSlot follows provideWithProxy (run.go:1430-1803) from the
// strategy settings to the provide mode, in the same order, with the same
// helpers. The run-level steps it leaves out are listed in the design doc:
// client JWT acquisition (an unsigned fixture token stands in), the admission
// gate, the wallet, progress and hot swap. Test-only settings: TLS roots, the
// offline resolver, the noop logger and the loopback DNS pump host.
func startResourceGuardSlot(t *testing.T, ctx context.Context, index uint64, proxySettings *connect.ProxySettings, bwRegistry *bandwidth.ProxyRegistry, roots *x509.CertPool, apiUrl string, connectUrl string) *resourceGuardSlot {
	t.Helper()
	proxyCtx, proxyCancel := context.WithCancel(ctx)
	closers := []func() error{}
	defer func() {
		if proxyCancel != nil {
			// construction failed part way
			for i := len(closers) - 1; i >= 0; i-- {
				closers[i]()
			}
			proxyCancel()
		}
	}()

	clientStrategySettings, bw, proxyAddr := slotClientStrategySettings(proxySettings, bwRegistry, nil)
	clientStrategySettings.TlsConfig = &tls.Config{RootCAs: roots}
	clientStrategySettings.Resolver = resourceGuardOfflineResolver
	clientStrategySettings.Log = withProxyLegObserver(connect.NewNoopLogger(), proxySettings.Key())

	clientStrategy := connect.NewClientStrategy(proxyCtx, clientStrategySettings)
	closers = append(closers, func() error { clientStrategy.Close(); return nil })

	networkSpace := sdk.NewNetworkSpaceWithUrls(proxyCtx, apiUrl, connectUrl, clientStrategySettings)
	api := networkSpace.GetApi()
	closers = append(closers, func() error { return api.CloseAndWait(context.Background()) })
	closers = append(closers, func() error { networkSpace.Close(); return nil })

	clientId := connect.NewId()
	byClientJwt := providerRegistrationTestToken(t, clientId.String(), "resource-guard")

	stopSmartDialer := startSmartDialerIdentity(clientStrategy, bw, apiUrl, int(index), proxySettings.Address, true)
	closers = append(closers, func() error { stopSmartDialer(); return nil })

	seed := make([]byte, 32)
	rand.Read(seed)
	settings := ProviderDeviceSettings([32]byte{})
	if settings.ClientSettings.ContractManagerSettings != nil {
		settings.ClientSettings.ContractManagerSettings.ProxyDescription = contractProxyDescription(index, proxyAddr)
	}
	settings.Log = withProxyLegObserver(connect.NewNoopLogger(), proxySettings.Key())
	settings.ProvideExtenderEnabled = false
	settings.KeyMaterial = sdk.NewDeviceLocalKeyMaterial(seed, nil, nil)
	settings.ProviderDialContextSettings = providerSlotDialSettings(nil, proxySettings, bw, proxyAddr)
	// The sdk default pump is a production host; the loopback provider
	// fixtures name their own (h3_provider_policy_test.go).
	settings.DnsPumpHost = "127.0.0.1"

	device, err := newProviderDeviceLocal(
		proxyCtx,
		networkSpace,
		clientStrategySettings,
		byClientJwt,
		"resource guard provider",
		settings,
		nil,
		proxySettings.Key(),
		clientId,
		h3SlotTransportSettings(proxySettings),
	)
	if err != nil {
		t.Fatalf("slot %d: %v", index, err)
	}
	closers = append(closers, func() error { return device.CloseAndWait(context.Background()) })
	stopHealth := watchProxyTransportHealth(proxyCtx, proxySettings.Key(), device, proxyTransportWatchInterval)
	closers = append(closers, func() error { stopHealth(); return nil })
	stopBilling := watchProxyBilling(proxyCtx, bw, device, providerBillingSampleInterval)
	closers = append(closers, func() error { stopBilling(); return nil })
	retireContracts := registerContractOutcomes(device)
	closers = append(closers, func() error { retireContracts(); return nil })

	device.SetProvideControlMode(sdk.ProvideControlModeAlways)

	cancel := proxyCancel
	proxyCancel = nil
	return &resourceGuardSlot{
		device: device,
		cleanup: func() error {
			cancel()
			var errs []error
			for i := len(closers) - 1; i >= 0; i-- {
				errs = append(errs, closers[i]())
			}
			return errors.Join(errs...)
		},
	}
}

// resourceGuardGoroutineGroups parses the debug=1 goroutine profile and
// counts goroutines by their entry function (the frame the goroutine was
// started at). Goroutines of the fakes themselves (the TLS server's
// connections and the SOCKS proxies) are reported under "(fake)" so they can
// be separated from the slots'.
func resourceGuardGoroutineGroups(t *testing.T) (map[string]int, int) {
	t.Helper()
	var buffer bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buffer, 1); err != nil {
		t.Fatal(err)
	}
	groups := map[string]int{}
	total := 0
	records := strings.Split(buffer.String(), "\n\n")
	for _, record := range records {
		lines := strings.Split(strings.TrimSpace(record), "\n")
		if len(lines) == 0 {
			continue
		}
		var count int
		if _, err := fmt.Sscanf(lines[0], "%d @", &count); err != nil {
			continue
		}
		var frames []string
		for _, line := range lines[1:] {
			fields := strings.Fields(line)
			// "#  0x4f2a1c  pkg.func+0x5c  /path/file.go:123"
			if len(fields) >= 3 && fields[0] == "#" && strings.HasPrefix(fields[1], "0x") {
				name := fields[2]
				if plus := strings.LastIndex(name, "+0x"); plus > 0 {
					name = name[:plus]
				}
				frames = append(frames, name)
			}
		}
		if len(frames) == 0 {
			continue
		}
		total += count
		// The entry is the bottom frame. connect starts most of its
		// goroutines as `go HandleError(func() {...})`, so a HandleError
		// entry names the function it runs instead (the frame above it).
		entryIndex := len(frames) - 1
		for entryIndex > 0 && strings.HasPrefix(frames[entryIndex], "github.com/urnetwork/connect.HandleError") {
			entryIndex--
		}
		entry := frames[entryIndex]
		fake := false
		for _, frame := range frames {
			if strings.Contains(strings.ToLower(frame), "resourceguardfake") || strings.HasPrefix(frame, "net/http.(*conn).serve") || strings.HasPrefix(frame, "net/http.(*Server).Serve") || strings.Contains(frame, "testing.(*T)") || strings.Contains(frame, "testing.tRunner") {
				fake = true
				break
			}
		}
		if fake {
			entry = "(fake or test) " + entry
		}
		groups[entry] += count
	}
	return groups, total
}

func resourceGuardHeapAlloc() uint64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

func TestPerIdentityResourceGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("builds 50 provider devices; skipped under -short")
	}
	server := newResourceGuardFakeServer(t)
	serverAddr := server.server.Listener.Addr().String()
	_, serverPort, _ := net.SplitHostPort(serverAddr)
	apiUrl := fmt.Sprintf("https://api.%s:%s", resourceGuardDomain, serverPort)
	connectUrl := fmt.Sprintf("wss://connect.%s:%s", resourceGuardDomain, serverPort)

	proxies := make([]*resourceGuardFakeSocks, resourceGuardIdentities)
	for i := range proxies {
		proxies[i] = newResourceGuardFakeSocks(t, serverAddr)
	}

	baselineGroups, baselineGoroutines := resourceGuardGoroutineGroups(t)
	baselineHeap := resourceGuardHeapAlloc()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	bwRegistry := bandwidth.NewRegistry()
	slots := make([]*resourceGuardSlot, 0, resourceGuardIdentities)
	cleaned := false
	cleanupSlots := func() error {
		if cleaned {
			return nil
		}
		cleaned = true
		var wg sync.WaitGroup
		errs := make([]error, len(slots))
		for i, slot := range slots {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = slot.cleanup()
			}()
		}
		wg.Wait()
		return errors.Join(errs...)
	}
	t.Cleanup(func() {
		if err := cleanupSlots(); err != nil {
			t.Errorf("slot cleanup: %v", err)
		}
	})
	startedAt := time.Now()
	for i := range resourceGuardIdentities {
		proxySettings := &connect.ProxySettings{Network: "tcp", Address: proxies[i].listener.Addr().String()}
		slots = append(slots, startResourceGuardSlot(t, ctx, uint64(i+1), proxySettings, bwRegistry, server.roots, apiUrl, connectUrl))
	}

	// Settle: every identity's provider transport is connected, and the
	// platform's connection counts and the goroutine count hold still for
	// resourceGuardStablePolls consecutive polls.
	const resourceGuardPoll = 200 * time.Millisecond
	const resourceGuardStablePolls = 10
	deadline := time.Now().Add(120 * time.Second)
	stable := 0
	var last resourceGuardServerSnapshot
	lastGoroutines := -1
	for {
		connected := 0
		for _, slot := range slots {
			if slot.device.GetProviderConnected() {
				connected++
			}
		}
		snapshot := server.snapshot()
		goroutines := runtime.NumGoroutine()
		if connected == len(slots) && snapshot.total == last.total && snapshot.open == last.open && goroutines == lastGoroutines {
			stable++
		} else {
			stable = 0
		}
		last, lastGoroutines = snapshot, goroutines
		if stable >= resourceGuardStablePolls {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slots did not settle: connected %d/%d, platform upgrades %d open %d, goroutines %d", connected, len(slots), snapshot.total, snapshot.open, goroutines)
		}
		timer := time.NewTimer(resourceGuardPoll)
		<-timer.C
	}
	settleTime := time.Since(startedAt)

	groups, goroutines := resourceGuardGoroutineGroups(t)
	heap := resourceGuardHeapAlloc()
	snapshot := server.snapshot()
	n := float64(resourceGuardIdentities)

	slotGoroutines := 0
	type group struct {
		name  string
		delta int
	}
	var deltas []group
	for name, count := range groups {
		delta := count - baselineGroups[name]
		if delta == 0 {
			continue
		}
		deltas = append(deltas, group{name, delta})
		if !strings.HasPrefix(name, "(fake or test)") {
			slotGoroutines += delta
		}
	}
	sort.Slice(deltas, func(i, j int) bool {
		if deltas[i].delta != deltas[j].delta {
			return deltas[i].delta > deltas[j].delta
		}
		return deltas[i].name < deltas[j].name
	})
	transportGoroutines := 0
	for _, g := range deltas {
		if strings.Contains(g.name, "PlatformTransport") {
			transportGoroutines += g.delta
		}
	}

	upgradesByHost := map[string]int{}
	openByHost := map[string]int{}
	for _, byHost := range snapshot.upgrades {
		for host, count := range byHost {
			upgradesByHost[host] += count
		}
	}
	for _, byHost := range snapshot.openBy {
		for host, count := range byHost {
			openByHost[host] += count
		}
	}
	refused := map[string]int{}
	for _, proxy := range proxies {
		for what, count := range proxy.refusals() {
			key := what
			if host, port, err := net.SplitHostPort(what); err == nil {
				key = host + ":" + port
			}
			refused[key] += count
		}
	}

	heapDelta := int64(heap) - int64(baselineHeap)
	t.Logf("identities %d, settled in %s", resourceGuardIdentities, settleTime.Round(time.Millisecond))
	t.Logf("goroutines: baseline %d, settled %d, delta %d (%.1f per identity); slot goroutines (fakes excluded) %d (%.1f per identity)",
		baselineGoroutines, goroutines, goroutines-baselineGoroutines, float64(goroutines-baselineGoroutines)/n, slotGoroutines, float64(slotGoroutines)/n)
	t.Logf("heap after GC: baseline %d, settled %d, delta %d (%.0f KiB per identity)", baselineHeap, heap, heapDelta, float64(heapDelta)/n/1024)
	t.Logf("platform transport goroutines: %d (%.1f per identity)", transportGoroutines, float64(transportGoroutines)/n)
	for _, host := range resourceGuardSortedKeys(upgradesByHost) {
		t.Logf("platform upgrades %s: %d (%.2f per identity), open %d (%.2f per identity)", host, upgradesByHost[host], float64(upgradesByHost[host])/n, openByHost[host], float64(openByHost[host])/n)
	}
	for _, path := range resourceGuardSortedKeys(snapshot.api) {
		t.Logf("api %s: %d", path, snapshot.api[path])
	}
	requested := map[string]int{}
	for _, proxy := range proxies {
		for what, count := range proxy.requests() {
			requested[what] += count
		}
	}
	for _, what := range resourceGuardSortedKeys(requested) {
		t.Logf("proxy request %s: %d", what, requested[what])
	}
	for _, what := range resourceGuardSortedKeys(refused) {
		t.Logf("proxy refused %s: %d", what, refused[what])
	}
	t.Logf("goroutine delta by entry function:")
	for _, g := range deltas {
		t.Logf("  %+5d (%5.2f/identity) %s", g.delta, float64(g.delta)/n, g.name)
	}

	perIdentity := func(entry string) float64 {
		return float64(groups[entry]-baselineGroups[entry]) / n
	}
	maxOpenPerIdentity := 0
	for _, byHost := range snapshot.openBy {
		open := 0
		for _, count := range byHost {
			open += count
		}
		maxOpenPerIdentity = max(maxOpenPerIdentity, open)
	}
	strategies := perIdentity("github.com/urnetwork/connect.NewClientStrategy.func1")
	transports := perIdentity("github.com/urnetwork/connect.NewPlatformTransportWithTargetMode.func1")
	pinnedTransports := perIdentity("github.com/urnetwork/connect.(*PlatformTransport).runFamilyHoldWatcher")
	// Whether the pinned transports dial depends on this host's interfaces
	// (connect pinnedFamilySupported, transport_family.go:179); that they are
	// built does not. The structural counts below are host-independent.
	t.Logf("host family support: v4 %t, v6 %t", connect.FamilySupported(4), connect.FamilySupported(6))
	t.Logf("per identity: strategies %.2f, platform transports %.2f (pinned %.2f), max open platform connections %d",
		strategies, transports, pinnedTransports, maxOpenPerIdentity)

	// Upper bounds. Measured on base 20353851 (sdk 5dfc8a7c2a50, connect
	// 49d75e264480), N=50, three runs with -count=1, on a host whose only
	// interface address is in 192.0.2.0/24 (so neither pinned transport
	// dials): see docs/design/per-identity-resource-guard.md for the table.
	//
	// The structural counts are exact on that base: one strategy for the run
	// (run.go:1492), one for the network space (sdk network_space.go:1331),
	// and three NewDirectClientStrategy (v4, v6, public standby) in the
	// provider's transport group; three platform transports (v4, v6,
	// standby). A proxied slot built without the family transports (task E's
	// DisablePlatformFamilyTransports) is expected to measure 3 strategies,
	// 1 transport and 0 pinned; tighten these bounds when that lands.
	const (
		maxStrategiesPerIdentity     = 5.0     // measured 5.00 x3
		maxTransportsPerIdentity     = 3.0     // measured 3.00 x3
		maxPinnedPerIdentity         = 2.0     // measured 2.00 x3
		maxOpenPlatformConnsPerSlot  = 3       // measured 1 x3 (one per transport at most)
		maxSlotGoroutinesPerIdentity = 80.0    // measured 49.5, 48.8, 48.6
		maxHeapBytesPerIdentity      = 2 << 20 // measured 820, 799, 799 KiB
	)
	if strategies > maxStrategiesPerIdentity {
		t.Errorf("connection strategies per identity %.2f > %.0f", strategies, maxStrategiesPerIdentity)
	}
	if transports > maxTransportsPerIdentity {
		t.Errorf("platform transports per identity %.2f > %.0f", transports, maxTransportsPerIdentity)
	}
	if pinnedTransports > maxPinnedPerIdentity {
		t.Errorf("family-pinned platform transports per identity %.2f > %.0f", pinnedTransports, maxPinnedPerIdentity)
	}
	if maxOpenPerIdentity > maxOpenPlatformConnsPerSlot {
		t.Errorf("open platform connections of one identity %d > %d", maxOpenPerIdentity, maxOpenPlatformConnsPerSlot)
	}
	if got := float64(slotGoroutines) / n; got > maxSlotGoroutinesPerIdentity {
		t.Errorf("slot goroutines per identity %.1f > %.0f", got, maxSlotGoroutinesPerIdentity)
	}
	if got := float64(heapDelta) / n; got > maxHeapBytesPerIdentity {
		t.Errorf("heap per identity %.0f bytes > %d", got, maxHeapBytesPerIdentity)
	}

	// Teardown returns the slots' goroutines: poll until the non-fake
	// goroutines are back within a small allowance of the baseline (process
	// singletons such as a scheduler may start once and stay).
	if err := cleanupSlots(); err != nil {
		t.Fatalf("slot cleanup: %v", err)
	}
	const resourceGuardTeardownAllowance = 10
	teardownDeadline := time.Now().Add(30 * time.Second)
	for {
		groups, _ := resourceGuardGoroutineGroups(t)
		remaining := 0
		for name, count := range groups {
			if !strings.HasPrefix(name, "(fake or test)") {
				remaining += count - baselineGroups[name]
			}
		}
		if remaining <= resourceGuardTeardownAllowance {
			t.Logf("after teardown: %d slot goroutines above baseline", remaining)
			break
		}
		if time.Now().After(teardownDeadline) {
			for name, count := range groups {
				if delta := count - baselineGroups[name]; delta > 0 && !strings.HasPrefix(name, "(fake or test)") {
					t.Logf("  left: %+d %s", delta, name)
				}
			}
			t.Fatalf("after teardown: %d slot goroutines above baseline", remaining)
		}
		timer := time.NewTimer(resourceGuardPoll)
		<-timer.C
	}
}

func resourceGuardSortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
