package miner

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// This file ports the v33 provider's performance-based degraded-proxy reaper
// (provider/degraded_reaper.go) to the miner.
//
// The miner already sheds proxies two ways: by the operator trim/OOM cap, and
// by deadness (the URL reaper). Neither looks at a proxy that is ALIVE but
// performing badly, which is what the provider culls: a SOCKS endpoint that
// accepts connections, can authenticate sometimes, and moves almost no
// traffic still occupies a provider slot, an auth budget and a platform
// session. The provider reaps the worst contributors and keeps the best half
// of the degraded set; the miner did not, so a pool could sit half-full of
// nominally-live, non-earning proxies for as long as the operator left it.
//
// The port is deliberately structure-for-structure with the provider:
//
//	miner                                                 provider
//	-----------------------------------------------------  ---------------------------
//	minerDegradedProxyEntry                                DegradedProxyEntry
//	degradedReaperKeepCount / keepPct / ticker / min down   degradedReaperKeepCount / ...
//	minerScoredDegradedProxy + scoreMinerDegradedProxies    scoredDegradedProxy + scoreDegradedProxies
//	minerScoresCarryNoRanking                              scoresCarryNoRanking
//	selectMinerProxiesToReap                               selectProxiesToReap
//	(*minerProxyReloader).reap                             reapProxies
//	(*minerDegradedReaper).run / .cycle                     runDegradedProxyReaper
//
// The seams differ because the process does:
//
//   - the provider cancels entries in `proxyCancelMap` keyed by identity. The
//     miner's equivalent is the live reloader's running set, so the reaper
//     asks minerProxyReloader to cancel by identity (reap) and never touches a
//     slot it does not own;
//   - the provider reads `DegradedProxies()` out of its per-index health
//     registry. The miner has no provider health registry; it keeps live health
//     per identity (proxy_manager_health_registry.go), which is where
//     `degraded`+`down since` already come from;
//   - the provider scores by index against globalContractMetrics. The miner's
//     lifetime-contribution signal is the identity-keyed earnings store
//     (proxy_manager_earnings_store.go), so the contribution hook is keyed by
//     identity, not index.
//
// What the miner adds on top, and why, is called out at each site: the hold
// that keeps a reaped identity out of the next desired-set build (the provider
// has no desired-set build to defend), and the timeout-family exclusion the
// miner needs because its health registry, unlike the provider's, does not see
// the error kind unless it records it.

// isTimeoutFamilyError reports whether err is a timeout/deadline failure, as
// opposed to a refusal, a reset, or an API rejection.
//
// Copied verbatim from the provider: the quality reaper must draw the same line
// the provider's auth ladder draws. A proxy that is only slow must not be culled
// for under-performing — that is the failure spiral this predicate exists to
// prevent. The provider pairs the predicate with a measured-attempt threshold
// (authTimeoutCutShortThreshold: 12s of the client's 15s connect deadline) to
// tell "the deadline cut this dial short" from "this proxy refused us"; the
// miner's health write has no per-attempt duration to compare against, so it
// records the error kind alone and a deadline-cut attempt is treated as slow.
func isTimeoutFamilyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	errMsg := err.Error()
	return strings.Contains(errMsg, "Timeout") ||
		strings.Contains(errMsg, "timeout") ||
		strings.Contains(errMsg, "deadline exceeded")
}

const (
	// degradedReaperTicker is the provider's reaper cadence. Slow on purpose:
	// a proxy has to stay degraded for degradedReaperMinDownTime before it is a
	// candidate at all, so a faster pass would only add churn.
	degradedReaperTicker = 3 * time.Minute
	// degradedReaperMinDownTime is how long an identity must have been down
	// before the reaper may act on it. It is longer than any transient connect
	// wobble and gives a slow-but-live proxy a full window to recover on its
	// own.
	degradedReaperMinDownTime = 30 * time.Minute
	// degradedReaperKeepPct is the share of the DEGRADED set the reaper keeps.
	// It is a share of the candidates, not of the pool: a pool with two
	// degraded proxies loses one, a pool with twenty loses ten. Reaping keeps
	// the best half so a bad ranking can never empty the pool.
	degradedReaperKeepPct = 50
)

// degradedReaperKeepCount returns how many proxies to keep (ceil rounding).
func degradedReaperKeepCount(total int) int {
	if total <= 0 {
		return 1
	}
	keep := (total*degradedReaperKeepPct + 99) / 100
	if keep < 1 {
		return 1
	}
	return keep
}

// minerDegradedProxyEntry mirrors provider.DegradedProxyEntry for the miner's
// identity-keyed health. Index is absent because every miner consumer (the
// cancel map, the health registry, the earnings store) is keyed by identity;
// Address is kept only for the operator-facing log.
type minerDegradedProxyEntry struct {
	// Key is the proxy identity (connect.ProxySettings.Key()): what the
	// reloader's running set and the health registry are keyed by. Address
	// alone does not match for a credentialed proxy.
	Key     string
	Address string
	DownFor time.Duration
	// LastFailureTimeout records that this identity's most recent failure was
	// timeout-family (slow, not broken — see isTimeoutFamilyError).
	LastFailureTimeout bool
	TotalRxBytes       uint64
	TotalTxBytes       uint64
}

// minerDegradedProxies is the miner's DegradedProxies(): the current degraded
// set, as of the given instant. A variable so tests can supply their own.
var minerDegradedProxies = proxyLiveDegradedProxies

// proxyLiveDegradedProxies reads the degraded identities out of the live health
// registry, with the traffic each has moved.
//
// The predicate is the provider's DegradedProxies(): up at some point, not up
// now, and a down clock that has started. The miner's registry classifies the
// never-up case as its "dead" bucket (proxyHealthBucket), so a proxy that never
// authenticated is not a quality candidate here either — it belongs to the
// deadness path, not to this one.
func proxyLiveDegradedProxies(now time.Time) []minerDegradedProxyEntry {
	// Take the bandwidth snapshot before the health lock: the two registries
	// are independent, and the publish/withdraw path never takes the health
	// lock, so the order only has to be consistent here.
	bandwidth := proxyEarnBandwidthSnapshot()

	proxyLiveHealthRegistry.Lock()
	defer proxyLiveHealthRegistry.Unlock()

	var out []minerDegradedProxyEntry
	for key, entry := range proxyLiveHealthRegistry.byKey {
		if proxyHealthBucket(entry.Health) != "degraded" {
			continue
		}
		if entry.DownSince.IsZero() {
			continue
		}
		degraded := minerDegradedProxyEntry{
			Key:                key,
			DownFor:            now.Sub(entry.DownSince),
			LastFailureTimeout: entry.LastFailureTimeout,
		}
		if address, _ := connect.SplitProxyKey(key); address != "" {
			degraded.Address = address
		}
		if bw := bandwidth[key]; bw != nil {
			degraded.TotalRxBytes = bw.TotalRx.Load()
			degraded.TotalTxBytes = bw.TotalTx.Load()
		}
		out = append(out, degraded)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// minerScoredDegradedProxy pairs a degraded proxy with its lifetime-contribution
// score.
type minerScoredDegradedProxy struct {
	entry minerDegradedProxyEntry
	score uint64
}

// minerProxyContributionFunc returns the lifetime contribution of a proxy
// identity, or 0 if unknown. It is the miner's contractsAcquiredFunc.
type minerProxyContributionFunc func(key string) int64

// liveMinerProxyContribution adapts the real identity-keyed earnings store to
// minerProxyContributionFunc.
func liveMinerProxyContribution(key string) int64 {
	score := globalProxyEarningsStore.Score(key, time.Now())
	if score <= 0 {
		return 0
	}
	return int64(score)
}

// scoreMinerDegradedProxies ranks degraded proxies by lifetime contribution,
// ascending — the worst contributors sort first.
//
// The provider adds `contracts_acquired * 1024` to the live byte counters,
// because its contribution signal is a COUNT of acquired contracts and the
// multiplier scales that count into the byte range of a tiebreak. The miner's
// contribution signal is the decayed earnings store, which is already in bytes,
// so it enters the sum unscaled. Applying the provider's *1024 to a byte figure
// would let a proxy's history outweigh the live counters it is being judged on
// by three orders of magnitude and invert the ranking.
func scoreMinerDegradedProxies(entries []minerDegradedProxyEntry, getContribution minerProxyContributionFunc) []minerScoredDegradedProxy {
	scored := make([]minerScoredDegradedProxy, len(entries))
	for i, d := range entries {
		score := d.TotalRxBytes + d.TotalTxBytes
		if getContribution != nil && d.Key != "" {
			if contribution := getContribution(d.Key); contribution > 0 {
				score += uint64(contribution)
			}
		}
		scored[i] = minerScoredDegradedProxy{entry: d, score: score}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score < scored[j].score
	})
	return scored
}

// minerScoresCarryNoRanking reports whether every candidate scored the same,
// which is what an unreachable upstream produces: no bytes move, so every
// proxy's contribution freezes at the same value. An ordering with no
// information in it cannot say which proxies are under-performing, and reaping
// on it would cut a healthy pool in half the moment the platform stops
// answering. One differing score means the ranking is real again.
func minerScoresCarryNoRanking(scored []minerScoredDegradedProxy) bool {
	if len(scored) < 2 {
		// Nothing to compare: one candidate cannot be ranked against anything.
		return true
	}
	for _, s := range scored[1:] {
		if s.score != scored[0].score {
			return false
		}
	}
	return true
}

// selectMinerProxiesToReap picks which degraded proxies to cancel.
//
// Copied from the provider's selectProxiesToReap: walk the ascending ranking and
// take the worst (len-keep) entries, but only those that have been down long
// enough. The two miner-only filters are commented where they apply.
func selectMinerProxiesToReap(scored []minerScoredDegradedProxy, keep int, minDownTime time.Duration) []minerDegradedProxyEntry {
	var toReap []minerDegradedProxyEntry
	for i := 0; i < len(scored)-keep; i++ {
		p := scored[i].entry
		if p.DownFor < minDownTime {
			continue
		}
		// Timeout-family failure: the proxy was cut short by the connect
		// deadline, which is latency, not a broken proxy (isTimeoutFamilyError).
		// The provider never has to make this check in its reaper because its
		// auth ladder has already used the attempt duration to decide the same
		// question (authTimeoutCutShortThreshold) before the health registry
		// sees it; the miner records the kind at the health write instead
		// (noteLiveProxyHealthFailureCause) and honors it here.
		if p.LastFailureTimeout {
			continue
		}
		toReap = append(toReap, p)
	}
	return toReap
}

// ---- the hold that keeps a reaped identity out ----

// reapedProxyHold records the identities this run's quality reaper cancelled and
// the moment each may be re-added to the desired set.
//
// The provider does not need this: its reload loop recomputes the pool on its
// own and a cancelled proxy goroutine is simply gone until the next reload
// decides otherwise. The miner's reloader applies the desired set — which still
// contains the identity, because the reaper does not rewrite proxy.state or the
// URL cache — so without a hold the very next reload (any operator change, any
// URL refresh) would re-add the identity the reaper just judged. The hold is
// the reaper's record of that judgement, and it is what filterReapHeldProxySettings
// consults.
//
// The same window is also mirrored into globalProxyFailureHistory so the
// existing URL-sourced give-up gate (dropBackoffHeldURLProxies) honors it too,
// and so the backoff shows up in the history commands read
// (`proxy status`, the pruning keep-set).
var reapedProxyHold = struct {
	sync.Mutex
	until map[string]time.Time
}{until: map[string]time.Time{}}

// setReapedProxyHold records that key is held out of the desired-set build
// until until. It touches only the hold: the failure history is the caller's to
// write, because the two callers set the backoff differently (the degraded
// reaper places its minimum-down window outright, proxy audit extends the
// ladder rung it computed, so a longer backoff someone else placed survives).
func setReapedProxyHold(key string, until time.Time) {
	if key == "" {
		return
	}
	reapedProxyHold.Lock()
	if reapedProxyHold.until == nil {
		reapedProxyHold.until = map[string]time.Time{}
	}
	if current, ok := reapedProxyHold.until[key]; !ok || until.After(current) {
		reapedProxyHold.until[key] = until
	}
	reapedProxyHold.Unlock()
}

// holdReapedProxy records that key was cancelled for under-performing and may
// not be re-added before until, and mirrors the window into the give-up history
// so the existing URL-sourced gate (dropBackoffHeldURLProxies) honors it too.
func holdReapedProxy(key string, until time.Time) {
	setReapedProxyHold(key, until)
	if key == "" {
		return
	}
	// URL-sourced identities are held out by dropBackoffHeldURLProxies, curated
	// ones by filterReapHeldProxySettings. The same window in both places keeps
	// the two gates from disagreeing.
	globalProxyFailureHistory.SetBackoffUntil(key, until)
}

// releaseReapedProxyHold drops key's hold early and reports whether it was
// held, so a released identity may rejoin the pool on the next desired-set
// build. The failure-history backoff is left alone: the caller releases exactly
// the window it owns (proxy audit's takeOwnBackoff), and a longer one someone
// else placed stays.
func releaseReapedProxyHold(key string) bool {
	reapedProxyHold.Lock()
	defer reapedProxyHold.Unlock()
	if _, ok := reapedProxyHold.until[key]; !ok {
		return false
	}
	delete(reapedProxyHold.until, key)
	return true
}

// reapedProxyHoldActive reports whether key is still inside its hold window.
func reapedProxyHoldActive(key string, now time.Time) bool {
	reapedProxyHold.Lock()
	defer reapedProxyHold.Unlock()
	until, ok := reapedProxyHold.until[key]
	return ok && now.Before(until)
}

// releaseExpiredReapedProxyHolds drops the holds whose window has elapsed and
// reports how many. The identity is then eligible again on the next desired-set
// build; the reaper also reloads on the same tick so it comes back without
// waiting for an unrelated trigger.
func releaseExpiredReapedProxyHolds(now time.Time) []string {
	var released []string
	reapedProxyHold.Lock()
	for key, until := range reapedProxyHold.until {
		if !now.Before(until) {
			delete(reapedProxyHold.until, key)
			released = append(released, key)
		}
	}
	reapedProxyHold.Unlock()

	sort.Strings(released)
	// Reset the down clock here, not in the caller: every release path has to
	// clear it, and a released identity that kept its old DownSince is ranked
	// long-down by the next cycle and reaped straight back.
	resetLiveProxyHealthDownClock(released, now)
	return released
}

// resetReapedProxyHoldsForTest clears the hold. Tests only.
func resetReapedProxyHoldsForTest() {
	reapedProxyHold.Lock()
	defer reapedProxyHold.Unlock()
	reapedProxyHold.until = map[string]time.Time{}
}

// filterReapHeldProxySettings drops from a desired set the identities the
// quality reaper cancelled and is still holding out. It is applied where the
// live reloader computes what it should be serving, so the hold governs the
// running pool only: `proxy list` and the other command readers keep reporting
// the operator's configured set, which is still what proxy.state says.
func filterReapHeldProxySettings(settings []*connect.ProxySettings, now time.Time) []*connect.ProxySettings {
	if len(settings) == 0 {
		return settings
	}
	reapedProxyHold.Lock()
	held := make(map[string]time.Time, len(reapedProxyHold.until))
	for key, until := range reapedProxyHold.until {
		if now.Before(until) {
			held[key] = until
		}
	}
	reapedProxyHold.Unlock()
	if len(held) == 0 {
		return settings
	}
	out := make([]*connect.ProxySettings, 0, len(settings))
	for _, settingsAt := range settings {
		if settingsAt != nil {
			if _, ok := held[settingsAt.Key()]; ok {
				continue
			}
		}
		out = append(out, settingsAt)
	}
	return out
}

// ---- the reaper ----

// minerDegradedReaper culls alive-but-under-performing proxies from the live
// running set.
type minerDegradedReaper struct {
	// reloader owns the running slots; the reaper cancels through it and never
	// touches a slot it does not own.
	reloader *minerProxyReloader

	// degraded returns the current degraded set, as of now. Defaults to the
	// live health registry (proxyLiveDegradedProxies).
	degraded func(now time.Time) []minerDegradedProxyEntry

	// contribution returns an identity's lifetime contribution. Defaults to the
	// earnings store (liveMinerProxyContribution).
	contribution minerProxyContributionFunc

	// stillDegraded re-verifies one identity immediately before the cancel. A
	// snapshot taken a tick earlier can be stale: a proxy that recovered in
	// between must not be cancelled for the state it was in.
	stillDegraded func(key string) bool

	// frozen reports a platform outage. Defaults to the miner's auth-stream
	// detector (proxyAuthOutageFrozen).
	frozen func() bool

	// now is the clock, for tests.
	now func() time.Time
}

// newMinerDegradedReaper builds the reaper for one run.
func newMinerDegradedReaper(reloader *minerProxyReloader) *minerDegradedReaper {
	return &minerDegradedReaper{
		reloader:      reloader,
		degraded:      minerDegradedProxies,
		contribution:  liveMinerProxyContribution,
		stillDegraded: minerProxyStillDegraded,
		frozen:        proxyAuthOutageFrozen,
		now:           time.Now,
	}
}

// minerProxyStillDegraded reports whether an identity is degraded in the live
// health registry right now. The provider's equivalent is liveIsDegraded over
// its per-index registry.
func minerProxyStillDegraded(key string) bool {
	proxyLiveHealthRegistry.Lock()
	defer proxyLiveHealthRegistry.Unlock()
	entry, ok := proxyLiveHealthRegistry.byKey[key]
	if !ok {
		return false
	}
	return proxyHealthBucket(entry.Health) == "degraded" && !entry.DownSince.IsZero()
}

// minerDegradedReapResult reports what one pass did, for the operator log and
// for the tests.
type minerDegradedReapResult struct {
	// Considered is the degraded set the health registry reported.
	Considered int
	// Cancellable is how many of those this run is actually running.
	Cancellable int
	// Keep is the size of the degraded set the pass decided to keep.
	Keep int
	// Reaped is how many identities were cancelled.
	Reaped int
	// Released is how many holds expired and were reloaded back in.
	Released int
	// Frozen/NoRanking record which guard stopped the pass.
	Frozen    bool
	NoRanking bool
}

// cycle runs one reap pass. It is the whole of the provider's ticker body,
// separated from the ticker so tests drive it directly.
func (r *minerDegradedReaper) cycle() minerDegradedReapResult {
	var result minerDegradedReapResult
	if r.reloader == nil {
		return result
	}
	if r.stillDegraded == nil {
		r.stillDegraded = minerProxyStillDegraded
	}
	now := r.now()

	// Holds that have elapsed are released, and the desired set is re-applied:
	// nothing else recomputes it on a clock, so without this a reaped identity
	// would only come back when an unrelated trigger fired.
	if released := releaseExpiredReapedProxyHolds(now); len(released) > 0 {
		result.Released = len(released)
		if _, err := r.reloader.reload(); err != nil {
			tlog("[proxy][quality] could not re-apply the desired set after %s came off hold: %v\n",
				plural(len(released), "reaped proxy", "reaped proxies"), err)
		} else {
			tlog("[proxy][quality] %s came off hold and may rejoin the pool\n",
				plural(len(released), "reaped proxy", "reaped proxies"))
		}
	}

	// The provider's reaper has no outage lever of its own: its tie detection
	// (minerScoresCarryNoRanking) is what stops it while the platform is
	// unreachable. The miner has an explicit detector fed by the auth stream,
	// and the quality evidence here is traffic — which also freezes during an
	// outage. Using it is the same "one lever per evidence source" rule the
	// URL reaper follows (proxy_outage_freeze.go): a platform that answers
	// nobody must not cost the operator half the pool.
	if r.frozen != nil && r.frozen() {
		result.Frozen = true
		return result
	}

	degraded := r.degraded(now)
	result.Considered = len(degraded)
	if len(degraded) <= 1 {
		return result
	}

	// Only identities this run is actually serving can be cancelled. The health
	// registry outlives slots (a proxy that stopped is still recorded), so this
	// is the miner's onlyCancellableProxies.
	running := map[string]struct{}{}
	for _, key := range r.reloader.runningKeys() {
		running[key] = struct{}{}
	}
	cancellable := make([]minerDegradedProxyEntry, 0, len(degraded))
	for _, entry := range degraded {
		if _, ok := running[entry.Key]; ok {
			cancellable = append(cancellable, entry)
		}
	}
	result.Cancellable = len(cancellable)
	if len(cancellable) <= 1 {
		return result
	}

	scored := scoreMinerDegradedProxies(cancellable, r.contribution)
	if minerScoresCarryNoRanking(scored) {
		result.NoRanking = true
		return result
	}

	keep := degradedReaperKeepCount(len(scored))
	result.Keep = keep
	toReap := selectMinerProxiesToReap(scored, keep, degradedReaperMinDownTime)
	if len(toReap) == 0 {
		return result
	}

	// Re-verify each candidate right before pulling the trigger, as the
	// provider's reapProxies does: the decision was made from a snapshot, and a
	// proxy that recovered since must not be cancelled for it.
	keys := make([]string, 0, len(toReap))
	for _, entry := range toReap {
		if !r.stillDegraded(entry.Key) {
			continue
		}
		keys = append(keys, entry.Key)
	}
	if len(keys) == 0 {
		return result
	}

	reaped := r.reloader.reap(keys)
	if len(reaped) == 0 {
		return result
	}
	// Hold each identity that was really cancelled. The window is the same
	// degradedReaperMinDownTime the candidate had to clear: a proxy that had to
	// be down half an hour to be reaped stays out half an hour before the
	// desired-set build will offer it again, so the next reload cannot undo the
	// decision on the tick that made it.
	until := now.Add(degradedReaperMinDownTime)
	for _, key := range reaped {
		holdReapedProxy(key, until)
	}
	result.Reaped = len(reaped)

	tlog("[proxy][quality] dropped %s under-performing (kept best %d of %d, held out until %s)\n",
		plural(result.Reaped, "proxy", "proxies"), keep, len(scored), until.Format(time.RFC3339))
	return result
}

// run is the reaper loop: one pass per degradedReaperTicker until ctx ends.
//
// It is started only when the run installed a reloader, and it stops with the
// run: the reloader's slots are the only thing it can cancel, so a run without
// one has nothing to reap.
func (r *minerDegradedReaper) run(ctx context.Context) {
	if r == nil || r.reloader == nil {
		return
	}
	ticker := time.NewTicker(degradedReaperTicker)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		r.cycle()
	}
}
