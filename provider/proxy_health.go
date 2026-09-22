package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
)

// proxyBandwidthSnapshot returns a detached copy of bw that carries the same
// latency, byte, and client counters, plus enough internal timing state for
// MaxAge() to return the correct value. It avoids the fake-AddSession pattern
// that silently zeroed LatencyNs/SocksLatencyNs on every copy.
func proxyBandwidthSnapshot(bw *bandwidth.ProxyBandwidth) *bandwidth.ProxyBandwidth {
	if bw == nil {
		return nil
	}
	return bw.Snapshot()
}

// ProxyFailureCounters tracks per-proxy failure counts by category, so
// operators can distinguish recurring auth errors from transient timeouts.
type ProxyFailureCounters struct {
	AuthFailures    atomic.Int64
	TimeoutFailures atomic.Int64
	TransportDrops  atomic.Int64
}

// proxyHealth tracks one proxy's platform-transport liveness for the
// [health][proxies] report. See docs/design/dead-proxy-health-report.md.
type proxyHealth struct {
	address         string
	currentlyUp     bool
	everUp          bool
	connecting      bool      // registered and still trying to establish first WebSocket
	connectingSince time.Time // when connecting was last set true (bounds stale connecting)
	downSince       time.Time // when currentlyUp last went false (for recovery latency)
	lastSeenUp      bool      // currentlyUp as of the previous heartbeat (baseline)
	deadLogged      bool      // a confirmed-dead event has been emitted for this proxy
	bw              *bandwidth.ProxyBandwidth
	failures        ProxyFailureCounters
	lastError       string
	lastErrorAt     time.Time // when lastError was recorded (so a stale error reads as such)
	regen           uint64    // incremented on each RegisterProxy; UnregisterProxy skips if stale
}

// StagingWindowDuration is the shared duration used by both connectingStaleAfter
// (proxy_health.go) and deadConfirmDelay (health_heartbeat.go, cmd_remove_dead.go).
// Both must agree: connectingStaleAfter bounds the connecting state from
// RegisterProxy, while deadConfirmDelay gates the uptime check from provider start.
// The ~1h staging window promised in docs/Proxy-Management.md depends on these
// two values matching. If one changes, the other must too — enforced by this
// shared constant (M7 fix).
const StagingWindowDuration = 65 * time.Minute

// connectingStaleAfter bounds the connecting state. connecting is cleared only
// by markProxyUp/markProxyDown, neither of which fires on a failed dial, so a
// proxy that respawns and can never reconnect would otherwise read "connecting"
// forever. After this duration the state falls back to a degraded tier (computed
// from the stale downSince), making a hung respawn distinguishable from a fresh
// one and actionable during an outage.
//
// This equals StagingWindowDuration: deadConfirmDelay (provider start) gates
// only the NewlyDead log row; connectingStaleAfter (per-proxy registration)
// gates state classification. A never-connected proxy reads as "dead" 65m
// after its own registration and its NewlyDead row appears only once both
// conditions hold.
const connectingStaleAfter = StagingWindowDuration

// connectingActive reports whether the proxy is still in a fresh connecting
// window (true) or its connecting state has gone stale (false). A stale
// connecting proxy is treated as degraded rather than "connecting".
func (h *proxyHealth) connectingActive(now time.Time) bool {
	if !h.connecting {
		return false
	}
	if h.connectingSince.IsZero() {
		// never stamped (pre-bound state or RegisterProxyBandwidth init): treat
		// as active so the bound doesn't retroactively break existing behavior
		return true
	}
	return now.Sub(h.connectingSince) < connectingStaleAfter
}

// ProxyEvent identifies a proxy in a transition list. After is set for
// recovered events (time the proxy was down before coming back).
type ProxyEvent struct {
	Index   int
	Address string
	After   time.Duration
}

// ProxyHealthReport is the full per-heartbeat result.
type ProxyHealthReport struct {
	Up       int
	Dead     []string // formatted "proxy[idx] (addr)", index-sorted, complete (uncapped)
	Degraded []string

	Recovered     []ProxyEvent // down->up since last heartbeat
	NewlyDegraded []ProxyEvent // up->down since last heartbeat
	NewlyDead     []ProxyEvent // never-up proxies newly confirmed dead (logged once)

	LifetimeRecovered int
	LifetimeLost      int

	Bandwidth map[string]*bandwidth.ProxyBandwidth
}

var (
	proxyHealthMu      sync.Mutex
	proxyHealthByIndex = map[int]*proxyHealth{}
	// addr -> health, kept in sync with proxyHealthByIndex so ProxyBandwidthByAddress
	// is O(1). It is polled every 5s per draining proxy during hot-reload; a linear
	// scan there was O(proxies) under the global lock for every poll.
	proxyHealthByAddr = map[string]*proxyHealth{}

	proxyLifetimeRecovered int
	proxyLifetimeLost      int
	proxyBaselineSet       bool

	// proxyHealthGen is a monotonic counter incremented on each RegisterProxy.
	// UnregisterProxy receives the value captured at registration and only
	// cleans up if the entry hasn't been re-registered since.
	proxyHealthGen atomic.Uint64
)

// RegisterProxy adds or updates a proxy in the health registry.
func RegisterProxy(index int, address string) uint64 {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	h, ok := proxyHealthByIndex[index]
	if !ok {
		h = &proxyHealth{}
		proxyHealthByIndex[index] = h
	}
	if h.address != "" && h.address != address {
		// Different address at the same index: full reset — the old
		// proxy instance is being replaced by a different one.
		delete(proxyHealthByAddr, h.address)
		h.everUp = false
		h.currentlyUp = false
		h.downSince = time.Time{}
		h.lastSeenUp = false
		h.deadLogged = false
		h.lastError = ""
		h.lastErrorAt = time.Time{}
		h.failures = ProxyFailureCounters{}
	}
	h.address = address
	h.connecting = true
	h.connectingSince = time.Now()

	// Reset per-instance error/failure state so a re-registered proxy
	// starts clean. Live connection state (currentlyUp, everUp) is
	// preserved: a proxy re-registering during hot-reload may still be
	// actively serving traffic, and resetting its up state would cause
	// a spurious "connecting" blip in health reports.
	h.lastError = ""
	h.lastErrorAt = time.Time{}
	h.failures = ProxyFailureCounters{}

	gen := proxyHealthGen.Add(1)
	h.regen = gen

	proxyHealthByAddr[address] = h
	return gen
}

// RegisterProxyBandwidth securely retrieves or initializes the proxyBandwidth.
// Returns nil if the proxy has not been registered via RegisterProxy first —
// callers that run before RegisterProxy will get nil rather than a health
// entry with an empty address that pollutes the registry (invariant:
// proxyHealthByIndex and proxyHealthByAddr must stay in sync).
func RegisterProxyBandwidth(index int) *bandwidth.ProxyBandwidth {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	h, ok := proxyHealthByIndex[index]
	if !ok {
		return nil
	}
	if h.bw == nil {
		h.bw = &bandwidth.ProxyBandwidth{}
	}
	return h.bw
}

// MarkProxyUp records that the proxy's platform transport is live.
func MarkProxyUp(index int) {
	markProxyUp(index)
}

// markProxyUp records that the proxy's platform transport is live.
func markProxyUp(index int) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[index]; ok {
		h.currentlyUp = true
		h.everUp = true
		h.connecting = false
		h.connectingSince = time.Time{}
	}
}

// ProxyBandwidthByAddress returns the ProxyBandwidth for a given address, or nil.
func ProxyBandwidthByAddress(addr string) *bandwidth.ProxyBandwidth {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByAddr[addr]; ok {
		return h.bw
	}
	return nil
}

// MarkProxyDown records that the proxy's platform transport went down, stamping
// downSince when it was previously up (for recovery-latency reporting).
func MarkProxyDown(index int) {
	markProxyDown(index)
}

// markProxyDown records that the proxy's platform transport went down, stamping
// downSince when it was previously up or when downSince is zero (for never-up
// proxies that need accurate inactive classification).
func markProxyDown(index int) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[index]; ok {
		if h.downSince.IsZero() || h.currentlyUp {
			h.downSince = time.Now()
		}
		h.currentlyUp = false
		h.connecting = false
		h.connectingSince = time.Time{}
	}
}

// isTimeoutError returns true if err indicates a timeout — either a
// context deadline or an underlying net.Error with Timeout()==true.
// Avoids fragile substring-matching on error strings.
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// RecordProxyAuthFailure increments the auth-failure counter for a proxy.
func RecordProxyAuthFailure(index int, err error) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[index]; ok {
		errStr := ""
		if err != nil {
			errStr = err.Error()
			if isTimeoutError(err) {
				h.failures.TimeoutFailures.Add(1)
			} else {
				h.failures.AuthFailures.Add(1)
			}
		}
		h.lastError = errStr
		h.lastErrorAt = time.Now()
	}
}

// RecordProxyTransportDrop increments the transport-drop counter for a proxy.
func RecordProxyTransportDrop(index int, err error) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[index]; ok {
		h.failures.TransportDrops.Add(1)
		if err != nil {
			h.lastError = err.Error()
			h.lastErrorAt = time.Now()
		}
	}
}

// ProxyAuthFailureCount returns the cumulative transport auth-failure count
// for a proxy index, or 0 if the proxy isn't registered.
func ProxyAuthFailureCount(index int) int64 {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[index]; ok {
		return h.failures.AuthFailures.Load()
	}
	return 0
}

// ProxyEverUp reports whether the proxy's transport has come up at least
// once since it was registered.
func ProxyEverUp(index int) bool {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[index]; ok {
		return h.everUp
	}
	return false
}

// UnregisterProxySafe removes a proxy from the health registry only if the
// registration generation still matches (no re-registration happened since
// the goroutine started). Must be called after the goroutine exits.
func UnregisterProxySafe(id int, gen uint64) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	h, ok := proxyHealthByIndex[id]
	if !ok || h.address == "" {
		delete(proxyHealthByIndex, id)
		return
	}
	// Stale goroutine: the proxy was re-registered since this goroutine
	// started, so its cleanup must not destroy the fresh entry.
	if h.regen != gen {
		return
	}
	deleteProxyIndex(h.address)
	// only drop the addr entry if it still points at this proxy
	if proxyHealthByAddr[h.address] == h {
		delete(proxyHealthByAddr, h.address)
	}
	delete(proxyHealthByIndex, id)
}

// UnregisterProxy removes a proxy from the health registry unconditionally.
// Use UnregisterProxySafe for production goroutines that may race with
// hot-reload re-registration; this variant is kept for tests and compat
// wrappers where the generation is unknown.
func UnregisterProxy(id int) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	if h, ok := proxyHealthByIndex[id]; ok && h.address != "" {
		deleteProxyIndex(h.address)
		if proxyHealthByAddr[h.address] == h {
			delete(proxyHealthByAddr, h.address)
		}
	}
	delete(proxyHealthByIndex, id)
}

// ProxyHealthCount returns the number of registered proxies (0 = non-proxy mode).
func ProxyHealthCount() int {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	return len(proxyHealthByIndex)
}

func formatProxyEntry(index int, address string) string {
	return fmt.Sprintf("proxy[%d] (%s)", index, address)
}

func formatProxyErrorEntry(index int, address string, failures *ProxyFailureCounters, lastError string, lastErrorAt time.Time) string {
	base := formatProxyEntry(index, address)
	var parts []string
	if auth := failures.AuthFailures.Load(); auth > 0 {
		parts = append(parts, fmt.Sprintf("auth:%d", auth))
	}
	if timeout := failures.TimeoutFailures.Load(); timeout > 0 {
		parts = append(parts, fmt.Sprintf("timeout:%d", timeout))
	}
	if drops := failures.TransportDrops.Load(); drops > 0 {
		parts = append(parts, fmt.Sprintf("drops:%d", drops))
	}
	if lastError != "" {
		if !lastErrorAt.IsZero() {
			parts = append(parts, fmt.Sprintf("last_err:%q (%s ago)", lastError, time.Since(lastErrorAt).Round(time.Second)))
		} else {
			parts = append(parts, fmt.Sprintf("last_err:%q", lastError))
		}
	}
	if len(parts) > 0 {
		return fmt.Sprintf("%s [%s]", base, strings.Join(parts, ", "))
	}
	return base
}

// sortedIndicesLocked returns registry indices in ascending order. Caller holds the lock.
func sortedIndicesLocked() []int {
	indices := make([]int, 0, len(proxyHealthByIndex))
	for idx := range proxyHealthByIndex {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	return indices
}

// ProxyHealthSnapshot returns the current state without advancing the transition
// baseline, so it is safe to call from the pulse-fire marker. Lists are complete
// (no display cap) and index-sorted.
func ProxyHealthSnapshot() (up int, dead []string, degraded []string, bwMap map[string]*bandwidth.ProxyBandwidth, connecting []string) {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	now := time.Now()
	bwMap = make(map[string]*bandwidth.ProxyBandwidth)
	for _, idx := range sortedIndicesLocked() {
		h := proxyHealthByIndex[idx]
		coarse := classifyProxyHealth(h, now)
		switch coarse {
		case "up":
			up++
		case "connecting":
			// a re-registered instance reuses the struct and inherits its
			// predecessor's everUp/downSince, so a fresh `connecting` must win
			// over `everUp` or a respawning proxy reads as degraded (or dead,
			// when the inherited downSince is older than 7d) mid-respawn.
			connecting = append(connecting, formatProxyEntry(idx, h.address))
		case "degraded":
			degraded = append(degraded, formatProxyEntry(idx, h.address))
		case "dead":
			dead = append(dead, formatProxyEntry(idx, h.address))
		}

		if h.bw != nil {
			pb := proxyBandwidthSnapshot(h.bw)
			bwMap[formatProxyEntry(idx, h.address)] = pb
		}
	}
	return up, dead, degraded, bwMap, connecting
}

// ProxyHealthHeartbeat builds the per-heartbeat report and advances the transition
// baseline. Call exactly once per heartbeat. On the first call it only establishes
// the baseline (no transition events). NewlyDead is populated only when confirmDead
// is true (caller passes uptime >= deadConfirmDelay), once per never-up proxy.
func ProxyHealthHeartbeat(confirmDead bool) ProxyHealthReport {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()

	now := time.Now()
	first := !proxyBaselineSet
	var r ProxyHealthReport
	r.Bandwidth = make(map[string]*bandwidth.ProxyBandwidth)

	for _, idx := range sortedIndicesLocked() {
		h := proxyHealthByIndex[idx]

		switch {
		case h.currentlyUp:
			r.Up++
		case h.connectingActive(now):
			// still trying to connect — not degraded or dead. Checked before
			// everUp for the same reason as ProxyHealthSnapshot: a re-registered
			// instance inherits its predecessor's everUp/downSince and must not
			// read as degraded while its fresh connection attempt is running.
		case h.everUp:
			r.Degraded = append(r.Degraded, formatProxyErrorEntry(idx, h.address, &h.failures, h.lastError, h.lastErrorAt))
		default:
			r.Dead = append(r.Dead, formatProxyErrorEntry(idx, h.address, &h.failures, h.lastError, h.lastErrorAt))
		}

		if h.bw != nil {
			pb := proxyBandwidthSnapshot(h.bw)
			r.Bandwidth[formatProxyEntry(idx, h.address)] = pb
		}

		if !first {
			switch {
			case h.currentlyUp && !h.lastSeenUp:
				ev := ProxyEvent{Index: idx, Address: h.address}
				if !h.downSince.IsZero() {
					ev.After = now.Sub(h.downSince)
				}
				r.Recovered = append(r.Recovered, ev)
				proxyLifetimeRecovered++
			case !h.currentlyUp && h.lastSeenUp:
				r.NewlyDegraded = append(r.NewlyDegraded, ProxyEvent{Index: idx, Address: h.address})
				proxyLifetimeLost++
			}
		}

		// Only mark as "newly dead" if the proxy was NOT connecting
		// (never got a chance = still trying = not dead)
		if confirmDead && !h.currentlyUp && !h.everUp && !h.connectingActive(now) && !h.deadLogged {
			r.NewlyDead = append(r.NewlyDead, ProxyEvent{Index: idx, Address: h.address})
			h.deadLogged = true
		}

		h.lastSeenUp = h.currentlyUp
	}

	proxyBaselineSet = true
	r.LifetimeRecovered = proxyLifetimeRecovered
	r.LifetimeLost = proxyLifetimeLost
	return r
}

// ProxyHealthStatus represents the live health of a proxy
type ProxyHealthStatus struct {
	Health         string
	DownSince      time.Time
	AuthFailures   int64
	TransportDrops int64
	TimeoutFails   int64
	LatencyMs      int64
	SocksLatencyMs int64
}

// ProxyHealthByAddress returns the current health classification for each
// registered proxy, keyed by address. Used to update proxy.state snapshots.
// Uses classifyProxyHealth for the coarse classification so the threshold
// boundaries match ProxyHealthSnapshot and the heartbeat report.
func ProxyHealthByAddress() map[string]ProxyHealthStatus {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	now := time.Now()
	result := make(map[string]ProxyHealthStatus, len(proxyHealthByIndex))
	for _, h := range proxyHealthByIndex {
		coarse := classifyProxyHealth(h, now)
		// For the "degraded" tier, provide fine-grained sub-status via
		// degradedTierFromDuration so operators can distinguish recent
		// from long-standing outages. For "dead" and other coarse labels,
		// use them directly to stay consistent with ProxyHealthSnapshot.
		health := coarse
		if coarse == "degraded" {
			health = degradedTierFromDuration(time.Since(h.downSince))
		}
		latencyNs := int64(0)
		socksLatencyNs := int64(0)
		if h.bw != nil {
			latencyNs = h.bw.LatencyNs.Load()
			socksLatencyNs = h.bw.SocksLatencyNs.Load()
		}
		result[h.address] = ProxyHealthStatus{
			Health:         health,
			DownSince:      h.downSince,
			AuthFailures:   h.failures.AuthFailures.Load(),
			TransportDrops: h.failures.TransportDrops.Load(),
			TimeoutFails:   h.failures.TimeoutFailures.Load(),
			LatencyMs:      latencyNs / 1_000_000,
			SocksLatencyMs: socksLatencyNs / 1_000_000,
		}
	}
	return result
}

func degradedTierFromDuration(d time.Duration) string {
	switch {
	case d < 24*time.Hour:
		return "recently_offline"
	case d < 72*time.Hour:
		return "offline"
	case d < 7*24*time.Hour:
		return "long_offline"
	default:
		return "inactive"
	}
}

// classifyProxyHealth returns the coarse health classification for a proxy.
// This is the single source of truth for the up/connecting/degraded/dead
// threshold, used by ProxyHealthSnapshot, ProxyHealthByAddress, and the
// heartbeat's DegradedProxies predicate.
//
// Classification mapping:
//
//	"connecting"  — fresh connecting window (connectingStaleAfter)
//	"up"          — currentlyUp
//	"degraded"    — everUp but down < 7d, or connecting window stale,
//	               or everUp but down >= 7d (fine-tier "inactive" via
//	               degradedTierFromDuration in ProxyHealthByAddress)
//	"dead"        — never-up
func classifyProxyHealth(h *proxyHealth, now time.Time) string {
	switch {
	case h.currentlyUp:
		return "up"
	case h.connectingActive(now):
		return "connecting"
	case h.everUp:
		return "degraded"
	default:
		return "dead"
	}
}

// DegradedProxyEntry captures metrics and duration for a degraded proxy.
type DegradedProxyEntry struct {
	Index        int
	Address      string
	DownFor      time.Duration
	TotalRxBytes uint64
	TotalTxBytes uint64
}

// IsDegraded reports whether the proxy at address is degraded right now.
// Mirrors DegradedProxies()'s predicate, plus excludes an instance that is
// mid-connect: RegisterProxy sets connecting=true on every (re)registration
// and reuses the existing *proxyHealth struct for that index rather than
// resetting it, so a freshly respawned instance inherits its predecessor's
// stale everUp/downSince fields until it reports its own first up/down
// transition. Without the connecting check, a brand-new instance would read
// as "degraded" before it had ever attempted to connect. Callers use this to
// re-verify a proxy is still the same stuck instance a decision was made
// about moments earlier, not a since-recovered or since-replaced one.
func IsDegraded(address string) bool {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()
	h, ok := proxyHealthByAddr[address]
	if !ok {
		return false
	}
	return h.everUp && !h.currentlyUp && !h.downSince.IsZero() && !h.connectingActive(time.Now())
}

// DegradedProxies returns a list of all currently degraded proxies.
func DegradedProxies() []DegradedProxyEntry {
	proxyHealthMu.Lock()
	defer proxyHealthMu.Unlock()

	now := time.Now()
	var result []DegradedProxyEntry
	for idx, h := range proxyHealthByIndex {
		if h.everUp && !h.currentlyUp && !h.downSince.IsZero() && !h.connectingActive(now) {
			entry := DegradedProxyEntry{
				Index:   idx,
				Address: h.address,
				DownFor: now.Sub(h.downSince),
			}
			if h.bw != nil {
				entry.TotalRxBytes = h.bw.TotalRx.Load()
				entry.TotalTxBytes = h.bw.TotalTx.Load()
			}
			result = append(result, entry)
		}
	}

	return result
}

// activeConnectionCount returns the current number of active client
// connections across all registered proxies. The fork sourced this from
// connect.ActiveConnectionCount(), an atomic counter incremented deep in
// connect's IP-layer code (ip.go); v2026 connect exposes no equivalent.
// Instead we sum the per-proxy client-session counts that this package
// already tracks in proxy_health.go/bandwidth for the [health] report and
// bandwidth_reporter.go, which is the same signal at proxy granularity.
func activeConnectionCount() int64 {
	_, _, _, bw, _ := ProxyHealthSnapshot()
	var total int64
	for _, b := range bw {
		total += b.Clients.Load()
	}
	return total
}

// activeProxyConnections returns the count of proxies currently reporting at
// least one active client. In the fork this was connect.ActiveProxyConnections(),
// an atomic counter maintained in connect's transport layer (transport.go)
// as transports were torn up/down; v2026 connect removed it. We derive the
// same "how many proxies are actively serving traffic" signal from the
// per-proxy client counts this package already tracks (proxy_health.go).
func activeProxyConnections() int64 {
	_, _, _, bw, _ := ProxyHealthSnapshot()
	var serving int64
	for _, b := range bw {
		if b.Clients.Load() > 0 {
			serving++
		}
	}
	return serving
}

// ResetProxyHealthForTesting clears the process-wide health registry so one
// unit test cannot observe registrations left behind by a shuffled predecessor.
// The registry is a package global shared by every proxy test; a proxy that
// resamples or relaunches keeps its health entry across tests unless the next
// test clears it, which makes assertions that watch the shared map order
// dependent. Test-only.
func ResetProxyHealthForTesting() {
	proxyHealthMu.Lock()
	proxyHealthByIndex = map[int]*proxyHealth{}
	proxyHealthByAddr = map[string]*proxyHealth{}
	proxyLifetimeRecovered = 0
	proxyLifetimeLost = 0
	proxyBaselineSet = false
	proxyHealthMu.Unlock()
}
