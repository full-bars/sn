package miner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync/atomic"
	"time"
)

// Ported from the 3.23-fix v33 provider's provider/health_heartbeat.go.
//
// What the provider's heartbeat is: a periodic proof that this process is alive
// plus a reading of its own health, so an external monitor can tell a process
// that is running but wedged from one that is serving. The miner already writes
// proxy health on a cadence (runProxyHealthLog, proxyHealthLogInterval), so the
// port integrates with that writer instead of duplicating it:
//
//   - One writer, one file. The pulse publishes a fixed-size snapshot in this
//     package; the existing status writer renders it as the ` heartbeat: ` line
//     of proxy_health.state. There is no second heartbeat file, and the pulse
//     loop itself does no file I/O: the history-store prune, which does read
//     three files, runs on its own loop and its own cadence
//     (runProxyHistoryPruneLoop), so a slow disk can delay a prune but never a
//     stamp. A late prune is recoverable; a false "wedged" alert is not.
//   - The per-proxy blocks stay where the miner already has them
//     (runProxyHealthLog and the ported proxy subcommands), reading the same
//     live registry this snapshot counts.
//   - connections= and h3= are NOT reported. The miner's connection accounting
//     is connect-internal (metrics_seams.go) and its H3 identity set is not
//     exported, so either number would be invented. proxies/up/down and clients
//     are the equivalents the miner really has.
//
// Why the heartbeat stamp is not the status file's own Updated: line. Updated:
// moves whenever the 60s status writer runs, so it proves the writer is alive.
// The heartbeat stamp moves only on a pulse, so a heartbeat whose age exceeds
// its own stale_after while Updated: keeps advancing means the pulse loop
// stopped — that is the wedge signal, and it survives the status writer still
// ticking.
//
// That discrimination is one-directional, and a monitor must not read it as
// symmetric. Both stamps reach disk through the status writer, so the two
// failure directions are not equally visible:
//
//   - Pulse stops, writer alive: Updated: advances, heartbeat.at freezes. A
//     monitor sees a stale heartbeat on a file that is still being rewritten and
//     can name the pulse loop as the stalled part. This is the case the design
//     detects.
//   - Writer stops, pulse alive: the pulse only stores into memory, so nothing
//     reaches proxy_health.state and BOTH Updated: and heartbeat.at freeze at
//     their last written values. A file monitor cannot distinguish a wedged
//     status writer from a wedged process: it sees one file go silent either
//     way. Detecting that direction needs a second writer outside this process
//     (systemd's watchdog, or a monitor that also checks the process is alive
//     and its [health] log lines keep arriving), not a second file written by
//     this one.
//
// So a stale heartbeat means "the pulse loop, or everything that feeds this
// file, has stopped" — enough to page, not enough to name the goroutine.

const (
	// healthHeartbeatDefaultInterval is the provider's cadence.
	healthHeartbeatDefaultInterval = 5 * time.Minute

	// healthHeartbeatMinInterval is the floor URNETWORK_HEALTH_INTERVAL may not
	// go under, matching the provider's: a tighter pulse is a load knob, not a
	// liveness knob.
	healthHeartbeatMinInterval = time.Minute

	// healthHeartbeatStatusGrace is slack added to the rendered staleness
	// threshold. The status file shows the heartbeat of the last pulse, so a
	// healthy stamp can look up to one status-write interval old; the grace
	// covers that plus scheduling jitter.
	healthHeartbeatStatusGrace = 30 * time.Second
)

// healthHeartbeat is one pulse: fixed size, no maps or slices, so both the
// process carrying it and a monitor reading it pay a constant.
type healthHeartbeat struct {
	At       time.Time
	Interval time.Duration
	Uptime   time.Duration

	Version string
	Profile string
	Host    string
	PID     int

	// HeapMiB is allocated heap objects and SysMiB the total obtained from the
	// OS — the provider's two /memory/classes readings, read through
	// runtime/metrics (as upstream does) rather than runtime.ReadMemStats,
	// which stops the world.
	HeapMiB    uint64
	SysMiB     uint64
	Goroutines int

	Proxies int
	// ProxiesUp is honestProxiesUp(): the proxies a live slot holds
	// control-plane evidence for in THIS process right now. It falls when a
	// slot exits or loses its credential. ReachedEver is the monotonic
	// companion (proxies that reached at least once here), log-only.
	// LastKnownUp is the persisted grade, kept beside them so a restart cannot
	// present an inherited pool as if this process had proven it.
	ProxiesUp   int
	ReachedEver int
	LastKnownUp int
	ProxiesDown int
	Clients     int64
	// CountsKnown is false when the byte/client counters were not published
	// yet, so Clients is "not read" rather than "zero clients".
	CountsKnown bool
}

// healthHeartbeatRecord is the on-disk shape of the heartbeat line. Seconds are
// floats so a short cadence (a test, or an operator's 90s) does not round to
// zero and read as "stalled immediately".
type healthHeartbeatRecord struct {
	At            string  `json:"at"`
	IntervalSec   float64 `json:"interval_seconds"`
	StaleAfterSec float64 `json:"stale_after_seconds"`
	UptimeSec     float64 `json:"uptime_seconds"`
	Version       string  `json:"version"`
	Profile       string  `json:"profile"`
	Host          string  `json:"host"`
	PID           int     `json:"pid"`
	HeapMiB       uint64  `json:"heap_mib"`
	SysMiB        uint64  `json:"sys_mib"`
	Goroutines    int     `json:"goroutines"`
	Proxies       int     `json:"proxies"`
	// ProxiesUp is honestProxiesUp(): proxies currently holding control-plane
	// evidence in this process. LastKnownUp is the persisted grade proxies_up
	// used to report, kept for monitors that want it under its real name.
	ProxiesUp   int   `json:"proxies_up"`
	LastKnownUp int   `json:"last_known_up"`
	ProxiesDown int   `json:"proxies_down"`
	Clients     int64 `json:"clients"`
	CountsKnown bool  `json:"counts_known"`
}

// healthHeartbeatCurrent is the last pulse, nil before the first one. An atomic
// pointer because the pulse goroutine writes it while the status writer reads
// it; neither takes a lock, so a stalled pulse can never block the writer.
var healthHeartbeatCurrent atomic.Pointer[healthHeartbeat]

// proxyHealthWriterCadence is the status writer's own interval in nanoseconds:
// the cadence that actually renders the heartbeat line, not the constant the
// writer defaults to. runProxyHealthLog stores its interval here (it is started
// with proxyHealthLogInterval in production but is injectable for tests and
// subcommands), and staleAfter adds it to the pulse interval because the
// on-disk stamp can be a full status-write old.
//
// Pre-set to the production default so a reader before the loop starts — and
// every test that renders a state file directly — gets the documented 60s.
var proxyHealthWriterCadence = func() *atomic.Int64 {
	v := &atomic.Int64{}
	v.Store(int64(proxyHealthLogInterval))
	return v
}()

// proxyHealthWriterInterval is the status-writer cadence in force.
func proxyHealthWriterInterval() time.Duration {
	return time.Duration(proxyHealthWriterCadence.Load())
}

// healthHeartbeatSnapshot returns the last pulse, or nil before the first one.
func healthHeartbeatSnapshot() *healthHeartbeat {
	return healthHeartbeatCurrent.Load()
}

// resetHealthHeartbeatForTest clears the published pulse. Tests only.
func resetHealthHeartbeatForTest() {
	healthHeartbeatCurrent.Store(nil)
}

// staleAfter is how old At may get before the pulse loop is presumed stopped.
// It is one pulse interval to the next stamp, plus one status write for the
// stamp to reach disk, plus the grace. The status-write term is read from the
// writer's live cadence rather than assumed to be the 60s default, so a status
// file that renders a heartbeat for a writer started at another interval does
// not publish a stale_after that contradicts it.
func (h *healthHeartbeat) staleAfter() time.Duration {
	if h == nil {
		return 0
	}
	return h.Interval + proxyHealthWriterInterval() + healthHeartbeatStatusGrace
}

// record renders the snapshot for the status file.
func (h *healthHeartbeat) record() healthHeartbeatRecord {
	return healthHeartbeatRecord{
		At:            h.At.UTC().Format(time.RFC3339),
		IntervalSec:   h.Interval.Seconds(),
		StaleAfterSec: h.staleAfter().Seconds(),
		UptimeSec:     h.Uptime.Seconds(),
		Version:       h.Version,
		Profile:       h.Profile,
		Host:          h.Host,
		PID:           h.PID,
		HeapMiB:       h.HeapMiB,
		SysMiB:        h.SysMiB,
		Goroutines:    h.Goroutines,
		Proxies:       h.Proxies,
		ProxiesUp:     h.ProxiesUp,
		LastKnownUp:   h.LastKnownUp,
		ProxiesDown:   h.ProxiesDown,
		Clients:       h.Clients,
		CountsKnown:   h.CountsKnown,
	}
}

// healthHeartbeatStatusLine is the status file's heartbeat line, or "" when no
// pulse has happened. A missing line means "never pulsed", which a monitor must
// read as not healthy.
//
// The line is one JSON object after a fixed prefix, so a monitor greps
// "^ heartbeat: " and parses the rest; the prose around it stays readable.
func healthHeartbeatStatusLine(h *healthHeartbeat) string {
	if h == nil {
		return ""
	}
	data, err := json.Marshal(h.record())
	if err != nil {
		return ""
	}
	return " heartbeat: " + string(data) + "\n"
}

// healthHeartbeatInterval resolves the cadence: URNETWORK_HEALTH_INTERVAL when
// it parses as a positive duration, clamped up to the floor, and the provider's
// 5 minutes when it does not parse or is not a cadence at all.
//
// The clamp matters: the docs promise "minimum 1m", so an operator who asks for
// 45s must get 1m, not silently get 5m — the slower default is a different
// answer than the one the floor describes, and it is the one an operator is
// least likely to notice.
func healthHeartbeatInterval(raw string) time.Duration {
	if raw = strings.TrimSpace(raw); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			if d < healthHeartbeatMinInterval {
				return healthHeartbeatMinInterval
			}
			return d
		}
	}
	return healthHeartbeatDefaultInterval
}

// heartbeatHostLabel names the node on the build line. The provider resolves a
// node name from control state; the miner has no such key, so HOST_HOSTNAME
// then the kernel hostname. This runs every pulse and never resolves the
// public IP, for the same reason the provider's does not.
func heartbeatHostLabel() string {
	hostname, _ := os.Hostname()
	return heartbeatHostLabelFrom(os.Getenv("HOST_HOSTNAME"), hostname)
}

// heartbeatHostLabelFrom holds the precedence so it can be tested without
// touching the environment or the host's own name.
func heartbeatHostLabelFrom(hostEnv, hostname string) string {
	for _, candidate := range []string{hostEnv, hostname} {
		if v := strings.TrimSpace(candidate); v != "" {
			return v
		}
	}
	return ""
}

// runHealthHeartbeat is the provider's loop: one pulse immediately, then one
// per interval until ctx ends.
//
// The immediate pulse is a deliberate difference from the provider, which waits
// for its first tick. The miner's status writer can run before the first tick,
// and a status file with no heartbeat line reads as "never pulsed"; pulsing
// once at start means a monitor that starts with the process is never looking
// at an empty file for a whole interval.
func runHealthHeartbeat(ctx context.Context, startTime time.Time, profile string) {
	runHealthHeartbeatWith(ctx, startTime, profile, healthHeartbeatConfiguredInterval())
}

// healthHeartbeatConfiguredInterval is the pulse cadence this process runs at,
// resolved once per caller from URNETWORK_HEALTH_INTERVAL. The pulse and the
// history prune both take their cadence from here so the two loops cannot
// disagree about it.
func healthHeartbeatConfiguredInterval() time.Duration {
	return healthHeartbeatInterval(os.Getenv("URNETWORK_HEALTH_INTERVAL"))
}

// runHealthHeartbeatWith is runHealthHeartbeat with the cadence injectable, so
// a test can observe the cadence without waiting five minutes.
//
// The loop does one thing per tick: publish. The history-store prune used to run
// here too, and it reads three files, so a slow disk held the goroutine past
// ticker.C and let an external monitor declare a healthy process wedged. The
// prune now has its own loop (runProxyHistoryPruneLoop) at the same cadence, so
// a late prune costs a prune, not a false alert.
func runHealthHeartbeatWith(ctx context.Context, startTime time.Time, profile string, interval time.Duration) {
	if interval < time.Nanosecond {
		interval = healthHeartbeatDefaultInterval
	}
	if profile == "" {
		profile = "default"
	}

	publishHealthHeartbeat(time.Now(), startTime, profile, interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publishHealthHeartbeat(time.Now(), startTime, profile, interval)
		}
	}
}

// runProxyHistoryPruneLoop bounds the per-identity stores this health picture is
// built from. A store that only grows is a leak the heartbeat would otherwise
// report as steady state.
//
// It is deliberately a separate loop from the pulse, not a step inside it: the
// prune reads proxy.state, the proxy list file and proxy_url.json, and anything
// that reads a file must not sit in front of a liveness stamp. Same cadence as
// the pulse, and still never at T=0: the first tick lands one interval in, after
// the run has reconciled its desired set with the URL cache, because a keep-set
// that is empty by timing rather than by truth would drop every identity's
// failure history (see pruneProxyHistoryStores).
func runProxyHistoryPruneLoop(ctx context.Context, interval time.Duration) {
	if interval < time.Nanosecond {
		interval = healthHeartbeatDefaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pruneProxyHistoryStores()
		}
	}
}

// healthLoopShutdownGrace bounds how long the run's deferred join waits for a
// health goroutine that is mid-syscall. Shutdown must not depend on a file read
// returning: the process can exit with a prune unfinished (it is idempotent and
// rebuilds from disk next start), but it cannot exit with a stuck join.
const healthLoopShutdownGrace = 2 * time.Second

// joinHealthLoop waits for a supervised health goroutine to finish, bounded by
// healthLoopShutdownGrace. It reports false when the join timed out, and the
// caller logs that rather than blocking shutdown forever.
func joinHealthLoop(done <-chan struct{}, name string) bool {
	timer := time.NewTimer(healthLoopShutdownGrace)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		tlog("[miner] warning: %s did not stop within %s; shutting down without it\n", name, healthLoopShutdownGrace)
		return false
	}
}

// publishHealthHeartbeat takes one reading, publishes it, and emits the
// provider's two lines. It is bounded work — a runtime/metrics read, three short
// registry locks and two writes to the log writer — so it neither measures nor
// blocks the provider run. It does no file I/O: the status writer renders the
// snapshot.
//
// The memory readings come from runtime/metrics, not runtime.ReadMemStats: the
// latter stops the world, and upstream's heartbeat migrated to the former for
// exactly that reason. Same two figures the provider reports.
func publishHealthHeartbeat(now time.Time, startTime time.Time, profile string, interval time.Duration) *healthHeartbeat {
	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/total:bytes"},
	}
	metrics.Read(samples)
	heapBytes := readUint64Metric(samples[0].Value)
	sysBytes := readUint64Metric(samples[1].Value)

	uptime := now.Sub(startTime)
	if uptime < 0 {
		uptime = 0
	}
	// proxyHealthRegistryCounts returns (up, degraded, dead, authFailures) in
	// that order. Bind all three buckets by name and fold both down buckets into
	// down: a positional binding here once read dead as "down" and dropped the
	// real dead count, so a fully dead pool reported proxies=0 up=0 down=0 — a
	// healthy-looking pool that had failed completely. The status file's own
	// report computes down the same way (len(r.Dead)+len(r.Degraded)).
	// proxyHealthRegistryCounts returns the PERSISTED health - proxy.state's
	// grades, seeded at manager start - so a process that has dialed nothing
	// yet still sees a pool full of previous-run "up" entries. That is the
	// figure that reported 300-400 up at uptime=0s, so it is reported as
	// last_known_up and never as up.
	//
	// up is honestProxiesUp() (provider_bringup.go), the same figure the
	// [profit] and [health][proxies] lines print: proxies whose slot reached
	// the control plane in this process and still holds that evidence.
	// proxies stays the pool size so the line still describes the pool rather
	// than this process's progress through it.
	persistedUp, degraded, dead, _ := proxyHealthRegistryCounts()
	lastKnownUp := persistedUp
	up := honestProxiesUp()
	down := degraded + dead
	_, _, clients, countsKnown := controlRuntime.counterTotals()

	version := RequireVersion()
	if version == "" {
		version = "unknown"
	}
	if profile == "" {
		profile = "default"
	}

	hb := &healthHeartbeat{
		At:          now,
		Interval:    interval,
		Uptime:      uptime.Truncate(time.Second),
		Version:     version,
		Profile:     profile,
		Host:        heartbeatHostLabel(),
		PID:         os.Getpid(),
		HeapMiB:     heapBytes / 1024 / 1024,
		SysMiB:      sysBytes / 1024 / 1024,
		Goroutines:  runtime.NumGoroutine(),
		Proxies:     lastKnownUp + down,
		ProxiesUp:   up,
		ReachedEver: proxiesReachedEver(),
		LastKnownUp: lastKnownUp,
		ProxiesDown: down,
		Clients:     clients,
		CountsKnown: countsKnown,
	}
	healthHeartbeatCurrent.Store(hb)

	// The provider leads with the build identity, then the health line: every
	// other reading is only interpretable once you know which build produced
	// it. The miner's line drops connections= and h3= rather than invent them.
	buildLine := fmt.Sprintf("🏷️ [health][build] %s profile=%s", hb.Version, hb.Profile)
	if hb.Host != "" {
		buildLine += " host=" + hb.Host
	}
	tlog("%s\n", buildLine)
	// up can fall (a slot exit or a rejected credential withdraws it), so it
	// may carry the word. reached_ever only rises and says so in its name.
	tlog("❤️ [health] uptime=%s profile=%s heap=%dMiB sys=%dMiB goroutines=%d proxies=%d up=%d reached_ever=%d last_known_up=%d down=%d clients=%d interval=%s\n",
		hb.Uptime, hb.Profile, hb.HeapMiB, hb.SysMiB, hb.Goroutines,
		hb.Proxies, hb.ProxiesUp, hb.ReachedEver, hb.LastKnownUp, hb.ProxiesDown, hb.Clients, hb.Interval)

	return hb
}

// pruneProxyHistoryStores drops failure-history and proven-proxy entries for
// identities no longer desired. See desiredAddressesForHistoryPruning for why
// this isn't just currently-registered health entries.
//
// The miner keeps both stores process-wide (proxy_manager_history.go) and never
// pruned them, so a node churning URL-sourced proxies grew them for the life of
// the process. The provider prunes from its heartbeat tick; the miner prunes
// from runProxyHistoryPruneLoop at the same cadence, on its own goroutine so the
// file reads here cannot delay a pulse.
func pruneProxyHistoryStores() {
	keepAddrs, pruneErr := desiredAddressesForHistoryPruning()
	if pruneErr != nil {
		tlog("[proxy] warning: could not determine desired proxy identities for history pruning: %v\n", pruneErr)
		// Fall back to the identities this process is running or has health
		// for — IDENTITY keys, never the health report's display-formatted
		// "proxy[N] (addr)" strings, which match nothing in the pruned stores.
		// The union is deliberately wider than the provider's live-bandwidth
		// set: an empty keep-set on a transient read error would drop every
		// identity's backoff and retry a given-up proxy forever.
		keepAddrs = make(map[string]bool)
		for k := range proxyEarnBandwidthSnapshot() {
			keepAddrs[k] = true
		}
		for _, k := range proxyLiveHealthKeys() {
			keepAddrs[k] = true
		}
	}
	globalProxyFailureHistory.Prune(keepAddrs)
	globalProvenProxies.Prune(keepAddrs)
}
