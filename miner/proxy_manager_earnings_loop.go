package miner

import (
	"context"
	"sync"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
)

// proxy_manager_earnings_loop.go is the recording half of the 3.23-fix v33
// provider's runEarningWindows (main.go): the one-minute cadence that folds the
// live per-identity billable byte counters into the two per-proxy earnings
// consumers.
//
//   - globalPerProxyEarnTracker (proxy_manager_earn_tracker.go) answers "is
//     this identity earning right now". The paid grader's earn-skip and the
//     proxy audit's park decision read it, so a proxy that is demonstrably
//     alive is not re-probed on every sweep.
//   - globalProxyEarningsStore (proxy_manager_earnings_store.go) keeps the
//     decayed earnings history, persisted to <state>/proxy_earnings.json,
//     which the launch ranking, the trim tiebreak, the scout and the audit
//     read across restarts.
//
// v33 reads connect.ProxyBandwidthSnapshotByKey(), the connect-side bandwidth
// map keyed by proxy identity. The pinned full-bars/connect exports no such
// registry, so the miner publishes the same map from the counters its provide
// loop already wraps per slot: publishProxyEarnBandwidth registers a slot's
// counter under its identity key (connect.ProxySettings.Key()) and the
// recorder reads it back. The direct slot has no identity key and is never
// published: it is not a paid proxy, it relays no billable traffic, and it
// must never be earn-skipped.
//
// Both consumers key by IDENTITY (address, or address+user for a credentialed
// proxy), never by bare address: two accounts behind one gateway are different
// paid proxies, and crediting the bare address would either collide their
// scores or recreate the legacy bare-address keys the store migrated away
// from.

// proxyEarnBandwidthRegistry is the live per-identity byte counters the
// recorder folds in, published by the provide loop as each slot starts.
var proxyEarnBandwidthRegistry = struct {
	sync.Mutex
	byKey map[string]*bandwidth.ProxyBandwidth
}{byKey: map[string]*bandwidth.ProxyBandwidth{}}

// publishProxyEarnBandwidth registers one live slot's counters under its proxy
// identity key. It is called by the provide loop when the slot's bandwidth
// wrapper is installed, so the counter the recorder reads is the same object
// the relay counts into — the earnings history and the earn tracker can never
// drift from the billing counters.
func publishProxyEarnBandwidth(key string, bw *bandwidth.ProxyBandwidth) {
	if key == "" || bw == nil {
		return
	}
	proxyEarnBandwidthRegistry.Lock()
	defer proxyEarnBandwidthRegistry.Unlock()
	proxyEarnBandwidthRegistry.byKey[key] = bw
}

// unpublishProxyEarnBandwidth withdraws a slot's counters when it exits, so an
// offline identity leaves the live snapshot (the tracker prunes it; the
// earnings record survives in the store, which is the point of the store).
func unpublishProxyEarnBandwidth(key string) {
	if key == "" {
		return
	}
	proxyEarnBandwidthRegistry.Lock()
	defer proxyEarnBandwidthRegistry.Unlock()
	delete(proxyEarnBandwidthRegistry.byKey, key)
}

// proxyEarnBandwidthSnapshot copies the live identity-keyed counters. The map
// is copied so the recorder folds a stable snapshot while the provide loop
// publishes and withdraws slots.
func proxyEarnBandwidthSnapshot() map[string]*bandwidth.ProxyBandwidth {
	proxyEarnBandwidthRegistry.Lock()
	defer proxyEarnBandwidthRegistry.Unlock()
	if len(proxyEarnBandwidthRegistry.byKey) == 0 {
		return nil
	}
	out := make(map[string]*bandwidth.ProxyBandwidth, len(proxyEarnBandwidthRegistry.byKey))
	for key, bw := range proxyEarnBandwidthRegistry.byKey {
		out[key] = bw
	}
	return out
}

// resetProxyEarnBandwidthForTest clears the published counters. Tests only.
func resetProxyEarnBandwidthForTest() {
	proxyEarnBandwidthRegistry.Lock()
	defer proxyEarnBandwidthRegistry.Unlock()
	proxyEarnBandwidthRegistry.byKey = map[string]*bandwidth.ProxyBandwidth{}
}

// proxyEarningsMaxSamples is how many one-minute billable deltas the [earn]
// line's 60-minute window keeps, exactly like the v33 loop.
const proxyEarningsMaxSamples = 60

// proxyEarningsRecorder is the state the v33 loop carries across ticks: the
// recent aggregate billable deltas behind the [earn] line, and the previous
// tick's total.
type proxyEarningsRecorder struct {
	deltas         []uint64
	prevCum        uint64
	prevSet        bool
	prevEarnActive bool
}

// record folds one tick: the live counters into the tracker and the earnings
// store, then the aggregate billable windows into the [earn] line.
//
// With no live proxies the tracker is cleared (v33 clears it when the health
// count drops to zero) so an identity that re-appears later cannot inherit
// stale "earning" state that would wrongly suppress its first paid probe. The
// earnings store is deliberately not cleared: an offline proxy still has an
// earnings record.
func (r *proxyEarningsRecorder) record(now time.Time) {
	bw := proxyEarnBandwidthSnapshot()
	if len(bw) == 0 {
		r.prevSet = false
		globalPerProxyEarnTracker.Update(nil)
		return
	}

	globalPerProxyEarnTracker.Update(bw)
	globalProxyEarningsStore.Observe(bw, now)
	globalProxyEarningsStore.MaybeSave(now)

	var cum uint64
	for _, p := range bw {
		cum += p.BillableRx.Load() + p.BillableTx.Load()
	}

	if r.prevSet {
		if cum >= r.prevCum {
			r.deltas = append(r.deltas, cum-r.prevCum)
		} else {
			r.deltas = append(r.deltas, 0)
		}
		if len(r.deltas) > proxyEarningsMaxSamples {
			r.deltas = r.deltas[len(r.deltas)-proxyEarningsMaxSamples:]
		}

		billable1m := proxyEarnSumLastN(r.deltas, 1)
		billable5m := proxyEarnSumLastN(r.deltas, 5)
		billable15m := proxyEarnSumLastN(r.deltas, 15)
		billable60m := proxyEarnSumLastN(r.deltas, 60)

		active := "no"
		if billable1m > 0 {
			active = "yes"
		}

		// The line is emitted when the active state flips or while it is
		// active, so an idle node is quiet and an earning one reports every
		// minute (v33's rule).
		nowActive := billable1m > 0
		if nowActive != r.prevEarnActive || nowActive {
			tlog("💰 [earn] billable_1m=%s billable_5m=%s billable_15m=%s billable_60m=%s active=%s\n",
				formatBytes(billable1m), formatBytes(billable5m), formatBytes(billable15m), formatBytes(billable60m), active)
			r.prevEarnActive = nowActive
		}
	}
	r.prevCum = cum
	r.prevSet = true
}

// runProxyEarningsRecorder is the loop the manager starts beside the health
// cadence: one tick per earnCheckInterval (the v33 earnCheckInterval).
func runProxyEarningsRecorder(ctx context.Context) {
	recorder := &proxyEarningsRecorder{}
	ticker := time.NewTicker(earnCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			recorder.record(now)
		}
	}
}

// proxyEarnSumLastN sums the last n deltas. Fewer than n entries sums what
// there is (a partial window), exactly like the v33 sumLastN.
func proxyEarnSumLastN(deltas []uint64, n int) uint64 {
	if len(deltas) < n {
		n = len(deltas)
	}
	var total uint64
	for _, d := range deltas[len(deltas)-n:] {
		total += d
	}
	return total
}
