package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Adaptation notes:
// - Ported from main.go lines 1312-1931 to package provider.
// - connect.ProxyHealthSnapshot() -> ProxyHealthSnapshot()
// - connect.ProxyHealthCount() -> ProxyHealthCount()
// - connect.ContractMetricsSnapshot() -> contractMetricsSnapshot()
// - lifetimeStore, globalPerProxyEarnTracker, globalProxyEarningsStore,
//   globalContractMetrics are already declared in their own files.

type trafficBytes struct {
	rx, tx uint64
}

// fmtRate formats a bytes-per-second rate as a human-readable string.
func fmtRate(bytesPerSec float64) string {
	switch {
	case bytesPerSec >= 1e9:
		return fmt.Sprintf("%.1f GB/s", bytesPerSec/1e9)
	case bytesPerSec >= 1e6:
		return fmt.Sprintf("%.1f MB/s", bytesPerSec/1e6)
	case bytesPerSec >= 1e3:
		return fmt.Sprintf("%.1f KB/s", bytesPerSec/1e3)
	default:
		return fmt.Sprintf("%.0f B/s", bytesPerSec)
	}
}

// fmtBytes formats a byte count as a human-readable string.
func fmtBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// nextMidnight returns the next local midnight after t.
func nextMidnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, t.Location())
}

// uDelta returns cur-prev guarding against counter resets (a reading below
// the previous sample means the source restarted; contribute 0, never wrap).
func uDelta(cur uint64, prev *uint64) uint64 {
	var d uint64
	if cur >= *prev {
		d = cur - *prev
	}
	*prev = cur
	return d
}

// u64At returns a pointer to a stable uint64 slot for key, creating the
// entry if absent. Map values are not addressable in Go, so the slot is
// held via *uint64 indirection.
func u64At(m map[string]*uint64, key string) *uint64 {
	if p, ok := m[key]; ok {
		return p
	}
	v := uint64(0)
	m[key] = &v
	return &v
}

// iDelta is uDelta's counterpart for signed counters (contract totals are
// int64 atomics): negative or decreasing readings contribute zero.
func iDelta(cur int64, prev *int64) uint64 {
	var d int64
	if cur >= *prev {
		d = cur - *prev
	}
	*prev = cur
	if d < 0 {
		return 0
	}
	return uint64(d)
}

// runLifetimeCollector samples the existing exported counters once per earn
// tick, converts them to reset-guarded deltas, and feeds the persistent
// store. Read-only over the network stack: nothing here can affect the hot
// path.
func runLifetimeCollector(ctx context.Context) {
	var prevPQE, prevClas uint64
	var prevUp, prevDeny int64
	prevBillable := map[string]uint64{}
	prevRxPerProxy := map[string]*uint64{}
	prevTxPerProxy := map[string]*uint64{}
	var prevTime time.Time
	var prevA1, prevA2, prevA3, prevA4, prevA5, prevA6, prevA7 uint64

	// departedBaselines remembers the last billable counter reading of
	// proxies that left the health snapshot, so a proxy returning within
	// tombstoneProxyBaselineTTL resumes against its old baseline instead
	// of re-counting its whole history as fresh delta.
	type departedBaseline struct {
		baseline uint64
		left     time.Time
	}
	departedBaselines := map[string]departedBaseline{}
	const tombstoneProxyBaselineTTL = 24 * time.Hour

	ticker := time.NewTicker(earnCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			lifetimeStore.Flush()
			return
		case <-ticker.C:
		}

		pq := pqeTotalCounts()
		up, deny := globalContractMetrics.totals()

		// Billable + transit volumes from the read-only health snapshot.
		var billableSum, rxSum, txSum, clients uint64
		serving := 0
		now := time.Now()
		_, _, _, bw, _ := ProxyHealthSnapshot()
		for key, b := range bw {
			billable := b.BillableRx.Load() + b.BillableTx.Load()
			pb, seen := prevBillable[key]
			if !seen {
				if dep, was := departedBaselines[key]; was && now.Sub(dep.left) < tombstoneProxyBaselineTTL {
					pb, seen = dep.baseline, true
				}
			}
			delete(departedBaselines, key)
			if seen && billable >= pb {
				billableSum += billable - pb
			}
			prevBillable[key] = billable
			rxSum += b.TotalRx.Load()
			txSum += b.TotalTx.Load()
			if c := b.Clients.Load(); c > 0 {
				clients += uint64(c)
				serving++
			}
		}
		// Move baselines of departed proxies into the tombstone map.
		for key := range prevBillable {
			if _, live := bw[key]; !live {
				departedBaselines[key] = departedBaseline{baseline: prevBillable[key], left: now}
				delete(prevBillable, key)
			}
		}
		// Expire stale tombstones so the map stays bounded under churn.
		for key, dep := range departedBaselines {
			if now.Sub(dep.left) >= tombstoneProxyBaselineTTL {
				delete(departedBaselines, key)
			}
		}

		lifetimeStore.Add(
			uDelta(uint64(pq.PQELifetime), &prevPQE),
			uDelta(uint64(pq.ClasLifetime), &prevClas),
			iDelta(up, &prevUp),
			iDelta(deny, &prevDeny),
			0, 0, // recovered/lost are fed by the health-heartbeat owner
			billableSum,
		)
		lifetimeStore.MaybeFlush(time.Now())

		// All-time rollup line (only once something changed since last tick).
		a1, a2, a3, a4, a5, a6, a7 := lifetimeStore.Snapshot()
		if a1 != prevA1 || a2 != prevA2 || a3 != prevA3 || a4 != prevA4 || a5 != prevA5 || a6 != prevA6 || a7 != prevA7 {
			if a1|a2|a3|a4|a5|a6|a7 != 0 {
				tlog("♾️ [lifetime] all-time: post-quantum=%d classical=%d contracts_acquired=%d denied=%d proxies_recovered=%d lost=%d billable_total=%s\n",
					a1, a2, a3, a4, a5, a6, fmtBytes(a7))
			}
			prevA1, prevA2, prevA3, prevA4, prevA5, prevA6, prevA7 = a1, a2, a3, a4, a5, a6, a7
		}

		// Transit visibility: traffic forwarded THROUGH this node to other
		// peers (as opposed to 🔐 [pqe] which counts tunnels terminating HERE).
		if clients > 0 && !prevTime.IsZero() {
			elapsed := now.Sub(prevTime).Seconds()
			if elapsed < 1 {
				elapsed = 1
			}
			var rxDelta, txDelta uint64
			for key, b := range bw {
				rx := b.TotalRx.Load()
				tx := b.TotalTx.Load()
				rxDelta += uDelta(rx, u64At(prevRxPerProxy, key))
				txDelta += uDelta(tx, u64At(prevTxPerProxy, key))
			}
			for key := range prevRxPerProxy {
				if _, live := bw[key]; !live {
					delete(prevRxPerProxy, key)
					delete(prevTxPerProxy, key)
				}
			}
			tlog("🛰️ [relay] as-hop: %d clients via %d proxies — %s in, %s out\n",
				clients, serving,
				fmtRate(float64(rxDelta)/elapsed),
				fmtRate(float64(txDelta)/elapsed))
		} else {
			for key, b := range bw {
				*u64At(prevRxPerProxy, key) = b.TotalRx.Load()
				*u64At(prevTxPerProxy, key) = b.TotalTx.Load()
			}
		}
		prevTime = now
	}
}

func runEarningWindows(ctx context.Context) {
	const maxSamples = 60
	deltas := make([]uint64, 0, maxSamples)
	var prevCum uint64
	var prevSet bool
	var prevEarnActive bool

	ticker := time.NewTicker(earnCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		c := pqeTotalCounts()
		if c.Measured && (c.ActivePQE != 0 || c.ActiveClas != 0 || c.PQEHour != 0 || c.PQEDay != 0 ||
			c.PQEWeek != 0 || c.PQELifetime != 0 || c.ClasHour != 0 || c.ClasDay != 0 ||
			c.ClasWeek != 0 || c.ClasLifetime != 0) {
			tlog("🔐 [pqe] direct-e2e tunnels terminated: live pqe=%d classical=%d | since-start: pqe=%d classical=%d | 1h: pqe=%d classical=%d | 24h: pqe=%d classical=%d | 7d: pqe=%d classical=%d\n",
				c.ActivePQE, c.ActiveClas,
				c.PQELifetime, c.ClasLifetime,
				c.PQEHour, c.ClasHour, c.PQEDay, c.ClasDay, c.PQEWeek, c.ClasWeek)
			allPQE, allClas, _, _, _, _, _ := lifetimeStore.Snapshot()
			tlog("🔐 [pqe] all-time: pqe=%d classical=%d\n", allPQE, allClas)
		}

		if ProxyHealthCount() == 0 {
			prevSet = false
			globalPerProxyEarnTracker.Update(nil)
			continue
		}

		_, _, _, bw, _ := ProxyHealthSnapshot()

		globalPerProxyEarnTracker.Update(bw)

		// The earnings store is identity-keyed: feed it the identity-keyed
		// bandwidth snapshot so two accounts sharing one gateway address
		// never collide (or re-migrate) in the store. The per-proxy earn
		// tracker stays on the display snapshot (address-normalized) — its
		// consumers look up by key and normalize on ingest.
		globalProxyEarningsStore.Observe(ProxyBandwidthSnapshotByKey(), time.Now())
		globalProxyEarningsStore.MaybeSave(time.Now())

		var cum uint64
		for _, p := range bw {
			cum += p.BillableRx.Load() + p.BillableTx.Load()
		}

		if prevSet {
			if cum >= prevCum {
				deltas = append(deltas, cum-prevCum)
			} else {
				deltas = append(deltas, 0)
			}
			if len(deltas) > maxSamples {
				deltas = deltas[len(deltas)-maxSamples:]
			}

			billable1m := sumLastN(deltas, 1)
			billable5m := sumLastN(deltas, 5)
			billable15m := sumLastN(deltas, 15)
			billable60m := sumLastN(deltas, 60)

			active := "no"
			if billable1m > 0 {
				active = "yes"
			}

			nowActive := billable1m > 0
			if nowActive != prevEarnActive || nowActive {
				tlog("💰 [earn] billable_1m=%s billable_5m=%s billable_15m=%s billable_60m=%s active=%s\n",
					fmtBytes(billable1m), fmtBytes(billable5m), fmtBytes(billable15m), fmtBytes(billable60m), active)
				prevEarnActive = nowActive
			}
		}
		prevCum = cum
		prevSet = true
	}
}

// sumLastN sums the last n entries from a slice of uint64. If fewer than n
// entries exist, sums all available (partial window).
func sumLastN(deltas []uint64, n int) uint64 {
	if len(deltas) < n {
		n = len(deltas)
	}
	var total uint64
	for _, d := range deltas[len(deltas)-n:] {
		total += d
	}
	return total
}

// earningReason returns a short, greppable token for the [profit] line's
// reason= field explaining why billable traffic is or isn't moving, so an
// operator can distinguish "no demand" from "no proxies" from "still warming
// up" without cross-referencing other lines. Returns "-" while earning. The
// checks are ordered most-fundamental first: a healthy earning provider needs
// proxies up, clients matched to them, and bytes actually moving.
func earningReason(earning bool, proxiesUp int, clients int64, warmup bool) string {
	switch {
	case earning:
		return "-"
	case warmup:
		return "warmup"
	case proxiesUp == 0:
		return "no_proxies"
	case clients == 0:
		return "idle"
	default:
		return "no_traffic"
	}
}

// profitIdleLogInterval caps how often a non-earning [profit] line is
// printed, so quiet periods (warmup, no assigned clients) don't flood the
// log at the 15s tick rate.
const profitIdleLogInterval = 5 * time.Minute

// runProfitHeartbeat logs a [profit] line focused on whether billable traffic
// is moving right now, distinct from runEarningWindows' longer rolling
// trend. It ticks every 15s but only prints every tick while earning=yes;
// once earning drops to no it prints immediately (so the exact stop time is
// visible) and then throttles to profitIdleLogInterval until traffic resumes.
func runProfitHeartbeat(ctx context.Context) {
	const interval = 15 * time.Second

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var prevBillable uint64
	var prevSet bool
	prevTickTime := time.Now()
	var lastLogTime time.Time
	wasEarning := false
	var prevClients int64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if ProxyHealthCount() == 0 {
			prevSet = false
			continue
		}

		proxiesUp, _, _, bw, connecting := ProxyHealthSnapshot()

		var billable uint64
		var clients int64
		var serving int
		for _, p := range bw {
			billable += p.BillableRx.Load() + p.BillableTx.Load()
			pc := p.Clients.Load()
			clients += pc
			if pc > 0 {
				serving++
			}
		}

		now := time.Now()
		if !prevSet {
			prevBillable = billable
			prevTickTime = now
			prevSet = true
			continue
		}

		elapsed := now.Sub(prevTickTime).Seconds()
		if elapsed < 1 {
			elapsed = 1
		}
		var delta uint64
		if billable >= prevBillable {
			delta = billable - prevBillable
		}
		prevBillable = billable
		prevTickTime = now

		earning := delta > 0 && clients > 0
		justStopped := wasEarning && !earning

		// Traffic start/stop markers (✈️/🛬 on clients transition)
		if prevClients == 0 && clients > 0 {
			tlog("✈️ [traffic] started (clients=%d)\n", clients)
		} else if prevClients > 0 && clients == 0 {
			tlog("🛬 [traffic] stopped (was=%d clients)\n", prevClients)
		}
		prevClients = clients

		wasEarning = earning

		if earning || justStopped || lastLogTime.IsZero() || now.Sub(lastLogTime) >= profitIdleLogInterval {
			status := "no"
			if earning {
				status = "yes"
			}
			idle := proxiesUp - serving
			if idle < 0 {
				idle = 0
			}
			warmup := len(connecting) >= 5
			reason := earningReason(earning, proxiesUp, clients, warmup)
			profitEmoji := ""
			if status == "yes" {
				profitEmoji = "💰 "
			}
			acquired, denied, utilSum := contractMetricsSnapshot()
			contractFields := ""
			if acquired+denied > 0 {
				contractFields = fmt.Sprintf(" contracts=%d denied=%d", acquired, denied)
				// v2026 connect removed contract byte-utilization
				// instrumentation. utilSum stays 0 — report as n/a
				// rather than misleading "avg_util=0%".
				if utilSum > 0 {
					avgUtil := utilSum / acquired
					contractFields += fmt.Sprintf(" avg_util=%d%%", avgUtil)
				} else {
					contractFields += " avg_util=n/a"
				}
			}
			tlog("%s[profit] earning=%s reason=%s clients=%d rate=%s proxies_up=%d serving=%d idle=%d%s\n",
				profitEmoji, status, reason, clients, fmtRate(float64(delta)/elapsed), proxiesUp, serving, idle, contractFields)
			lastLogTime = now
		}
	}
}

// runBillableRateWriter writes the aggregate billable traffic rate (bytes/sec)
// to ~/.urnetwork/billable_rate every 10 seconds. Used by urnet-tools
// idle-update to detect traffic lulls before applying updates.
func runBillableRateWriter(ctx context.Context) {
	const interval = 10 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	prevBillable := make(map[string]uint64)
	prevTickTime := time.Now()

	tlog("[billable_rate] writer started (interval=%s)\n", interval)

	for {
		select {
		case <-ctx.Done():
			tlog("[billable_rate] writer stopped (shutdown)\n")
			return
		case <-ticker.C:
		}

		if ProxyHealthCount() == 0 {
			for k := range prevBillable {
				delete(prevBillable, k)
			}
			writeRate(0)
			continue
		}

		_, _, _, bw, _ := ProxyHealthSnapshot()
		if len(bw) == 0 {
			for k := range prevBillable {
				delete(prevBillable, k)
			}
			writeRate(0)
			continue
		}

		var totalDelta uint64
		for key, p := range bw {
			cur := p.BillableRx.Load() + p.BillableTx.Load()
			if prev, ok := prevBillable[key]; ok {
				if cur >= prev {
					totalDelta += cur - prev
				}
			}
			prevBillable[key] = cur
		}
		for k := range prevBillable {
			if _, ok := bw[k]; !ok {
				delete(prevBillable, k)
			}
		}

		now := time.Now()
		elapsed := now.Sub(prevTickTime).Seconds()
		if elapsed < 1 {
			elapsed = 1
		}
		prevTickTime = now

		rate := uint64(float64(totalDelta) / elapsed)
		writeRate(rate)
	}
}

func writeRate(rate uint64) {
	dir, ok := proxyHealthDir()
	if !ok {
		return
	}
	path := filepath.Join(dir, "billable_rate")
	tmp := path + ".tmp"
	content := strconv.FormatUint(rate, 10) + "\n"
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		tlog("[billable_rate] warn: failed to write billable rate file (~/.urnetwork/health/billable_rate): %v\n", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		tlog("[billable_rate] warn: failed to finalize billable rate file (rename tmp -> billable_rate): %v\n", err)
	}
}
