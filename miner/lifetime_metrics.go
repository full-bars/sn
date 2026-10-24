package miner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/urfoundation/sn/miner/bandwidth"
)

// lifetime_metrics.go ports the 3.23-fix v33 provider's all-time counter store
// (provider/lifetime_metrics.go) and its collector (provider/metrics_collectors.go
// runLifetimeCollector). Everything here already existed as an in-process
// counter somewhere; the problem was that process death erased it. This store
// holds the ALL-TIME totals: loaded once at startup as the pre-restart base,
// incremented by DELTA observations from the periodic collector (never absolute
// assignments, because a counter reset between ticks must not corrupt history),
// and flushed to disk on a throttle plus a final flush on shutdown and at the
// two hotswap points that bypass the normal shutdown.
//
// The file lives beside the other miner state files (<state>/lifetime_metrics.json)
// and keeps the v33 JSON shape, so the scrape and any operator tooling read it
// unchanged. Writes are atomic: temp file + rename, so a crash mid-write can
// never leave a half-written store.
type lifetimeMetrics struct {
	mu sync.Mutex

	// Persistent totals (the authoritative numbers).
	PQEOpens    uint64 `json:"pqe_opens"`
	ClasOpens   uint64 `json:"clas_opens"`
	ContractsUp uint64 `json:"contracts_acquired"`
	// ContractsDeny is the v33 file's denied slot, kept so a persisted store
	// round-trips unchanged. The miner has no denial source at all (see
	// metrics_seams.go), so nothing increments it and nothing publishes it: a
	// constant zero would read as "nothing was refused".
	ContractsDeny uint64 `json:"contracts_denied"`
	ProxiesRecov  uint64 `json:"proxies_recovered"`
	ProxiesLost   uint64 `json:"proxies_lost"`
	BillableBytes uint64 `json:"billable_bytes"`

	// Since is when these totals began accumulating, so a log line can say what
	// "all-time" actually covers. It is written once, when the store is created,
	// and never rewritten afterwards.
	//
	// SinceApprox marks a store that predates this field: its totals really do
	// start somewhere earlier, but nothing recorded where, so the start is
	// pinned to the first load that understood the field.
	Since       time.Time `json:"since,omitempty"`
	SinceApprox bool      `json:"since_approx,omitempty"`

	path       string
	dirty      bool
	lastFlush  time.Time
	flushEvery time.Duration
}

// lifetimeMetricsPath returns <state>/lifetime_metrics.json, or "" when no
// state directory is available (the store then lives in memory only).
func lifetimeMetricsPath() string {
	path, err := providerStatePath("lifetime_metrics.json")
	if err != nil {
		return ""
	}
	return path
}

// loadLifetimeMetrics reads the persisted store. A missing or unreadable file
// starts a fresh store rather than blocking the provider: these are
// operator-visible stats, not consensus state.
func loadLifetimeMetrics(path string) *lifetimeMetrics {
	lm := &lifetimeMetrics{path: path, flushEvery: 5 * time.Minute, lastFlush: time.Now(), Since: time.Now()}
	if path == "" {
		return lm
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return lm // first boot or unreadable: start fresh, so "since" is now
	}
	var persisted struct {
		PQEOpens      uint64 `json:"pqe_opens"`
		ClasOpens     uint64 `json:"clas_opens"`
		ContractsUp   uint64 `json:"contracts_acquired"`
		ContractsDeny uint64 `json:"contracts_denied"`
		ProxiesRecov  uint64 `json:"proxies_recovered"`
		ProxiesLost   uint64 `json:"proxies_lost"`
		BillableBytes uint64 `json:"billable_bytes"`
		SavedAt       string `json:"saved_at"`
		Since         string `json:"since"`
		SinceApprox   bool   `json:"since_approx"`
	}
	if json.Unmarshal(data, &persisted) != nil {
		return lm
	}
	lm.PQEOpens = persisted.PQEOpens
	lm.ClasOpens = persisted.ClasOpens
	lm.ContractsUp = persisted.ContractsUp
	lm.ContractsDeny = persisted.ContractsDeny
	lm.ProxiesRecov = persisted.ProxiesRecov
	lm.ProxiesLost = persisted.ProxiesLost
	lm.BillableBytes = persisted.BillableBytes
	// A store written before the field existed keeps its real totals but has no
	// recorded start: pin the start to now and say so, rather than claiming a
	// date the numbers do not cover.
	if since, perr := time.Parse(time.RFC3339, persisted.Since); perr == nil && !since.IsZero() {
		lm.Since = since
		lm.SinceApprox = persisted.SinceApprox
	} else {
		lm.SinceApprox = true
		// Persist the pinned start even if no counter ever moves: otherwise an
		// idle node never flushes, and every restart re-pins a slightly later
		// date while the totals stay put.
		lm.dirty = true
	}
	// Anchor the flush throttle to the last real save (not boot time), so a
	// quick restart does not delay the first persistence of new deltas by a
	// full throttle window after an already-stale file.
	if savedAt, perr := time.Parse(time.RFC3339, persisted.SavedAt); perr == nil {
		lm.lastFlush = savedAt
	}
	return lm
}

// Started reports when the lifetime totals began and whether that start is
// approximate. A false approximation means the date is exact; true means the
// totals include history from before this build recorded one.
func (lm *lifetimeMetrics) Started() (since time.Time, approx bool) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.Since, lm.SinceApprox
}

// sinceLabel renders a start time for an operator-facing log line.
func sinceLabel(t time.Time, approx bool) string {
	if t.IsZero() {
		return "since unknown"
	}
	label := "since " + clockLabel(t)
	if approx {
		label += ", start approximate, totals include earlier history"
	}
	return label
}

// clockLabel renders an instant the way the operator-facing lines show it:
// local time with the zone, so "when did this start" needs no arithmetic.
func clockLabel(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format("2006-01-02 15:04 MST")
}

// Add applies positive deltas to named totals and marks the store dirty. A
// fully-zero call is a cheap no-op that does NOT mark the store dirty, so idle
// nodes never rewrite the state file.
func (lm *lifetimeMetrics) Add(pqeOpens, clasOpens, contractsUp, contractsDeny, proxiesRecov, proxiesLost uint64, billableBytesDelta uint64) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if pqeOpens|clasOpens|contractsUp|contractsDeny|proxiesRecov|proxiesLost|billableBytesDelta == 0 {
		return
	}
	lm.PQEOpens += pqeOpens
	lm.ClasOpens += clasOpens
	lm.ContractsUp += contractsUp
	lm.ContractsDeny += contractsDeny
	lm.ProxiesRecov += proxiesRecov
	lm.ProxiesLost += proxiesLost
	lm.BillableBytes += billableBytesDelta
	lm.dirty = true
}

// MaybeFlush writes the store to disk if it is dirty and the flush throttle has
// elapsed. Returns whether a write happened.
func (lm *lifetimeMetrics) MaybeFlush(now time.Time) bool {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if !lm.dirty || lm.path == "" {
		return false
	}
	if !lm.lastFlush.IsZero() && now.Sub(lm.lastFlush) < lm.flushEvery {
		return false
	}
	if err := lm.writeLocked(); err != nil {
		return false // keep dirty; retry next tick
	}
	lm.dirty = false
	lm.lastFlush = now
	return true
}

// Flush forces a write (shutdown and hotswap paths).
func (lm *lifetimeMetrics) Flush() {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if !lm.dirty || lm.path == "" {
		return
	}
	if lm.writeLocked() == nil {
		lm.dirty = false
	}
}

// writeLocked performs the atomic temp+rename write. Caller holds mu.
func (lm *lifetimeMetrics) writeLocked() error {
	if err := os.MkdirAll(filepath.Dir(lm.path), 0o700); err != nil {
		return err
	}
	// A zero Since must stay empty: time.Time marshals its zero value as year 1,
	// which would come back as an exact start on the next load.
	sinceValue := ""
	if !lm.Since.IsZero() {
		sinceValue = lm.Since.UTC().Format(time.RFC3339)
	}
	blob, err := json.MarshalIndent(struct {
		PQEOpens      uint64 `json:"pqe_opens"`
		ClasOpens     uint64 `json:"clas_opens"`
		ContractsUp   uint64 `json:"contracts_acquired"`
		ContractsDeny uint64 `json:"contracts_denied"`
		ProxiesRecov  uint64 `json:"proxies_recovered"`
		ProxiesLost   uint64 `json:"proxies_lost"`
		BillableBytes uint64 `json:"billable_bytes"`
		SavedAt       string `json:"saved_at"`
		Since         string `json:"since,omitempty"`
		SinceApprox   bool   `json:"since_approx,omitempty"`
	}{
		PQEOpens:      lm.PQEOpens,
		ClasOpens:     lm.ClasOpens,
		ContractsUp:   lm.ContractsUp,
		ContractsDeny: lm.ContractsDeny,
		ProxiesRecov:  lm.ProxiesRecov,
		ProxiesLost:   lm.ProxiesLost,
		BillableBytes: lm.BillableBytes,
		SavedAt:       time.Now().UTC().Format(time.RFC3339),
		Since:         sinceValue,
		SinceApprox:   lm.SinceApprox,
	}, "", " ")
	if err != nil {
		return err
	}
	tmp := lm.path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, lm.path)
}

// Snapshot returns a copy of the current totals for the scrape and the line.
func (lm *lifetimeMetrics) Snapshot() (pqe, clas, up, deny, recov, lost, bytes uint64) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.PQEOpens, lm.ClasOpens, lm.ContractsUp, lm.ContractsDeny,
		lm.ProxiesRecov, lm.ProxiesLost, lm.BillableBytes
}

// lifetimeStore is the process's all-time counter store. hotswap.go flushes it
// at the two points a handoff bypasses the normal shutdown; the collector
// flushes it on shutdown and on its throttle; the scrape reads it.
var lifetimeStore = loadLifetimeMetrics(lifetimeMetricsPath())

func init() { metricsSeamLifetime = lifetimeSeamSnapshot }

// lifetimeSeamSnapshot exposes the persisted totals and the start to the
// scrape (urnet_lifetime_* and the two start gauges).
func lifetimeSeamSnapshot() *seamLifetime {
	if lifetimeStore == nil {
		return nil
	}
	pqe, clas, up, _, recov, lost, bill := lifetimeStore.Snapshot()
	since, approx := lifetimeStore.Started()
	return &seamLifetime{
		SessionsPQE:       pqe,
		SessionsClas:      clas,
		ContractsAcquired: up,
		ProxiesRecovered:  recov,
		ProxiesLost:       lost,
		BillableBytes:     bill,
		Since:             since,
		SinceApprox:       approx,
	}
}

// noteHealthReportForLifetime feeds the proxy transition deltas into the store.
// The health snapshot writer is the exclusive consumer of the report, so each
// event is counted exactly once here. Lost counts up->down events only:
// NewlyDegraded plus NewlyDead, never the complete dead list, which would
// inflate the counter on every tick a proxy stays dead.
func noteHealthReportForLifetime(report proxyHealthReport) {
	lifetimeStore.Add(0, 0, 0, 0,
		uint64(len(report.Recovered)),
		uint64(len(report.NewlyDegraded))+uint64(len(report.NewlyDead)), 0)
}

// lifetimeDepartedTTL is how long a departed identity's billable baseline is
// remembered, so a proxy bouncing within the window resumes against its own
// history instead of re-counting everything it ever carried.
const lifetimeDepartedTTL = 24 * time.Hour

type lifetimeDepartedBaseline struct {
	baseline uint64
	left     time.Time
}

// lifetimeBillableFlip tracks the previous cumulative billable reading per
// identity and turns the live counters into deltas. Per identity, not one
// running total: a proxy that joins mid-run arrives with its own history, and a
// single total would count all of it as fresh traffic.
type lifetimeBillableFlip struct {
	prev     map[string]uint64
	departed map[string]lifetimeDepartedBaseline
}

func newLifetimeBillableFlip() *lifetimeBillableFlip {
	return &lifetimeBillableFlip{
		prev:     map[string]uint64{},
		departed: map[string]lifetimeDepartedBaseline{},
	}
}

// fold returns the billable bytes every live identity added since the previous
// fold. A first sighting is a baseline and adds nothing; a reset counter adds
// nothing; a departed identity's baseline is kept for lifetimeDepartedTTL.
func (f *lifetimeBillableFlip) fold(bw map[string]*bandwidth.ProxyBandwidth, now time.Time) uint64 {
	var sum uint64
	for key, p := range bw {
		cum := p.BillableRx.Load() + p.BillableTx.Load()
		base, seen := f.prev[key]
		if !seen {
			if dep, was := f.departed[key]; was && now.Sub(dep.left) < lifetimeDepartedTTL {
				base, seen = dep.baseline, true
			}
		}
		delete(f.departed, key)
		if seen && cum >= base {
			sum += cum - base
		}
		f.prev[key] = cum
	}
	// Move baselines of departed identities into the tombstone map.
	for key := range f.prev {
		if _, live := bw[key]; !live {
			f.departed[key] = lifetimeDepartedBaseline{baseline: f.prev[key], left: now}
			delete(f.prev, key)
		}
	}
	// Expire stale tombstones so the map stays bounded under churn.
	for key, dep := range f.departed {
		if now.Sub(dep.left) >= lifetimeDepartedTTL {
			delete(f.departed, key)
		}
	}
	return sum
}

// lifetimeDelta is the reset-guarded counter delta: a counter that went
// backwards counts nothing for the tick and becomes the new baseline.
func lifetimeDelta(cur uint64, prev *uint64) uint64 {
	var d uint64
	if cur >= *prev {
		d = cur - *prev
	}
	*prev = cur
	return d
}

// lifetimeFoldCursor is where the last fold left the four scrape-seam counters
// (the deltas' high-water mark). Those are process-lifetime values, so a
// supervised restart of the collector that began again from zero would fold
// their whole running total into the persisted all-time store a second time.
// Resuming from the cursor keeps the fold monotonic across restarts; in a fresh
// process the cursor is unset, which is exactly the old zero start.
var lifetimeFoldCursor = &lifetimeFoldCursorState{}

type lifetimeFoldCursorState struct {
	mu            sync.Mutex
	set           bool
	pqe, clas, up uint64
}

func (c *lifetimeFoldCursorState) load() (pqe, clas, up uint64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pqe, c.clas, c.up, c.set
}

func (c *lifetimeFoldCursorState) store(pqe, clas, up uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pqe, c.clas, c.up, c.set = pqe, clas, up, true
}

// lifetimeBillableBaselines is the collector's per-identity billable baseline,
// hoisted out of runLifetimeCollector so it survives a supervised restart. Its
// prev/departed maps are as long-lived as the counters they baseline: those are
// the provide run's process-lifetime counters, so rebuilding the flip on
// restart would treat every live identity as a first sighting (dropping a
// transition) or, worse, fold its whole history again (double-counting it).
// Rebuilding from the persisted store is not an alternative: the store keeps
// one all-time billable total, not a per-identity baseline.
var lifetimeBillableBaselines = newLifetimeBillableFlip()

// lifetimeCollector is one collector run's state: the last folded value of each
// scrape-seam counter, and the previous all-time totals behind the dated line.
// A supervised restart builds a new one, which resumes the counters from
// lifetimeFoldCursor; the billable baselines live in lifetimeBillableBaselines,
// because they belong to the process, not to the run.
type lifetimeCollector struct {
	prevPQE, prevClas                              uint64
	prevUp                                         uint64
	prevA1, prevA2, prevA3, prevA5, prevA6, prevA7 uint64
	flip                                           *lifetimeBillableFlip
}

func newLifetimeCollector() *lifetimeCollector {
	c := &lifetimeCollector{flip: lifetimeBillableBaselines}
	if pqe, clas, up, ok := lifetimeFoldCursor.load(); ok {
		c.prevPQE, c.prevClas, c.prevUp = pqe, clas, up
	}
	return c
}

// tick folds one interval of live counters into the all-time store and emits
// the dated line when the totals moved.
func (c *lifetimeCollector) tick(now time.Time) {
	var pqeDelta, clasDelta uint64
	if metricsSeamSessions != nil {
		if pq := metricsSeamSessions(); pq != nil {
			pqeDelta = lifetimeDelta(nonNegativeUint64(pq.PQELifetime), &c.prevPQE)
			clasDelta = lifetimeDelta(nonNegativeUint64(pq.ClasLifetime), &c.prevClas)
		}
	}
	var upDelta uint64
	if metricsSeamContracts != nil {
		if contracts := metricsSeamContracts(); contracts != nil {
			upDelta = lifetimeDelta(nonNegativeUint64(contracts.Acquired), &c.prevUp)
		}
	}
	billable := c.flip.fold(proxyEarnBandwidthSnapshot(), now)
	// The high-water mark is published before the effect. A panic between the
	// two then loses at most this tick's delta, which the next tick re-reads
	// from the live counter, where the reverse order would re-fold an
	// already-persisted delta: a collector that panics in this window every
	// restart would inflate the all-time totals without bound.
	lifetimeFoldCursor.store(c.prevPQE, c.prevClas, c.prevUp)
	lifetimeStore.Add(pqeDelta, clasDelta, upDelta, 0, 0, 0, billable)
	lifetimeStore.MaybeFlush(now)
	// Error counts share the tick and its throttle. A failure event is rare, so
	// waiting up to one flush interval costs nothing; the shutdown, hotswap and
	// fatal exit paths flush unconditionally. The store loads on first use and
	// both the accessor and MaybeFlush tolerate a nil store, so a collector that
	// starts before the store exists cannot panic the loop.
	persistentErrorStore().MaybeFlush(now)

	a1, a2, a3, _, a5, a6, a7 := lifetimeStore.Snapshot()
	// The store's denied total is not compared or printed: the miner has no
	// denial source, so it never moves and a constant denied=0 on the line
	// would read as "nothing was refused" instead of "not measured".
	if a1 != c.prevA1 || a2 != c.prevA2 || a3 != c.prevA3 ||
		a5 != c.prevA5 || a6 != c.prevA6 || a7 != c.prevA7 {
		if a1|a2|a3|a5|a6|a7 != 0 {
			since, approx := lifetimeStore.Started()
			tlog("♾️⚖️ [lifetime] all-time (%s): post-quantum=%d classical=%d contracts_acquired=%d proxies_recovered=%d lost=%d billable_total=%s\n",
				sinceLabel(since, approx),
				a1, a2, a3, a5, a6, formatBytes(a7))
		}
		c.prevA1, c.prevA2, c.prevA3, c.prevA5, c.prevA6, c.prevA7 = a1, a2, a3, a5, a6, a7
	}
}

// runLifetimeCollector folds the live counters into the all-time store once per
// earnCheckInterval, flushes on its throttle, and emits the dated all-time line
// when something moved. Session and contract deltas come from the scrape seams
// when those counters exist; until then they contribute nothing and the store
// still persists the deltas that do exist (billable, transitions).
func runLifetimeCollector(ctx context.Context) {
	c := newLifetimeCollector()

	ticker := time.NewTicker(earnCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			lifetimeStore.Flush()
			return
		case now := <-ticker.C:
			c.tick(now)
		}
	}
}

// nonNegativeUint64 clamps a signed counter (the seam's int64 shape) before it
// enters the unsigned lifetime totals.
func nonNegativeUint64(v int64) uint64 {
	if v <= 0 {
		return 0
	}
	return uint64(v)
}
