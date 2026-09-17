package provider

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/metrics"
	"time"
)

// Adaptation notes:
// - Ported from main.go lines 1936-2294 to package provider.
// - connect.ProxyHealthCount() -> ProxyHealthCount()
// - connect.ProxyHealthSnapshot() -> ProxyHealthSnapshot()
// - connect.ProxyHealthHeartbeat() -> ProxyHealthHeartbeat()
// - connect.ProxyHealthByAddress() -> ProxyHealthByAddress()
// - connect.GetDohFailureCount() -> getDohFailureCountStub()
// - connect.ActiveConnectionCount() -> activeConnectionCount()
// - connect.ActiveProxyConnections() -> activeProxyConnections()
// - connect.MessagePoolSummary() -> messagePoolSummary() (always nil in v2026)
// - pool_health.go, proxy_health.go, proxy_health_log.go provide
//   newPoolHealthWindow, poolHealthLine, ProxyHealthReport, proxyHealthDir,
//   capProxyList, proxyHealthListCap, writeProxyHealthState, etc.

func runHealthHeartbeat(ctx context.Context, startTime time.Time, profile string) {
	interval := 5 * time.Minute
	if s := os.Getenv("URNETWORK_HEALTH_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d >= time.Minute {
			interval = d
		}
	}
	if profile == "" {
		profile = "default"
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// poolHealth carries the idle-floor history across ticks so the pool line
	// can report a trend rather than an instant. Currently unused because
	// messagePoolSummary() always returns nil in v2026; retained for future
	// re-instrumentation.
	_ = newPoolHealthWindow(interval) // poolHealth placeholder

	// deadConfirmDelay gates confirmed-dead event logging until one pulse cycle has
	// elapsed, so the startup ramp is not recorded as dead.
	const deadConfirmDelay = 65 * time.Minute

	// per-proxy byte counts from the previous tick, used to compute rates.
	prevTick := map[string]trafficBytes{}
	prevTickTime := time.Now()

	// per-proxy billable byte checkpoint at midnight, to show "today" totals.
	midnightCheckpoint := map[string]uint64{}
	nextMidnightReset := nextMidnight(time.Now())

	// velocity and peak tracking
	var prevTotalRx, prevTotalTx uint64
	var peakRx, peakTx uint64
	var peakRxElapsed, peakTxElapsed float64 = 1, 1
	velocityLogged := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		samples := []metrics.Sample{
			{Name: "/memory/classes/heap/objects:bytes"},
			{Name: "/memory/classes/total:bytes"},
		}
		metrics.Read(samples)
		heapMiB := metricBytesToMiB("/memory/classes/heap/objects:bytes", samples[0].Value)
		sysMiB := metricBytesToMiB("/memory/classes/total:bytes", samples[1].Value)
		uptime := time.Since(startTime).Truncate(time.Second)
		dohFailures := getDohFailureCountStub()

		// Build identity leads the block: every other line in a tick is only
		// interpretable once you know which build produced it, and an operator
		// reading a tail or a pasted log excerpt has no other way to tell.
		buildVersion := RequireVersion()
		if buildVersion == "" {
			buildVersion = "unknown"
		}
		buildLine := fmt.Sprintf("🏷️ [health][build] %s profile=%s", buildVersion, profile)
		if host := heartbeatHostLabel(); host != "" {
			buildLine += fmt.Sprintf(" host=%s", host)
		}
		tlog("%s\n", buildLine)

		healthLine := fmt.Sprintf("❤️ [health] uptime=%s profile=%s heap=%dMiB sys=%dMiB goroutines=%d connections=%d proxies=%d",
			uptime, profile, heapMiB, sysMiB, runtime.NumGoroutine(), activeConnectionCount(), activeProxyConnections())
		if dohFailures > 0 {
			healthLine += fmt.Sprintf(" dns_failures=%d", dohFailures)
		}
		tlog("%s\n", healthLine)

		// Message-pool heartbeat: v2026 connect removed pool instrumentation
		// entirely (now handled by memory_budget package). messagePoolSummary()
		// always returns nil; the [health][pool] line is permanently skipped.
		// See stubs_batch_m.go for the decision document.

		if ProxyHealthCount() == 0 {
			continue // non-proxy mode: no [health][proxies] lines
		}

		now := time.Now()
		report := ProxyHealthHeartbeat(uptime >= deadConfirmDelay)
		down := len(report.Dead) + len(report.Degraded)
		tlog("❤️ [health][proxies] up=%d down=%d dead=%d degraded=%d recovered=%d lost=%d lifetime_recovered=%d lifetime_lost=%d\n",
			report.Up, down, len(report.Dead), len(report.Degraded),
			len(report.Recovered), len(report.NewlyDegraded),
			report.LifetimeRecovered, report.LifetimeLost)
		// Feed the persistent all-time store: this loop is the exclusive
		// consumer of the heartbeat report, so transition deltas are
		// captured exactly once, here. Lost = up->down EVENTS only:
		// NewlyDegraded (was up, went down) plus NewlyDead (never-up,
		// newly confirmed dead). report.Dead is the COMPLETE currently-
		// dead list rebuilt every tick — counting it here would inflate
		// the persisted counter on every tick a proxy stays dead.
		lifetimeStore.Add(0, 0, 0, 0,
			uint64(len(report.Recovered)),
			uint64(len(report.NewlyDegraded))+uint64(len(report.NewlyDead)), 0)
		if len(report.Dead) > 0 {
			tlog("[health][proxies] unreachable (dead): %s\n", capProxyList(report.Dead, proxyHealthListCap))
		}
		if len(report.Degraded) > 0 {
			tlog("[health][proxies] failing (degraded): %s\n", capProxyList(report.Degraded, proxyHealthListCap))
		}

		// Reset midnight checkpoints when the day rolls over.
		if now.After(nextMidnightReset) {
			for k, bw := range report.Bandwidth {
				midnightCheckpoint[k] = bw.BillableRx.Load() + bw.BillableTx.Load()
			}
			nextMidnightReset = nextMidnight(now)
		}

		// Compute per-tick rates and emit [traffic] lines.
		elapsed := now.Sub(prevTickTime).Seconds()
		if elapsed < 1 {
			elapsed = 1
		}
		var totalRxDelta, totalTxDelta, totalBillable uint64
		var totalClients int64
		activeProxies := 0
		serving := 0
		for key, bw := range report.Bandwidth {
			rx := bw.TotalRx.Load()
			tx := bw.TotalTx.Load()
			clients := bw.Clients.Load()
			totalClients += clients
			if clients > 0 {
				serving++
			}
			prev := prevTick[key]
			var rxDelta, txDelta uint64
			if rx >= prev.rx {
				rxDelta = rx - prev.rx
			}
			if tx >= prev.tx {
				txDelta = tx - prev.tx
			}
			totalRxDelta += rxDelta
			totalTxDelta += txDelta
			prevTick[key] = trafficBytes{rx: rx, tx: tx}

			if rxDelta == 0 && txDelta == 0 {
				continue
			}
			activeProxies++
			billableTotal := bw.BillableRx.Load() + bw.BillableTx.Load()
			cp := midnightCheckpoint[key]
			if billableTotal < cp {
				cp = billableTotal
				midnightCheckpoint[key] = billableTotal
			}
			billableToday := billableTotal - cp
			totalBillable += billableToday

			if clients == 0 {
				continue
			}
			ageStr := ""
			if age := bw.MaxAge(); age > 0 {
				ageStr = fmt.Sprintf(" age=%s", age.Round(time.Second))
			}
			tlog("📈 [traffic] %s rx=%s tx=%s clients=%d%s billable_today=%s\n",
				key,
				fmtRate(float64(rxDelta)/elapsed),
				fmtRate(float64(txDelta)/elapsed),
				clients,
				ageStr,
				fmtBytes(billableToday),
			)
		}
		prevTickTime = now

		// velocity alerts: detect dramatic rate changes
		totalRx := totalRxDelta
		totalTx := totalTxDelta
		if prevTotalRx > 0 || prevTotalTx > 0 {
			prevTotal := prevTotalRx + prevTotalTx
			currTotal := totalRx + totalTx
			if currTotal > 0 && prevTotal > 0 {
				if currTotal > prevTotal*3 && (currTotal-prevTotal) > 100*1024 {
					if time.Since(velocityLogged) > 5*time.Minute {
						tlog("📈 [traffic] velocity: %.1fx → rx=%s tx=%s (was rx=%s tx=%s)\n",
							float64(currTotal)/float64(prevTotal),
							fmtRate(float64(totalRx)/elapsed),
							fmtRate(float64(totalTx)/elapsed),
							fmtRate(float64(prevTotalRx)/elapsed),
							fmtRate(float64(prevTotalTx)/elapsed))
						velocityLogged = now
					}
				} else if prevTotal > currTotal*3 && (prevTotal-currTotal) > 100*1024 {
					if time.Since(velocityLogged) > 5*time.Minute {
						tlog("📈 [traffic] velocity: %.1fx → rx=%s tx=%s (was rx=%s tx=%s) — traffic dropping\n",
							float64(currTotal)/float64(prevTotal),
							fmtRate(float64(totalRx)/elapsed),
							fmtRate(float64(totalTx)/elapsed),
							fmtRate(float64(prevTotalRx)/elapsed),
							fmtRate(float64(prevTotalTx)/elapsed))
						velocityLogged = now
					}
				}
			}
		}

		// Prune per-proxy bookkeeping for addresses that left the health
		// snapshot.
		live := make(map[string]struct{}, len(report.Bandwidth))
		for key := range report.Bandwidth {
			live[key] = struct{}{}
		}
		for key := range prevTick {
			if _, ok := live[key]; !ok {
				delete(prevTick, key)
				delete(midnightCheckpoint, key)
			}
		}

		// peak tracking: update high water marks and freeze elapsed at the time of the peak
		if totalRx > peakRx {
			peakRx = totalRx
			peakRxElapsed = elapsed
		}
		if totalTx > peakTx {
			peakTx = totalTx
			peakTxElapsed = elapsed
		}

		prevTotalRx = totalRx
		prevTotalTx = totalTx

		earning := "no"
		if totalBillable > 0 {
			earning = "yes"
		}
		tlog("📈 [traffic] total rx=%s tx=%s clients=%d active_proxies=%d billable_today=%s earning=%s peak_rx=%s peak_tx=%s\n",
			fmtRate(float64(totalRxDelta)/elapsed),
			fmtRate(float64(totalTxDelta)/elapsed),
			totalClients,
			activeProxies,
			fmtBytes(totalBillable),
			earning,
			fmtRate(float64(peakRx)/peakRxElapsed),
			fmtRate(float64(peakTx)/peakTxElapsed),
		)
		idle := report.Up - serving
		if idle < 0 {
			idle = 0
		}
		tlog("[earn] proxies_up=%d serving=%d idle=%d clients=%d\n",
			report.Up, serving, idle, totalClients)

		keepAddrs, pruneErr := desiredAddressesForHistoryPruning()
		if pruneErr != nil {
			tlog("[proxy] warning: could not determine desired proxy addresses for history pruning: %v\n", pruneErr)
			keepAddrs = make(map[string]bool, len(report.Bandwidth))
			for k := range report.Bandwidth {
				keepAddrs[k] = true
			}
		}
		globalProxyFailureHistory.Prune(keepAddrs)
		globalProvenProxies.Prune(keepAddrs)

		// Update proxy.state health snapshot for use by proxy refresh subcommand.
		go func() {
			lockRelease, lockErr := acquireProxyLockWithRetry()
			if lockErr != nil {
				tlog("[proxy] warn: health snapshot could not acquire proxy lock, skipping this tick: %v\n", lockErr)
				return
			}
			defer lockRelease()

			proxyStateMu.Lock()
			defer proxyStateMu.Unlock()
			state, err := readProxyState()
			if err != nil {
				return
			}
			if state.StartedAt.IsZero() {
				state.StartedAt = startTime
			}
			liveHealth := ProxyHealthByAddress()
			for addr, entry := range state.Proxies {
				if h, ok := liveHealth[addr]; ok {
					entry.Health = h.Health
					if h.DownSince.IsZero() {
						entry.DownSince = ""
					} else {
						entry.DownSince = h.DownSince.Format(time.RFC3339)
					}
					entry.AuthFailures = h.AuthFailures
					state.Proxies[addr] = entry
				}
			}
			if err := writeProxyState(state); err != nil {
				tlog("[proxy] warn: state write failed: %v\n", err)
			}
		}()

		if dir, ok := proxyHealthDir(); ok {
			writeProxyHealthState(dir, report, now)
			writeProxyHealthEvents(dir, report, now)
			writeProxyTrafficState(dir, report, now)
			writeUsageHistory(dir, report, now)
		}
	}
}
