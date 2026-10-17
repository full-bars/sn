package miner

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"io"
	mathrand "math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// This file ports the 3.23-fix v33 provider's dual-stage probe (proxy_probe.go)
// and the kill-switch half of the stage-1 table probe config. The stage-1
// TABLE probe itself (proxy_table_probe.go, ~1.7k lines of sampled-destination
// grading) is P1(c): in P1(a) the config resolver defaults to enabled=false,
// which is v33's documented kill-switch behavior - stage-0 survivors are
// admitted on the SOCKS5+API check alone and no fabricated grades are written.
// An operator who turns the kill switch on (via the override file or the P2
// control state) gets liveness probes with an undecidable result: survivors
// are held for the reaper and never admitted on an invented score.

// Probe result classification for the unified dual-stage probe.
type probeResult int

const (
	probeDead         probeResult = iota // TCP unreachable or not SOCKS5
	probeSocks5Only                      // speaks SOCKS5 but CONNECT to api failed
	probeAPIReachable                    // both SOCKS5 and API CONNECT succeeded
	probeTLSFailed                       // CONNECT succeeded but TLS to api did not verify (MITM/intercepting)
)

const (
	// proxyProbeTimeout bounds the SOCKS5 greeting phase (TCP + 2-byte exchange).
	proxyProbeTimeout = 3 * time.Second

	// proxyAPIAccessTimeout bounds the SOCKS5 CONNECT phase (through proxy to api).
	proxyAPIAccessTimeout = 5 * time.Second

	// proxyProbeConcurrency caps how many reachability probes run at once.
	proxyProbeConcurrency = 50

	// proxyProbeStagger is the max random jitter before each probe dial,
	// spreading the initial burst from a batch across a ~100ms window.
	proxyProbeStagger = 100 * time.Millisecond

	// proxyAPIMaxFails is the number of consecutive API probe failures before
	// an address is moved to the persistent Blacklist.
	proxyAPIMaxFails = 3

	// proxyReaperInterval is how often the background reaper scans the cache
	// for unproven or stale entries.
	proxyReaperInterval = 5 * time.Minute

	// probeStartupCooldown is how long a freshly-started process must stay
	// alive before the URL fetcher is allowed to run its first fetch +
	// stage-1 probe pass. A crash-looping box (bad binary, SELinux exec
	// trap, OOM) restarts every RestartSec seconds and would otherwise
	// re-fetch + re-probe on every restart - the probe-amplification loop.
	// Deferring the first fetch past this window means a box that cannot
	// stay up never probes at all; a healthy box waits a few seconds and
	// then fetches exactly once as normal. Must be > the systemd
	// RestartSec (5s) so a crash-loop cannot reach the fetch.
	probeStartupCooldown = 20 * time.Second

	// proxyBlacklistCooldown is how long an address stays on the Blacklist
	// before the pruner removes it, giving it a chance to re-enter via a
	// fresh fetch cycle.
	proxyBlacklistCooldown = 24 * time.Hour

	// proxyBlacklistPruneInterval is how often the blacklist pruner runs.
	proxyBlacklistPruneInterval = 30 * time.Minute
)

// socks5Greeting is the client's opening message in the SOCKS5 handshake
// (RFC 1928 section 3): version 5, offering exactly one auth method, "no
// authentication required" (0x00).
var socks5Greeting = []byte{0x05, 0x01, 0x00}

// socks5ConnectV4 builds a SOCKS5 CONNECT frame for an IPv4 destination.
// Format: VER(1) CMD(1) RSV(1) ATYP(1) DST.ADDR(4) DST.PORT(2)
func socks5ConnectV4(ip net.IP, port uint16) []byte {
	frame := make([]byte, 10)
	frame[0] = 0x05 // VER
	frame[1] = 0x01 // CMD = CONNECT
	frame[2] = 0x00 // RSV
	frame[3] = 0x01 // ATYP = IPv4
	copy(frame[4:8], ip.To4())
	binary.BigEndian.PutUint16(frame[8:10], port)
	return frame
}

// socks5Greet completes the SOCKS5 greeting phase on conn: it offers no-auth
// (0x00), plus username/password (0x02) when BOTH credentials are non-empty,
// then validates the server's method selection. Returns true only when the
// server selected a method the client actually offered and any RFC 1929
// sub-negotiation succeeded.
func socks5Greet(conn net.Conn, user, password string) bool {
	if len(user) > 255 || len(password) > 255 {
		return false
	}
	greeting := socks5Greeting
	if user != "" && password != "" {
		greeting = []byte{0x05, 0x02, 0x00, 0x02}
	}
	if _, err := conn.Write(greeting); err != nil {
		return false
	}
	greetingResp := make([]byte, 2)
	if _, err := io.ReadFull(conn, greetingResp); err != nil {
		return false
	}
	if greetingResp[0] != 0x05 {
		return false
	}
	switch greetingResp[1] {
	case 0x00:
		// No auth selected - proceed.
		return true
	case 0x02:
		if user == "" || password == "" {
			// Server demands credentials the client did not fully supply.
			return false
		}
		// RFC 1929 sub-negotiation.
		auth := []byte{0x01, byte(len(user))}
		auth = append(auth, []byte(user)...)
		auth = append(auth, byte(len(password)))
		auth = append(auth, []byte(password)...)
		if _, err := conn.Write(auth); err != nil {
			return false
		}
		authResp := make([]byte, 2)
		if _, err := io.ReadFull(conn, authResp); err != nil {
			return false
		}
		return authResp[0] == 0x01 && authResp[1] == 0x00
	default:
		// A method we never offered, or 0xFF "no acceptable method".
		return false
	}
}

// readSocks5ConnectReply consumes and validates a SOCKS5 CONNECT reply:
// VER(1)=0x05, REP(1), RSV(1)=0x00, ATYP(1), then the BND.ADDR declared by
// ATYP (4 bytes IPv4, 1 length byte + name for domain, 16 bytes IPv6) and
// BND.PORT(2). Returns true only when the ENTIRE declared reply was read and
// REP == 0x00.
func readSocks5ConnectReply(conn net.Conn) bool {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return false
	}
	if header[0] != 0x05 || header[2] != 0x00 {
		return false
	}
	var addrLen int
	switch header[3] {
	case 0x01: // IPv4
		addrLen = 4
	case 0x03: // domain name
		lenByte := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenByte); err != nil {
			return false
		}
		addrLen = int(lenByte[0])
	case 0x04: // IPv6
		addrLen = 16
	default:
		return false
	}
	addr := make([]byte, addrLen)
	if _, err := io.ReadFull(conn, addr); err != nil {
		return false
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return false
	}
	return header[1] == 0x00
}

// apiProbeAddr caches the resolved IP for the API host so each probe
// doesn't trigger a fresh DNS lookup through every proxy.
var apiProbeAddr struct {
	mu   sync.Mutex
	ip   net.IP
	port uint16
	host string
}

func resolveAPIProbeAddr(host string, port uint16) (net.IP, uint16) {
	apiProbeAddr.mu.Lock()
	defer apiProbeAddr.mu.Unlock()
	if apiProbeAddr.ip != nil && apiProbeAddr.host == host && apiProbeAddr.port == port {
		return apiProbeAddr.ip, apiProbeAddr.port
	}
	ips, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip4", host)
	if err != nil || len(ips) == 0 {
		return nil, 0
	}
	apiProbeAddr.ip = ips[0].AsSlice()
	apiProbeAddr.port = port
	apiProbeAddr.host = host
	return apiProbeAddr.ip, apiProbeAddr.port
}

// proxyProbeTLSClientConfig builds the TLS client config used to verify the
// API connection through a candidate proxy. Production uses the system root
// pool with SNI pinned to the API host. Tests override it to inject a test
// CA (see proxy_probe_tls_test.go); the seam is restored per-test.
var proxyProbeTLSClientConfig = func(serverName string) *tls.Config {
	return &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
}

// probeProxy performs a three-stage check on a single proxy address:
//  1. SOCKS5 greeting (is this actually a SOCKS5 proxy?)
//  2. SOCKS5 CONNECT to the API host:port (can the proxy reach the API?)
//  3. TLS handshake through the proxy to the API host (does the proxy
//     relay TLS transparently, or does it terminate/intercept the
//     connection? A MITM proxy answers CONNECT with 0x00 like a real one
//     but presents its own certificate at the TLS layer - verification
//     fails and the proxy is classified probeTLSFailed, so an interceptor
//     is never admitted to the pool).
//
// Both stage 1 and stage 2 reuse one TCP connection; stage 3 (TLS) reuses
// the same tunnel. A random stagger up to proxyProbeStagger is applied
// before dialing to smooth batch bursts. The API destination IP is
// resolved once and cached across probes. Credentials (user/password) are
// honoured via RFC 1929 so credentialed entries are probed on the same
// terms the real auth path will use.
func probeProxy(ctx context.Context, address, user, password string, apiHost string, apiPort uint16) probeResult {
	stagger := time.Duration(mathrand.Intn(int(proxyProbeStagger)))
	timer := time.NewTimer(stagger)
	select {
	case <-ctx.Done():
		timer.Stop()
		return probeDead
	case <-timer.C:
	}

	// Stage 1: TCP connect + SOCKS5 greeting
	dialCtx, cancel := context.WithTimeout(ctx, proxyProbeTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", address)
	if err != nil {
		return probeDead
	}
	defer conn.Close()

	if deadline, ok := dialCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			tlog("[proxy][probe] warn: could not set stage-1 deadline: %v\n", err)
		}
	}

	// Offer no-auth, plus username/password when we have BOTH creds.
	if !socks5Greet(conn, user, password) {
		return probeDead
	}

	// Stage 2: SOCKS5 CONNECT to the API host (or custom apiHost)
	apiIP, apiPort := resolveAPIProbeAddr(apiHost, apiPort)
	if apiIP == nil {
		// DNS failed - can't probe, but the proxy is SOCKS5-reachable.
		// Return socks5-only so it isn't discarded; the reaper will retry.
		return probeSocks5Only
	}

	if err := conn.SetDeadline(time.Now().Add(proxyAPIAccessTimeout)); err != nil {
		tlog("[proxy][probe] warn: could not set stage-2 deadline: %v\n", err)
		return probeSocks5Only
	}
	connectFrame := socks5ConnectV4(apiIP, apiPort)
	if _, err := conn.Write(connectFrame); err != nil {
		return probeSocks5Only
	}

	if !readSocks5ConnectReply(conn) {
		return probeSocks5Only
	}

	// Stage 3: TLS handshake through the proxy to the API host. A real
	// SOCKS5 proxy is transparent to TLS, so verification succeeds against
	// the server's real certificate. A MITM/intercepting proxy answers
	// CONNECT with 0x00 just like a real one but terminates TLS itself and
	// presents its own certificate; verification fails and the proxy is
	// classified probeTLSFailed so it is never admitted to the pool.
	if err := conn.SetDeadline(time.Now().Add(proxyAPIAccessTimeout)); err != nil {
		tlog("[proxy][probe] warn: could not set stage-3 deadline: %v\n", err)
	}
	tlsConn := tls.Client(conn, proxyProbeTLSClientConfig(apiHost))
	defer tlsConn.Close()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return probeTLSFailed
	}
	return probeAPIReachable
}

// probeProxySocks5 is a light wrapper around probeProxy that returns true
// if the proxy completes a SOCKS5 greeting. It does not test API reachability -
// that is handled by the background reaper for cached entries and by the
// dual-stage probe during the URL fetch pipeline. Used at auth time as a
// cheap gate before spending an auth-rate-limiter slot.
func probeProxySocks5(ctx context.Context, address string, timeout time.Duration) bool {
	return probeProxy(ctx, address, "", "", "", 0) != probeDead
}

// probeAndFilterProxyURLLines parses each line, probes the address with the
// dual-stage check (SOCKS5 + API CONNECT), and returns only the lines whose
// probeResult is probeAPIReachable. Lines that fail to parse are dropped.
// Lines that reach SOCKS5 but fail API CONNECT are returned separately so
// the caller can cache them with ProbeOK=false for reaper retry.
func probeAndFilterProxyURLLines(ctx context.Context, lines []string, apiHost string, apiPort uint16) (apiOK, socks5Only []string) {
	type result struct {
		idx int
		r   probeResult
	}
	results := make([]result, len(lines))
	// Pool is sized once per batch at batch start; a batch in flight keeps
	// its size even if pressure changes mid-batch.
	sem := make(chan struct{}, scaledProbeConcurrency(currentPressure()))
	var wg sync.WaitGroup

	for i, line := range lines {
		address, user, password, ok := parseProxyURLLine(line)
		if !ok {
			results[i].r = probeDead
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, address, user, password string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = result{i, probeProxy(ctx, address, user, password, apiHost, apiPort)}
		}(i, address, user, password)
	}
	wg.Wait()

	for i, r := range results {
		switch r.r {
		case probeAPIReachable:
			apiOK = append(apiOK, lines[i])
		case probeSocks5Only, probeTLSFailed:
			// probeTLSFailed (CONNECT ok, TLS verify failed = MITM/interceptor)
			// is surfaced through the same retry bucket so the reaper's
			// failure-count to blacklist lifecycle retires it - it must never
			// be admitted to the pool.
			socks5Only = append(socks5Only, lines[i])
		}
	}
	return apiOK, socks5Only
}

// ---- stage-1 table probe config: the P1(c) seam ----

// proxyTableProbeConfig is the P1(a) subset of v33's table-probe config. The
// stage-1 sampled-destination grading (SampleWidth, growth, destinations) is
// P1(c); what P1(a) needs is the kill switch (Enabled) and the admission bar
// (PassBar) so merge/fetch/reaper behave exactly as v33 does with the table
// probe disabled.
type proxyTableProbeConfig struct {
	Enabled bool    `json:"enabled"`
	PassBar float64 `json:"pass_bar,omitempty"`
	// SampleWidth and MaxSampleWidth are the base and adaptive table-probe
	// widths; carried for config compatibility and the ported width tests.
	SampleWidth    int `json:"sample_width,omitempty"`
	MaxSampleWidth int `json:"max_sample_width,omitempty"`
	// MinConfirmDials is carried for format compatibility with v33 config
	// files; the confirmation floor itself is a paid-proxy concept (P1(c)).
	MinConfirmDials int `json:"min_confirm_dials,omitempty"`
}

// defaultProxyTableProbeConfig is v33's default: stage-1 grading enabled with
// a 0.6 pass bar and a 12-wide sample that may grow to 36. P1(a) deliberately
// reports Enabled=false (see resolveProxyTableProbeConfig) until the table
// probe lands, because admitting on stage 0 alone is v33's documented
// kill-switch behavior while admitting on a fabricated score is not.
func defaultProxyTableProbeConfig() proxyTableProbeConfig {
	return proxyTableProbeConfig{
		Enabled:        true,
		PassBar:        0.6,
		SampleWidth:    12,
		MaxSampleWidth: 36,
	}
}

// probeWidth is the wider of the base sample width and the adaptive ceiling
// (v33), kept for config compatibility; P1(a) does not run the sampler.
func (self proxyTableProbeConfig) probeWidth() int {
	if self.MaxSampleWidth > self.SampleWidth {
		return self.MaxSampleWidth
	}
	return self.SampleWidth
}

// resolveProxyTableProbeConfig returns the config the fetch/admission path
// uses. The P2 control-state override wins, then the
// <state>/proxy_table_probe.json override file, then the P1(a) default.
//
// P1(a) default: the table probe is NOT ported, so Enabled=false. This is the
// same behavior v33 shows with the kill switch off: stage-0 survivors are
// admitted and no grades are written. Operators who explicitly enable grading
// in the override file get liveness-only probes with an undecidable result
// (never admitted on a fabricated score), and one log line explaining it.
func resolveProxyTableProbeConfig() proxyTableProbeConfig {
	cfg := defaultProxyTableProbeConfig()
	cfg.Enabled = false
	applyProxyTableProbeConfigOverrides(&cfg)
	return cfg
}

// proxyTableProbeConfigOverridePath returns <state>/proxy_table_probe.json,
// the operator override for the stage-1 kill switch and bar.
func proxyTableProbeConfigOverridePath() (string, error) {
	return providerStatePath("proxy_table_probe.json")
}

// applyProxyTableProbeConfigOverrides layers the operator override file over
// cfg. A missing, empty, or unparseable file leaves cfg untouched; read errors
// are reported once per value so a bad override cannot spam the cycle log.
func applyProxyTableProbeConfigOverrides(cfg *proxyTableProbeConfig) {
	path, err := proxyTableProbeConfigOverridePath()
	if err != nil {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return
	}
	var override proxyTableProbeConfig
	if err := json.Unmarshal(b, &override); err != nil {
		proxyTableProbeConfigWarnOnce(string(b), err)
		return
	}
	cfg.Enabled = override.Enabled
	if override.PassBar > 0 {
		cfg.PassBar = override.PassBar
	}
	if override.SampleWidth > 0 {
		cfg.SampleWidth = override.SampleWidth
	}
	if override.MaxSampleWidth > 0 {
		cfg.MaxSampleWidth = override.MaxSampleWidth
	}
	if override.MinConfirmDials > 0 {
		cfg.MinConfirmDials = override.MinConfirmDials
	}
}

var proxyTableProbeConfigWarn = struct {
	sync.Mutex
	seen string
}{}

// proxyTableProbeConfigWarnOnce reports an unparseable override once per
// exact content, so a reload loop cannot repeat the same warning forever.
func proxyTableProbeConfigWarnOnce(content string, err error) {
	proxyTableProbeConfigWarn.Lock()
	defer proxyTableProbeConfigWarn.Unlock()
	if proxyTableProbeConfigWarn.seen == content {
		return
	}
	proxyTableProbeConfigWarn.seen = content
	tlog("[proxy][url] warning: proxy_table_probe.json is not valid config (%v); using the built-in stage-1 defaults\n", err)
}

// describeProxyTableProbeConfig renders the config for the per-cycle log.
func describeProxyTableProbeConfig(cfg proxyTableProbeConfig) string {
	if !cfg.Enabled {
		return "disabled (stage-1 table grading is not ported yet; stage-0 SOCKS5+API admission only)"
	}
	return "enabled (liveness-only: the stage-1 table probe is not ported yet, no score is fabricated)"
}

// tableProbeResult is the stage-1 verdict. P1(a) never produces a decidable
// one; the full type and the sampled probing land in P1(c).
type tableProbeResult struct {
	Score       float64
	OK          int
	SampleWidth int
	Failed      []string
	Decidable   bool
}

// proxyURLGrade is the admission decision for one URL-source line after the
// staged probe.
type proxyURLGrade struct {
	// Qualified is true when the proxy cleared the admission bar and may
	// enter the auth queue.
	Qualified bool
	// Socks5Only is true when stage 0 succeeded but the proxy failed the
	// API CONNECT - cached with ProbeOK=false for the reaper, never
	// admitted.
	Socks5Only bool
	// Decidable is true only when a real stage-1 verdict exists.
	Decidable bool
	// Score and Failed record the stage-1 table probe for grading.
	Score  float64
	Failed []string
}

// probeAndGradeProxyURLLines runs the staged probe over the lines: stage 0
// (SOCKS5 + API CONNECT, via probeAndFilterProxyURLLines), then - when the
// stage-1 kill switch is on - the table probe on survivors.
//
// P1(a) ports the kill-switch path exactly and stubs the enabled path:
// liveness is probed (so dead entries never enter the cache or consume an
// auth slot), but no stage-1 verdict exists, so every survivor is cached as
// below-bar (ProbeOK=false) for the reaper to re-check and never admitted on
// an invented score. That keeps "socks5-only proxies are held, not admitted"
// and "no fabricated grades" true while the sampler is P1(c).
func probeAndGradeProxyURLLines(ctx context.Context, lines []string, apiHost string, apiPort uint16, cfg proxyTableProbeConfig) map[string]proxyURLGrade {
	apiOK, socks5Only := probeAndFilterProxyURLLines(ctx, lines, apiHost, apiPort)

	// Kill switch (Enabled=false): stage-1 grading is OFF. Every stage-0
	// survivor is treated as qualified (pre-feature behavior - URL proxies
	// admitted on the SOCKS5+API check alone) and no grades are recorded,
	// so the auth-time gate has nothing to enforce.
	if !cfg.Enabled {
		grades := make(map[string]proxyURLGrade, len(apiOK)+len(socks5Only))
		for _, line := range apiOK {
			address, _, _, ok := parseProxyURLLine(line)
			if !ok {
				continue
			}
			grades[address] = proxyURLGrade{Qualified: true}
		}
		for _, line := range socks5Only {
			address, _, _, ok := parseProxyURLLine(line)
			if !ok {
				continue
			}
			grades[address] = proxyURLGrade{Socks5Only: true}
		}
		return grades
	}

	// Stage-1 requested but not ported (P1(c)). Survivors keep their stage-0
	// result: cached ProbeOK=false (held for the reaper), never admitted,
	// Decidable stays false so nothing is persisted as a grade.
	grades := make(map[string]proxyURLGrade, len(apiOK)+len(socks5Only))
	for _, line := range apiOK {
		address, _, _, ok := parseProxyURLLine(line)
		if !ok {
			continue
		}
		grades[address] = proxyURLGrade{}
	}
	for _, line := range socks5Only {
		address, _, _, ok := parseProxyURLLine(line)
		if !ok {
			continue
		}
		grades[address] = proxyURLGrade{Socks5Only: true}
	}
	return grades
}

// probeTableThroughProxy is the P1(c) stage-1 sampler. Until it lands, a call
// returns an undecidable result: the caller must not persist a score.
func probeTableThroughProxy(ctx context.Context, address, user, password, region string, port uint16, cfg proxyTableProbeConfig) tableProbeResult {
	return tableProbeResult{}
}

// tableProbePassCounter advances once per fetch cycle in v33 to rotate the
// stage-1 destination block; kept so the P1(c) port has the call site.
var tableProbePassCounter atomic.Int64
