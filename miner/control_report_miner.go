package miner

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// control_report_miner.go fills the control socket's data replies with the
// miner's real state (P2b):
//
//   - snapshot: uptime and version from this run, the proxy manager's desired
//     set with live health overlaid, clients and byte counters from the
//     bandwidth registry, per-slot connected/ready counts from the progress
//     owner, restart reason and resources from the process.
//   - traffic: the same byte counters, read live.
//   - internals and goroutines: the Go runtime views (control_internals.go).
//   - trim_preview: the running provider's own shed list.
//
// Anything the miner cannot supply is named in the snapshot's miner_notes
// field (an additive protocol field clients ignore) instead of being
// invented, and every builder returns an error rather than a zeroed reply
// when it has no source at all.

func init() {
	minerControlSnapshot = buildMinerSnapshot
	minerControlTraffic = buildMinerTraffic
	minerControlInternals = buildMinerInternals
	minerControlGoroutines = buildMinerGoroutines
	minerControlTrimPreview = buildMinerTrimPreview
	minerControlAuditStatus = buildMinerAuditStatus
}

// snapshotStateThresholds mirror the 3.23-fix provider's derivation.
const (
	snapshotStartingWindow = 120 * time.Second
	snapshotIdleBps        = 5120 // 5 KiB/s, the same idle default urnet-tools update --idle uses
)

// buildMinerSnapshot assembles the node snapshot from the miner's state.
func buildMinerSnapshot() (*nodeSnapshot, error) {
	now := time.Now()
	startedAt := controlRuntime.startedAtSnapshot()
	if startedAt.IsZero() {
		// A socket without a wired run (an embedded use or a test): report the
		// state honestly rather than fail the command.
		startedAt = now
	}
	uptime := now.Sub(startedAt)
	if uptime < 0 {
		uptime = 0
	}

	marker, hadPriorRun := controlRuntime.markerSnapshot()
	restart := snapshotRestart{Reason: "first-start"}
	if hadPriorRun {
		switch marker.Reason {
		case "clean":
			restart = snapshotRestart{Reason: "clean", CleanShutdown: true}
		case "hotswap":
			restart = snapshotRestart{Reason: "hotswap", CleanShutdown: true}
		case "update":
			restart = snapshotRestart{Reason: "update", CleanShutdown: true}
		default:
			restart = snapshotRestart{Reason: "unclean"}
		}
	}

	proxies, proxyNote := snapshotProxyCounts()
	billable, total, clients, haveCounters := controlRuntime.counterTotals()
	nowRate, avg1m, avg5m, seq, history := controlRuntime.rate.rates()
	totalNow, totalAvg1m, totalAvg5m, totalHistory := controlRuntime.rate.totalHistory()

	snap := &nodeSnapshot{
		V:               1,
		Version:         minerControlVersion(),
		PreviousVersion: marker.Version,
		StartedAt:       startedAt.UTC(),
		Now:             now.UTC(),
		UptimeSeconds:   uptime.Seconds(),
		Busy:            avg1m >= snapshotIdleBps,
		Rate: snapshotRate{
			NowBps:                 nowRate,
			Avg1mBps:               avg1m,
			Avg5mBps:               avg5m,
			HistoryIntervalSeconds: 1,
			HistorySeq:             seq,
			HistoryBps:             history,
		},
		Clients:   clients,
		Proxies:   proxies,
		Restart:   restart,
		Resources: collectMinerResources(),
	}
	if haveCounters {
		snap.Traffic = &snapshotTraffic{
			BillableBytes:   billable,
			TotalBytes:      total,
			TotalNowBps:     totalNow,
			TotalAvg1mBps:   totalAvg1m,
			TotalAvg5mBps:   totalAvg5m,
			TotalHistoryBps: totalHistory,
		}
	}
	if marker.Version == minerControlVersion() {
		snap.PreviousVersion = ""
	}

	// Per-slot sessions: the progress owner knows how many slots are connected
	// and ready. The SDK does not expose the PQE/classical session split, so
	// sessions stays zero and the note says so.
	slotTotal, slotConnected, slotReady, haveSlots := controlRuntime.slotSummary(context.Background())
	notes := []string{}
	if haveSlots {
		notes = append(notes, fmt.Sprintf("slots: %d total, %d connected, %d ready", slotTotal, slotConnected, slotReady))
	}
	if !haveCounters {
		notes = append(notes, "traffic counters are not available yet (the provide loop has not started)")
	} else {
		notes = append(notes, "traffic counters cover TCP through the connect engine; billable bytes come from the relay observers")
	}
	notes = append(notes, "sessions.pqe/classical are not exposed by the miner SDK (reported as 0)")
	if proxyNote != "" {
		notes = append(notes, proxyNote)
	}
	snap.MinerNotes = notes

	// State derivation, in the same order as the 3.23-fix provider: a failed
	// URL source is degraded at once, a warmup window is starting, an
	// unhealthy pool or pressure is degraded, then idle or flowing.
	switch {
	case proxyResolutionStatus.Load() == proxyResolutionFailed:
		snap.State = "degraded"
		snap.StateReason = "proxy URL source failed: " + getProxyResolutionReason()
	case uptime < snapshotStartingWindow:
		snap.State = "starting"
	case proxies.total() > 0 && 2*(proxies.Dead+proxies.Degraded) > proxies.total():
		snap.State = "degraded"
		snap.StateReason = fmt.Sprintf("%d of %d proxies down", proxies.Dead+proxies.Degraded, proxies.total())
	case avg1m < snapshotIdleBps:
		snap.State = "idle"
		snap.IdleHint = idleHintText(proxies)
	default:
		snap.State = "flowing"
	}

	snap.RestartPending = restartPendingFromControlState()
	return snap, nil
}

// snapshotProxyCounts maps the proxy manager's desired set plus the live
// health registry onto the snapshot's counters.
func snapshotProxyCounts() (snapshotProxies, string) {
	var counts snapshotProxies
	state, err := readProxyState()
	if err != nil {
		return counts, fmt.Sprintf("proxy state is unreadable: %v", err)
	}
	live := liveProxyHealthByKey()
	for key, entry := range state.Proxies {
		health := entry.Health
		if liveStatus, ok := live[key]; ok && liveStatus.Health != "" {
			health = liveStatus.Health
		}
		switch health {
		case "up":
			counts.Up++
		case "dead", "inactive":
			counts.Dead++
		case "recently_offline", "offline", "long_offline", "parked":
			counts.Degraded++
		default:
			counts.Connecting++
		}
	}
	return counts, ""
}

// idleHintText is the plain-language reason the node is idle.
func idleHintText(proxies snapshotProxies) string {
	total := proxies.total()
	switch {
	case total == 0:
		return "no proxies configured"
	case proxies.Up == 0:
		return fmt.Sprintf("all %d proxies down or connecting", total)
	case 2*proxies.Up < total:
		return fmt.Sprintf("only %d of %d proxies connected", proxies.Up, total)
	}
	return fmt.Sprintf("no traffic offered (%d/%d proxies up)", proxies.Up, total)
}

// restartPendingFromControlState reports whether a restart-required key now
// differs from the value the process started with.
func restartPendingFromControlState() bool {
	startup := startupValues()
	for key, started := range startup {
		current, ok := globalControlState.get(key)
		if !ok {
			continue
		}
		if key == "ramlogs" {
			if controlTruthy(current) != controlTruthy(started) {
				return true
			}
			continue
		}
		if current != started {
			return true
		}
	}
	return false
}

// controlTruthy accepts the boolean spellings the client uses.
func controlTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "on", "true", "yes":
		return true
	}
	return false
}

// collectMinerResources reads the process's own resource picture.
func collectMinerResources() snapshotResources {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	resources := snapshotResources{
		HeapInuseBytes: mem.HeapInuse,
		Goroutines:     runtime.NumGoroutine(),
	}
	if open, limit, ok := controlReportOpenFDs(); ok {
		openCopy, limitCopy := open, limit
		resources.OpenFDs = &openCopy
		resources.FDLimit = &limitCopy
	}
	return resources
}

// buildMinerTraffic is the light live-counter reply:
// the bandwidth registry's cumulative sums, read now.
func buildMinerTraffic() (*liveTraffic, error) {
	billable, total, _, ok := controlRuntime.counterTotals()
	if !ok {
		return nil, fmt.Errorf("traffic counters are not available yet (the provide loop has not started)")
	}
	return &liveTraffic{
		AtUnixNano:    time.Now().UnixNano(),
		BillableBytes: billable,
		TotalBytes:    total,
	}, nil
}

// buildMinerTrimPreview computes what a trim to count would shed, from this
// process's running set (the manager's desired set minus what is held), the
// persisted state and the URL cache. This is the reply the CLI's
// `proxy trim --preview` receives over the socket.
func buildMinerTrimPreview(count int) (string, error) {
	state, err := readProxyState()
	if err != nil {
		return "", fmt.Errorf("could not read provider state: %v", err)
	}
	urlState, _ := readProxyURLState()
	running := controlRuntime.runningKeySnapshot()
	if len(running) == 0 {
		// Before the provide loop publishes its slots (or in a state dir whose
		// slots were never launched), fall back to the live health keys.
		running = proxyLiveHealthKeys()
	}
	// The shed ranking protects what a proxy has EARNED: this run's billable
	// bytes per identity (the counters the provide loop publishes) with the
	// decayed earnings score behind them, so an idle proxy is shed before a
	// revenue stream (v33 runningProxyEarnings).
	return trimPreviewText(state, urlState, running, runningProxyEarnings(), count), nil
}

// buildMinerAuditStatus reports the proxy audit engine, which the miner does
// not implement. An explicit error beats a fabricated "observing only"
// status: the client falls back to the status command, whose audit field is
// nil in the same way an older provider's is.
func buildMinerAuditStatus() (*proxyAuditStatus, error) {
	return nil, fmt.Errorf("the miner does not implement the v3.23 proxy audit engine (no tick to report)")
}
