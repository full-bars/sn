package miner

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
)

// metrics_profit.go ports the profit / billable-rate surface of the 3.23-fix
// provider's provider/metrics_collectors.go:
//
//   - runProfitHeartbeat  -> the 15s [profit] line
//   - runBillableRateWriter + writeRate -> <state>/billable_rate
//
// billable_rate is not cosmetic. internal/urnettools/idle_update.go polls that
// file to decide whether it is safe to apply an update: a rate at or below its
// threshold counts as idle. The consumer cannot distinguish "no traffic" from
// "no metric", so when the file is ABSENT it deliberately assumes ACTIVE
// traffic ("billable_rate metric not found; assuming active traffic") and waits
// out its whole 30m timeout. The provider has always written the file; the
// miner never did, so every miner idle-update paid the full timeout even on a
// silent node. Writing a real 0 is the fix: 0 <= threshold reads as idle,
// whereas absent reads as busy.
//
// The miner reads the same counters through its own registries, because the
// pinned full-bars/connect exports no bandwidth registry:
//
//   - proxyEarnBandwidthSnapshot (proxy_manager_earnings_loop.go) is the
//     identity-keyed live counter set the provide loop publishes per slot —
//     the counterpart of the provider's ProxyBandwidthSnapshotByKey.
//   - proxyLiveHealthSnapshot (proxy_manager_health_registry.go) is the live
//     per-identity health — the health half of the provider's
//     ProxyHealthSnapshot.
//
// Metric names, the file format, the cadences and the log-line shape are the
// provider's. Where the miner has no source for a provider field, the field is
// omitted rather than faked (see profitLine).

const (
	// billableRateWriteInterval is the provider's runBillableRateWriter cadence.
	// It must stay well under idle-update's 5m window so a lull is visible long
	// before the window is evaluated.
	billableRateWriteInterval = 10 * time.Second
	// profitHeartbeatInterval is the provider's runProfitHeartbeat cadence.
	profitHeartbeatInterval = 15 * time.Second
	// profitIdleLogInterval caps how often a NON-earning [profit] line prints,
	// so a quiet node does not flood the log at the tick rate (provider
	// profitIdleLogInterval).
	profitIdleLogInterval = 5 * time.Minute
	// profitWarmupConnecting is the provider's `len(connecting) >= 5` warmup
	// threshold, i.e. "enough proxies are still dialing that no demand yet is
	// not evidence of no demand".
	profitWarmupConnecting = 5
)

// formatRate formats a bytes-per-second rate as a human-readable string. It is
// the provider's fmtRate verbatim: the [profit] line's rate= field is compared
// against provider logs by operators, so the unit ladder and precision match
// (decimal units here, unlike formatBytes' binary ones).
func formatRate(bytesPerSec float64) string {
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

// earningReason returns a short, greppable token for the [profit] line's
// reason= field explaining why billable traffic is or is not moving, so an
// operator can distinguish "no demand" from "no proxies" from "still warming
// up". Provider earningReason, same ordering: most fundamental first — a
// healthy earning node needs proxies up, clients matched to them, and bytes
// actually moving.
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

// profitSample is one coherent reading of the two live registries. It is a
// value, not a live view, so a tick cannot observe half a publish.
type profitSample struct {
	// population is how many identities the health registry knows about — the
	// counterpart of the provider's ProxyHealthCount(). Zero means this process
	// has no registered proxy at all, and the tick is skipped exactly as the
	// provider skips it.
	population int
	// proxiesUp counts identities classified up. A published slot is NOT
	// evidence of "up": the provide loop publishes its counters before
	// authentication starts (run.go), so a slot that is still dialing, or one
	// that keeps failing auth, is published but not up.
	proxiesUp int
	// connecting counts identities whose last health note was "connecting",
	// which is what the provider's warmup test consumes.
	connecting int
	// clients is the number of live client contracts across the published
	// slots (see proxyBillingSampler.sampleClients), and serving is how many
	// identities have at least one.
	clients int64
	serving int
	// billableByKey is the identity-keyed billable total of every published
	// slot. It is a map rather than a sum because the heartbeat must delta per
	// identity — see profitHeartbeat.billableDelta.
	billableByKey map[string]uint64
	// billableTotal and settledTotal are the run's cumulative sums over the
	// bandwidth registry, the same figures /metrics publishes as
	// urnet_billable_bytes_total and urnet_contract_used_bytes_total. They are
	// read from the registry rather than folded from the live snapshot above so
	// the log line and the scrape cannot disagree, and settledTotal is what the
	// platform's contracts have used — carrying it here is what lets a human
	// reading the log see the drift between the two without opening Grafana.
	billableTotal uint64
	settledTotal  uint64
	totalsKnown   bool
	// contractsAcquired, avgUtilPct and contractsKnown are the process
	// contract accounting the scrape publishes, read from the same snapshot
	// (contractStatsSnapshot) so the line and urnet_contracts_total cannot
	// disagree. contractsAcquired is cumulative; avgUtilPct is the open book's
	// mean utilization and NaN when undefined. contractsKnown says a slot has
	// ever registered a contract feed -- before that these are unknown, not
	// zero.
	contractsAcquired int64
	avgUtilPct        float64
	contractsKnown    bool
}

// readProfitSample folds the two live registries into one profit reading.
func readProfitSample() profitSample {
	health := proxyLiveHealthSnapshot()
	bw := proxyEarnBandwidthSnapshot()

	s := profitSample{population: len(health)}
	for _, h := range health {
		// proxyHealthBucket maps up->up, connecting/other->degraded,
		// ""/dead/inactive->dead. Only "up" is up, as in the provider's
		// classifyProxyHealth, where "connecting" is its own class.
		if proxyHealthBucket(h.Health) == "up" {
			s.proxiesUp++
		}
		if h.Health == "connecting" {
			s.connecting++
		}
	}
	for _, p := range bw {
		c := p.Clients.Load()
		s.clients += c
		if c > 0 {
			s.serving++
		}
	}
	if len(bw) > 0 {
		s.billableByKey = make(map[string]uint64, len(bw))
		for key, p := range bw {
			s.billableByKey[key] = p.BillableRx.Load() + p.BillableTx.Load()
		}
	}
	if totals := controlRuntime.trafficReading(); totals.OK {
		s.billableTotal = totals.BillableRx + totals.BillableTx
		s.settledTotal = totals.ContractUsedIngress + totals.ContractUsedEgress
		s.totalsKnown = true
	}
	if stats, ok := contractStatsSnapshot(); ok {
		s.contractsAcquired = stats.Acquired
		s.avgUtilPct = stats.AvgUtilPct
		s.contractsKnown = true
	}
	return s
}

// profitLine renders the [profit] line. The provider's
// `contracts=... denied=... avg_util=...` group is rendered from the same
// contract accounting the scrape publishes (contract_metrics.go): contracts= is
// the cumulative acquired count and avg_util= the open book's mean utilization.
// denied= is the one field the miner omits: a refused contract never opens and
// the pinned SDK exports no denial event, so there is no count to print
// (metrics_seams.go explains the gap). A missing denied field therefore means
// "cannot measure", not zero. When no slot has ever registered a contract feed
// the whole group is omitted, because an unknown is not a zero either.
func profitLine(earning bool, s profitSample, delta uint64, elapsed float64) string {
	status := "no"
	profitEmoji := ""
	if earning {
		status = "yes"
		profitEmoji = "💰 "
	}
	idle := s.proxiesUp - s.serving
	if idle < 0 {
		idle = 0
	}
	warmup := s.connecting >= profitWarmupConnecting
	reason := earningReason(earning, s.proxiesUp, s.clients, warmup)
	line := fmt.Sprintf("%s[profit] earning=%s reason=%s clients=%d rate=%s proxies_up=%d serving=%d idle=%d",
		profitEmoji, status, reason, s.clients, formatRate(float64(delta)/elapsed), s.proxiesUp, s.serving, idle)
	// The billable-vs-settled cross-check, cumulative over the run. Both
	// numbers are printed when the registry has published either, so drift is
	// visible here and not only in a scrape; neither is printed when the run
	// has no counters at all, because "unknown" must not read as zero bytes.
	if s.totalsKnown {
		line += fmt.Sprintf(" billable=%s settled=%s", formatBytes(s.billableTotal), formatBytes(s.settledTotal))
	}
	if s.contractsKnown {
		line += fmt.Sprintf(" contracts=%d avg_util=", s.contractsAcquired)
		if math.IsNaN(s.avgUtilPct) {
			line += "n/a"
		} else {
			line += fmt.Sprintf("%.0f%%", s.avgUtilPct)
		}
	}
	return line + "\n"
}

// profitHeartbeat is the state the provider's runProfitHeartbeat carries across
// ticks: the previous per-identity billable totals and tick time for the delta,
// plus the log throttle and the traffic-transition markers.
type profitHeartbeat struct {
	// prevBillable is keyed by proxy IDENTITY, not summed. The delta has to be
	// per identity for two reasons the provider's scalar sum gets wrong:
	//
	//   - The registry keeps one counter per identity INDEX for the life of the
	//     process (bandwidth.ProxyRegistry.Register). A slot that leaves drops
	//     out of the published snapshot, so the summed total falls, and when
	//     the same identity is re-added its counter comes back with its whole
	//     history — which a scalar delta reads as one enormous burst and
	//     reports as `earning=yes`.
	//   - A departure understates the next tick: the sum loses that identity's
	//     bytes before the tick that would have counted them.
	//
	// Reading the map instead makes a returning identity a fresh baseline (it
	// is absent from prev, so it contributes nothing) and keeps every other
	// identity's growth exactly as it happened. billableRateTracker.rate does
	// the same for the file the idle-update tool polls; the two must agree, or
	// [profit] would claim earnings the idle gate cannot see.
	prevBillable map[string]uint64
	prevSet      bool
	prevTickTime time.Time
	lastLogTime  time.Time
	wasEarning   bool
	prevClients  int64
}

func newProfitHeartbeat(now time.Time) *profitHeartbeat {
	return &profitHeartbeat{prevTickTime: now}
}

// billableDelta returns the billable bytes the snapshot grew by since the last
// tick, summed over identities present in both, and re-baselines the map.
//
// A counter that went backwards (a slot rebuilt inside the same identity)
// contributes zero rather than wrapping, and an identity that left the snapshot
// is dropped from the baseline so its return is not counted as a burst — the
// same two guards billableRateTracker.rate applies.
func (h *profitHeartbeat) billableDelta(now map[string]uint64) uint64 {
	var delta uint64
	for key, cur := range now {
		if prev, ok := h.prevBillable[key]; ok && cur >= prev {
			delta += cur - prev
		}
	}
	h.prevBillable = now
	return delta
}

// tick folds one sample and returns the lines to emit (none, one, or a traffic
// marker plus the line). Returning lines instead of logging keeps the cadence
// and the throttle testable without a log seam; runProfitHeartbeat writes them
// to tlog.
func (h *profitHeartbeat) tick(now time.Time, s profitSample) []string {
	// No registered proxy: the provider clears its baseline and emits nothing,
	// so a node that has not started its pool yet cannot report a rate.
	if s.population == 0 {
		h.prevSet = false
		h.prevBillable = nil
		return nil
	}

	// First sample only establishes the baseline; the provider `continue`s here
	// without logging, because a delta needs two points.
	if !h.prevSet {
		h.prevBillable = s.billableByKey
		h.prevTickTime = now
		h.prevSet = true
		return nil
	}

	elapsed := now.Sub(h.prevTickTime).Seconds()
	if elapsed < 1 {
		elapsed = 1
	}
	delta := h.billableDelta(s.billableByKey)
	h.prevTickTime = now

	// Earning needs BOTH bytes moving and clients matched: bytes can only move
	// because of clients, so requiring both keeps a counter glitch from
	// reporting demand that is not there.
	earning := delta > 0 && s.clients > 0
	justStopped := h.wasEarning && !earning

	var lines []string
	if h.prevClients == 0 && s.clients > 0 {
		lines = append(lines, fmt.Sprintf("✈️ [traffic] started (clients=%d)\n", s.clients))
	} else if h.prevClients > 0 && s.clients == 0 {
		lines = append(lines, fmt.Sprintf("🛬 [traffic] stopped (was=%d clients)\n", h.prevClients))
	}
	h.prevClients = s.clients
	h.wasEarning = earning

	// Print every tick while earning; print immediately on the stop edge so the
	// exact stop time is visible; otherwise throttle.
	if earning || justStopped || h.lastLogTime.IsZero() || now.Sub(h.lastLogTime) >= profitIdleLogInterval {
		lines = append(lines, profitLine(earning, s, delta, elapsed))
		h.lastLogTime = now
	}
	return lines
}

// runProfitHeartbeat logs the [profit] line (provider runProfitHeartbeat).
func runProfitHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(profitHeartbeatInterval)
	defer ticker.Stop()

	h := newProfitHeartbeat(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for _, line := range h.tick(now, readProfitSample()) {
				tlog("%s", line)
			}
		}
	}
}

// billableRateTracker converts the live per-identity billable counters into the
// aggregate bytes/second written to billable_rate (provider
// runBillableRateWriter's map + elapsed arithmetic).
type billableRateTracker struct {
	prev     map[string]uint64
	prevTick time.Time
}

func newBillableRateTracker(now time.Time) *billableRateTracker {
	return &billableRateTracker{prev: map[string]uint64{}, prevTick: now}
}

// rate folds one sample and returns the aggregate billable bytes/second since
// the previous sample.
//
// Keys are proxy IDENTITIES, not addresses: two accounts behind one gateway are
// two counters, and a per-address key would drop one of them. A key that leaves
// the snapshot is dropped from the baseline so a returning identity is not
// credited with its whole history as a fresh delta. A counter that went
// backwards (slot restarted, counter reset) contributes zero rather than
// wrapping, which is the provider's guard.
func (t *billableRateTracker) rate(now time.Time, bw map[string]*bandwidth.ProxyBandwidth) uint64 {
	if len(bw) == 0 {
		// No live proxy: the rate is really zero, and saying so is what makes
		// idle-update proceed on a stopped pool instead of waiting out its
		// timeout. Baselines are dropped so a pool that comes back does not
		// count the gap as one enormous burst.
		t.prev = map[string]uint64{}
		t.prevTick = now
		return 0
	}

	var totalDelta uint64
	for key, p := range bw {
		cur := p.BillableRx.Load() + p.BillableTx.Load()
		if prev, ok := t.prev[key]; ok && cur >= prev {
			totalDelta += cur - prev
		}
		t.prev[key] = cur
	}
	for key := range t.prev {
		if _, ok := bw[key]; !ok {
			delete(t.prev, key)
		}
	}

	elapsed := now.Sub(t.prevTick).Seconds()
	if elapsed < 1 {
		elapsed = 1
	}
	t.prevTick = now
	return uint64(float64(totalDelta) / elapsed)
}

// billableRatePath resolves the file urnet-tools idle-update polls:
// <provider state dir>/billable_rate — URNETWORK_STATE_DIR when set, else
// <home>/.urnetwork.
//
// It deliberately does NOT go through proxyHealthDir(). Those are two separate
// knobs: URNETWORK_PROXY_HEALTH_DIR relocates proxy_health.state and
// proxy_traffic.state, and nothing in internal/urnettools reads it. Resolving
// the rate through the health dir made writer and reader disagree in exactly
// the deployments that set that variable: idle-update looked in the state dir,
// found nothing, and because an ABSENT file means "metric unknown, assume
// active traffic" it waited out its full 30m timeout on a silent node. The rate
// has to land where the reader looks, so it follows the reader's rule — the
// same one providerStateDir applies to the JWT, provider.sock and the rest of
// the miner's state.
func billableRatePath() (string, bool) {
	dir, err := providerStateDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(dir, "billable_rate"), true
}

// writeBillableRate atomically writes the aggregate rate to the path
// billableRatePath names, the file urnet-tools idle-update polls. The
// provider's writeRate: tmp file plus rename so a reader never sees a partial
// number (a torn "12" out of "1234" would read as idle and let an update land
// mid-transfer).
//
// The directory is created at 0700 when absent, matching every other state-dir
// write in the miner: this is the provider state dir, which also holds the JWT
// and key material, so it must never be created world-readable. When the
// directory already exists MkdirAll is a no-op and its mode is left alone. The
// rate file itself is 0644, as the provider writes it, because it holds a byte
// count and idle-update may read it as another user inside a container.
func writeBillableRate(rate uint64) {
	path, ok := billableRatePath()
	if !ok {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		tlog("[billable_rate] warn: cannot create state dir %s: %v\n", dir, err)
		return
	}
	tmp := path + ".tmp"
	content := strconv.FormatUint(rate, 10) + "\n"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		tlog("[billable_rate] warn: failed to write billable rate file (%s): %v\n", path, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		tlog("[billable_rate] warn: failed to finalize billable rate file (rename tmp -> %s): %v\n", path, err)
	}
}

// runBillableRateWriter publishes the aggregate billable rate every 10s
// (provider runBillableRateWriter). Read-only over the network stack: it only
// loads counters the relay already keeps.
func runBillableRateWriter(ctx context.Context) {
	ticker := time.NewTicker(billableRateWriteInterval)
	defer ticker.Stop()

	tracker := newBillableRateTracker(time.Now())
	tlog("[billable_rate] writer started (interval=%s)\n", billableRateWriteInterval)

	for {
		select {
		case <-ctx.Done():
			tlog("[billable_rate] writer stopped (shutdown)\n")
			return
		case now := <-ticker.C:
			writeBillableRate(tracker.rate(now, proxyEarnBandwidthSnapshot()))
		}
	}
}
