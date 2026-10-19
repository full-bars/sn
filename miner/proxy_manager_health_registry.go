package miner

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// proxy_manager_health_registry.go replaces the P1(a) no-op health seams with
// the health registry the reaper and trim read, and drives the v33 health log
// files (proxy_manager_health_log.go).
//
// v33's registry is connect's own (`connect.ProxyHealthByKey`,
// `connect.ProxyHealthReportFrom`), fed by the client strategy's per-proxy
// connection state. The pinned full-bars/connect does not export those, so the
// port keeps the same shape and keying (identity key, see
// connect.ProxySettings.Key) and fills it from what the miner can actually
// observe:
//
//   - the auth path (noteProviderAuthSuccess / noteProviderAuthFailure): a
//     proxy that just authenticated is "up"; one that just failed is not, so
//     the URL reaper stops shielding it from demotion;
//   - the persisted proxy.state health, seeded at manager start and overlaid
//     after each health cycle, so health survives a restart exactly as v33's
//     state write does.
//
// Behavior wired by this file (v33 parity):
//
//   - liveProxyHealthByKey now returns the registry snapshot, so
//     proxyHeldOrConnecting shields a live proxy from reaper demotion and the
//     degraded freeze can be honored;
//   - the manager overlays live health onto proxy.state before the startup
//     trim ranks, so trim sees health, and writes the overlaid state on the
//     health cadence;
//   - the health log files (proxy_health.state, proxy_health.log,
//     proxy_traffic.state, usage_history.jsonl) are written on the same
//     cadence, so the ported proxy subcommands have real content to read.

// proxyHealthLogInterval is how often the process writes its health snapshot.
// v33 writes from its provider heartbeat (a few seconds); the miner has no
// heartbeat loop, so the port uses a calm fixed cadence that also bounds how
// stale proxy.state's health column can be.
const proxyHealthLogInterval = 60 * time.Second

// proxyLiveHealthEntry is one identity's health as this process knows it.
type proxyLiveHealthEntry struct {
	Health       string
	DownSince    time.Time
	AuthFailures int64
	LastSeen     time.Time
}

// proxyLiveHealthRegistry is the process-wide live health, keyed by proxy
// identity (address, or address+user for a credentialed entry).
var proxyLiveHealthRegistry = struct {
	sync.Mutex
	byKey map[string]proxyLiveHealthEntry
}{byKey: map[string]proxyLiveHealthEntry{}}

// noteLiveProxyHealthUp records a successful, live proxy: the reaper shields
// it and its down clock is cleared.
func noteLiveProxyHealthUp(key string) {
	if key == "" {
		return
	}
	proxyLiveHealthRegistry.Lock()
	defer proxyLiveHealthRegistry.Unlock()
	proxyLiveHealthRegistry.byKey[key] = proxyLiveHealthEntry{
		Health:   "up",
		LastSeen: time.Now(),
	}
}

// noteLiveProxyHealthFailure records an auth failure: the proxy is no longer
// "up" (so it is not shielded from reaper demotion), its down clock starts if
// it was up, and the auth failure count rises. v33 keeps the same pairing of
// Health and AuthFailures in the health snapshot.
func noteLiveProxyHealthFailure(key string) {
	if key == "" {
		return
	}
	now := time.Now()
	proxyLiveHealthRegistry.Lock()
	defer proxyLiveHealthRegistry.Unlock()
	entry := proxyLiveHealthRegistry.byKey[key]
	if entry.Health == "up" || entry.Health == "" {
		entry.Health = "recently_offline"
		if entry.DownSince.IsZero() {
			entry.DownSince = now
		}
	}
	entry.AuthFailures++
	entry.LastSeen = now
	proxyLiveHealthRegistry.byKey[key] = entry
}

// noteLiveProxyHealth records an observed health state for an identity (used
// when a cycle learns health from the persisted state or a probe result).
func noteLiveProxyHealth(key, health string, downSince time.Time) {
	if key == "" || health == "" {
		return
	}
	proxyLiveHealthRegistry.Lock()
	defer proxyLiveHealthRegistry.Unlock()
	entry := proxyLiveHealthRegistry.byKey[key]
	entry.Health = health
	entry.DownSince = downSince
	entry.LastSeen = time.Now()
	proxyLiveHealthRegistry.byKey[key] = entry
}

// proxyLiveHealthSnapshot implements the liveProxyHealthByKey seam: the
// registry's health per identity key, in the slice shape the URL reaper reads.
func proxyLiveHealthSnapshot() map[string]proxyHealthStatus {
	proxyLiveHealthRegistry.Lock()
	defer proxyLiveHealthRegistry.Unlock()
	if len(proxyLiveHealthRegistry.byKey) == 0 {
		return nil
	}
	out := make(map[string]proxyHealthStatus, len(proxyLiveHealthRegistry.byKey))
	for key, entry := range proxyLiveHealthRegistry.byKey {
		out[key] = proxyHealthStatus{Health: entry.Health}
	}
	return out
}

// resetProxyLiveHealthRegistryForTest clears the registry and the transition
// bookkeeping. Tests only: the lifetime counters are deliberately left alone,
// they are process-lifetime values in v33 too.
func resetProxyLiveHealthRegistryForTest() {
	proxyLiveHealthRegistry.Lock()
	defer proxyLiveHealthRegistry.Unlock()
	proxyLiveHealthRegistry.byKey = map[string]proxyLiveHealthEntry{}
	proxyHealthRegistrySeen = map[string]string{}
}

// resetProxyHealthReportStateForTest clears the registry and the previous
// report's buckets, so one test's transitions cannot leak into the next.
func resetProxyHealthReportStateForTest() { resetProxyLiveHealthRegistryForTest() }

// seedLiveProxyHealthFromState loads the persisted health of every known
// identity into the registry. Entries the registry already knows win: a live
// signal observed in this process is fresher than the file.
func seedLiveProxyHealthFromState(state *ProxyState) int {
	if state == nil {
		return 0
	}
	now := time.Now()
	seeded := 0
	proxyLiveHealthRegistry.Lock()
	defer proxyLiveHealthRegistry.Unlock()
	for key, entry := range state.Proxies {
		if _, ok := proxyLiveHealthRegistry.byKey[key]; ok {
			continue
		}
		if entry.Health == "" {
			continue
		}
		var downSince time.Time
		if entry.DownSince != "" {
			if t, err := time.Parse(time.RFC3339, entry.DownSince); err == nil {
				downSince = t
			}
		}
		proxyLiveHealthRegistry.byKey[key] = proxyLiveHealthEntry{
			Health:       entry.Health,
			DownSince:    downSince,
			AuthFailures: entry.AuthFailures,
			LastSeen:     now,
		}
		seeded++
	}
	return seeded
}

// overlayLiveProxyHealth copies the registry's health onto the state entries
// (health, down-since and auth failures, exactly the three fields v33's state
// write updates) and reports how many entries changed.
func overlayLiveProxyHealth(proxies map[string]ProxyEntry) int {
	if len(proxies) == 0 {
		return 0
	}
	changed := 0
	proxyLiveHealthRegistry.Lock()
	defer proxyLiveHealthRegistry.Unlock()
	for key, entry := range proxies {
		live, ok := proxyLiveHealthRegistry.byKey[key]
		if !ok || live.Health == "" {
			continue
		}
		health := live.Health
		downSince := ""
		if !live.DownSince.IsZero() {
			downSince = live.DownSince.Format(time.RFC3339)
		}
		if entry.Health == health && entry.DownSince == downSince && entry.AuthFailures == live.AuthFailures {
			continue
		}
		entry.Health = health
		entry.DownSince = downSince
		entry.AuthFailures = live.AuthFailures
		proxies[key] = entry
		changed++
	}
	return changed
}

// proxyHealthBuckets classifies a persisted health value the way the v33
// report does: "up" is up, a never-up proxy is dead, everything else is a
// degraded (dropped) entry.
func proxyHealthBucket(health string) string {
	switch health {
	case "up":
		return "up"
	case "", "dead", "inactive":
		return "dead"
	default:
		return "degraded"
	}
}

// buildProxyHealthReport renders this process's health report from the state
// and the registry, with the transition lists and the lifetime counters the
// health log's event lines need. The previous bucket per identity is kept in
// the registry so consecutive reports produce exactly one transition line per
// change, as v33's per-heartbeat report does.
func buildProxyHealthReport(state *ProxyState, now time.Time) proxyHealthReport {
	report := proxyHealthReport{Bandwidth: map[string]*proxyBandwidth{}}
	if state == nil {
		return report
	}
	overlayLiveProxyHealth(state.Proxies)

	type bucketEntry struct {
		id     int
		bucket string
	}
	current := map[string]bucketEntry{}
	for key, entry := range state.Proxies {
		bucket := proxyHealthBucket(entry.Health)
		current[key] = bucketEntry{id: entry.ID, bucket: bucket}
		switch bucket {
		case "up":
			report.Up++
		case "dead":
			report.Dead = append(report.Dead, fmt.Sprintf("proxy[%d] (%s)", entry.ID, proxyKeyDisplay(key)))
		default:
			report.Degraded = append(report.Degraded, fmt.Sprintf("proxy[%d] (%s)", entry.ID, proxyKeyDisplay(key)))
		}
	}
	sort.Strings(report.Dead)
	sort.Strings(report.Degraded)

	for key, cur := range current {
		prev, seen := proxyHealthRegistrySeen[key]
		addr := proxyKeyDisplay(key)
		switch {
		case seen && prev == "up" && cur.bucket != "up":
			if cur.bucket == "dead" {
				report.NewlyDead = append(report.NewlyDead, proxyEvent{Index: cur.id, Address: addr})
			} else {
				report.NewlyDegraded = append(report.NewlyDegraded, proxyEvent{Index: cur.id, Address: addr})
			}
			proxyHealthLifetimeLost++
		case seen && prev != "up" && cur.bucket == "up":
			report.Recovered = append(report.Recovered, proxyEvent{Index: cur.id, Address: addr})
			proxyHealthLifetimeRecovered++
		}
	}
	report.LifetimeRecovered = proxyHealthLifetimeRecovered
	report.LifetimeLost = proxyHealthLifetimeLost

	proxyHealthRegistrySeen = map[string]string{}
	for key, cur := range current {
		proxyHealthRegistrySeen[key] = cur.bucket
	}
	return report
}

var (
	// proxyHealthRegistrySeen is the previous report's bucket per identity.
	proxyHealthRegistrySeen = map[string]string{}
	// proxyHealthLifetimeRecovered/Lost are the v33 lifetime transition
	// counters the state file renders.
	proxyHealthLifetimeRecovered int
	proxyHealthLifetimeLost      int
)

// writeProxyHealthSnapshot renders the report and writes the v33 health files
// in the health directory, and persists the overlaid health back into
// proxy.state. It reports whether the files were written (the health directory
// can be unresolvable, which v33 treats as "persistence disabled").
func writeProxyHealthSnapshot(now time.Time) bool {
	dir, ok := proxyHealthDir()
	if !ok {
		return false
	}
	state, err := readProxyState()
	if err != nil {
		tlog("[proxy][health] warn: could not read proxy.state for the health snapshot: %v\n", err)
		return false
	}
	overlayed := overlayLiveProxyHealth(state.Proxies)
	report := buildProxyHealthReport(state, now)
	writeProxyHealthState(dir, report, now)
	writeProxyHealthEvents(dir, report, now)
	writeProxyTrafficState(dir, report, now)
	writeUsageHistory(dir, report, now)
	if overlayed > 0 {
		if err := writeProxyState(state); err != nil {
			tlog("[proxy] warn: state write failed: %v\n", err)
		}
	}
	return true
}

// runProxyHealthLog is the health cadence loop: one snapshot at start (so a
// fresh process immediately has the files the subcommands read), then one per
// interval, and a final event flush on shutdown.
func runProxyHealthLog(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = proxyHealthLogInterval
	}
	writeProxyHealthSnapshot(time.Now())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			writeProxyHealthSnapshot(now)
		}
	}
}

// proxyHealthRegistrySummary is the one-line state of the registry for the
// per-cycle log (up/degraded/dead counts and the auth-failure total).
func proxyHealthRegistrySummary() string {
	proxyLiveHealthRegistry.Lock()
	defer proxyLiveHealthRegistry.Unlock()
	up, degraded, dead := 0, 0, 0
	var authFailures int64
	for _, entry := range proxyLiveHealthRegistry.byKey {
		switch proxyHealthBucket(entry.Health) {
		case "up":
			up++
		case "dead":
			dead++
		default:
			degraded++
		}
		authFailures += entry.AuthFailures
	}
	return fmt.Sprintf("live health: up=%d degraded=%d dead=%d auth_failures=%d", up, degraded, dead, authFailures)
}

// proxyLiveHealthKeys returns the registry's identity keys, sorted. Tests and
// diagnostics only.
func proxyLiveHealthKeys() []string {
	proxyLiveHealthRegistry.Lock()
	defer proxyLiveHealthRegistry.Unlock()
	keys := make([]string, 0, len(proxyLiveHealthRegistry.byKey))
	for key := range proxyLiveHealthRegistry.byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// trimLiveHealthOverlay is the trim-side read: the live health per identity
// key, so the shed ranking sees a proxy that is live right now even before the
// state file has been rewritten. It is exported to the trim path through
// overlayLiveProxyHealth.
func trimLiveHealthOverlay() map[string]string {
	proxyLiveHealthRegistry.Lock()
	defer proxyLiveHealthRegistry.Unlock()
	if len(proxyLiveHealthRegistry.byKey) == 0 {
		return nil
	}
	out := make(map[string]string, len(proxyLiveHealthRegistry.byKey))
	for key, entry := range proxyLiveHealthRegistry.byKey {
		if entry.Health != "" {
			out[key] = entry.Health
		}
	}
	return out
}
