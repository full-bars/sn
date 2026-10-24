package miner

// Ported from the 3.23-fix v33 provider's proxy_audit_run.go (read-only source
// candidate/v33-ops), including the manager wiring shape: a 5 minute ticker
// loop, the live env behind function fields, and the control-socket release
// helpers.
//
// Port adaptations (the pinned full-bars/connect does not export the v33
// connect registries):
//
//   - the LIVE health registry is the fork-local one (proxyLiveHealthSnapshot,
//     proxy_manager_health_registry.go);
//   - the per-proxy client count comes from the bandwidth counters the provide
//     loop already publishes per identity (proxyEarnBandwidthRegistry,
//     proxy_manager_earnings_loop.go), which is the miner's stand-in for v33's
//     connect.ProxyBandwidthByKey(addr).Clients;
//   - park acts through the live reloader (proxy_reloader.go), which owns the
//     running slots: cancelling a slot is only possible where the slot is
//     started, and the manager starts before the provide loop builds the
//     reloader, so the seam resolves it per tick (proxy_manager_audit_park.go).
//     A park records the degraded reaper's hold so the reloader will not put the
//     identity straight back, and a release drops that hold and re-applies the
//     desired set, which is what lets an unparked proxy return;
//   - the earnings score and the recency signal are the real store
//     (proxy_manager_earnings_store.go) and tracker
//     (proxy_manager_earn_tracker.go);
//   - the audit-enabled resolution keeps the in-memory override, the control
//     state stand-in (globalProxyControlState, proxy_manager_url_source.go),
//     the override file and the startup value, in v33's order.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"

	"github.com/urnetwork/connect"
)

// parkProxies cancels each address in toPark that is still eligible at the
// moment of the cancel, and returns the addresses it actually parked.
//
// It follows reapProxies: the candidate list came from a snapshot that is
// stale by now (the proxy may have picked up sessions or been relaunched), so
// stillEligible is re-checked under the SAME lock as the cancel and delete so
// nothing can change between the check and the act.
//
// The cancel-map entry MUST be deleted. A paid proxy's goroutine does not
// remove its own entry on exit, so cancelling without deleting would leave the
// address looking "running" forever and reload would never relaunch it.
// CancelFunc is idempotent, so a second canceller (reaper, trim, drain) is
// harmless.
//
// onParked, when set, runs for each parked address while the lock is still
// held. The caller puts the backoff there: a reload that runs between the
// cancel and the backoff would see the address not running and eligible, and
// relaunch what was just parked. It must not take proxyStateMu or the
// cross-process proxy lock, which stay outside the cancel lock.
//
// This is the provider's map shape, kept as the audit's map seam (the tick
// tests park through it). The live park goes through the reloader, which owns
// the running slots and has to see the cancel to keep its own bookkeeping
// truthful — minerProxyReloader.cancelSlots, reached via liveProxyAuditPark
// (proxy_manager_audit_park.go).
func parkProxies(toPark []string, cancelMap map[string]context.CancelFunc, cancelMu *sync.Mutex, stillEligible func(addr string) bool, onParked func(addr string)) []string {
	var parked []string
	for _, addr := range toPark {
		// One function per address so the lock is released by defer: a panic in
		// a callback must not leave reload and the reaper blocked on it.
		func() {
			cancelMu.Lock()
			defer cancelMu.Unlock()
			if !stillEligible(addr) {
				return
			}
			cancel, ok := cancelMap[addr]
			if !ok {
				return
			}
			cancel()
			delete(cancelMap, addr)
			parked = append(parked, addr)
			if onParked != nil {
				onParked(addr)
			}
		}()
	}
	return parked
}

// proxyAuditEnv is everything proxy audit reads or does in the live process,
// behind function fields so runOnce can be tested with fakes and no registry.
type proxyAuditEnv struct {
	now func() time.Time
	// readPaid returns the tracked paid/file proxies from proxy.state, plus a
	// credential fingerprint per address (see proxyAuditCredFingerprint). ok is
	// false when the paid set cannot be trusted (source file unreadable or
	// empty mid-edit): proxy audit then does nothing this tick.
	readPaid func() (entries map[string]ProxyEntry, creds map[string]string, ok bool)
	// health is the LIVE registry. Running must come from here, never from
	// proxy.state Health, which goes stale after a park.
	health func() map[string]proxyHealthStatus
	// clients is the active client count for a proxy identity; known is false
	// when the bandwidth registry publishes no entry for it (unknown is not
	// idle: a proxy we cannot measure might be carrying sessions).
	clients        func(addr string) (n int64, known bool)
	earningsScore  func(addr string, now time.Time) float64
	earnedRecently func(addr string) bool
	trackerWarm    func() bool
	// act is true when parks should be executed (proxy audit on AND hot restart
	// on). False means observe: log would-park, touch nothing, and give back
	// anything already parked.
	act func() bool
	// notActingReason says why act is false (one of proxyAuditReason values);
	// optional, empty when unknown.
	notActingReason func() string
	markParked      func(addrs []string)
	rand            func() float64
	// failureHistory carries the relaunch backoff the reload path enforces.
	failureHistory *proxyFailureHistory
	// park stops running slots. It is the reloader's cancelSlots
	// (proxy_reloader.go), reached through liveProxyAuditPark
	// (proxy_manager_audit_park.go): the reloader owns the slots, so only it
	// can cancel one. stillEligible re-checks each identity under the cancel
	// lock, and onParked records the park, its backoff and its hold there too,
	// so a reload cannot relaunch the identity between the cancel and the
	// record.
	park func(keys []string, stillEligible func(addr string) bool, onParked func(addr string)) []string
	// unpark drops the holds of identities proxy audit is handing back and
	// re-applies the desired set, so they can rejoin the pool. Nil (or a park
	// that does not use holds) makes releases a no-op.
	unpark func(keys []string)
	log    func(format string, args ...any)
}

// proxyAuditor drives proxyAuditTick against the live process. Not safe for
// concurrent runOnce calls; the ticker loop is its only caller.
type proxyAuditor struct {
	cfg proxyAuditConfig
	st  *proxyAuditState
	env proxyAuditEnv

	// mu serializes runOnce with control-socket audit actions (on, off,
	// release). The ticker loop is the scheduled caller, but the control
	// actions mutate the same state (parks, announced, paused), so they
	// must not run concurrently with a tick.
	mu sync.Mutex

	// announced is the set of proxies already logged as would-park, so observe
	// mode says it once per newly eligible proxy rather than every tick.
	announced map[string]bool
	// degraded remembers the last thin/distrusted note so it logs on change.
	degraded string
	// pausedSince is when the paid proxy list last became untrustworthy; zero
	// while it is trusted. Proxy audit does nothing while paused.
	pausedSince time.Time
}

func newProxyAuditor(cfg proxyAuditConfig, env proxyAuditEnv) *proxyAuditor {
	return &proxyAuditor{cfg: cfg, st: newProxyAuditState(), env: env, announced: map[string]bool{}}
}

// runOnce performs one audit tick, serialized against control actions.
func (g *proxyAuditor) runOnce() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runOnceLocked()
}

// runOnceLocked is the tick body; callers must hold g.mu.
func (g *proxyAuditor) runOnceLocked() {
	now := g.env.now()
	entries, creds, ok := g.env.readPaid()
	act := g.env.act()
	if !act {
		// Observe mode, or hot restart off: hand back everything we took. This
		// comes before the paid-set check so switching self-heal off is honoured
		// even while the paid list is unreadable.
		if released := g.st.releaseAll(); len(released) > 0 {
			for _, addr := range released {
				g.releaseBackoff(addr)
			}
			// Drop the holds too, and re-apply the desired set: a park that is
			// called off must put the proxy back, not leave it out of the pool
			// until its hold lapses.
			g.unpark(released)
			g.env.log("[proxy][audit] released %d parked proxies (audit is not acting)\n", len(released))
		}
	}
	if !ok {
		// The paid list is what proves a proxy is paid-owned, so without it the
		// audit cannot tell what it may park. Do nothing, but say so.
		if g.pausedSince.IsZero() {
			g.pausedSince = now
			g.env.log("[proxy][audit] paused: the paid proxy list is unreadable or empty, so proxy audit cannot tell which proxies it may park; parking nothing until it can\n")
		}
		g.st.keepTime(now)
		g.publish(now, act, proxyAuditResult{})
		return
	}
	if !g.pausedSince.IsZero() {
		g.env.log("[proxy][audit] resumed after %s\n", now.Sub(g.pausedSince).Round(time.Second))
		g.pausedSince = time.Time{}
	}

	health := g.env.health()
	addrs := make([]string, 0, len(entries))
	for addr := range entries {
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs)

	proxies := make([]proxyAuditProxy, 0, len(addrs))
	for _, addr := range addrs {
		h, has := health[addr]
		n, known := g.env.clients(addr)
		proxies = append(proxies, proxyAuditProxy{
			Addr:           addr,
			Entry:          entries[addr],
			Running:        has && h.Health == "up",
			BandwidthKnown: known,
			Idle:           known && n == 0,
			EarningsScore:  g.env.earningsScore(addr, now),
			EarnedRecently: g.env.earnedRecently(addr),
			Cred:           creds[addr],
		})
	}

	res := proxyAuditTick(g.cfg, g.st, proxyAuditInput{
		Now:         now,
		Proxies:     proxies,
		Act:         act,
		TrackerWarm: g.env.trackerWarm(),
	})

	for _, addr := range res.Release {
		g.releaseBackoff(addr)
		if _, listed := entries[addr]; listed {
			g.env.log("[proxy][audit] restored %s\n", proxyKeyDisplay(addr))
		} else {
			g.env.log("[proxy][audit] released %s (no longer in the paid proxy list)\n", proxyKeyDisplay(addr))
		}
	}
	if len(res.Release) > 0 {
		// The parks are given back: drop their holds and re-apply the desired
		// set once, so the identities return on this tick rather than on the
		// next unrelated reload trigger.
		g.unpark(res.Release)
	}

	note := ""
	switch {
	case res.Distrusted:
		note = "distrusted"
	case res.Thin:
		note = "thin"
	}
	if note != g.degraded {
		g.degraded = note
		switch note {
		case "distrusted":
			g.env.log("[proxy][audit] correlated failure: a large share of fresh grades came back bad at once; treating this pass as a box-level problem and parking nothing\n")
		case "thin":
			g.env.log("[proxy][audit] too few proxies could be evaluated from this box; parking nothing\n")
		}
	}

	if !act {
		g.observe(res, proxies)
	} else {
		g.announced = map[string]bool{}
		g.remarkLost(entries, proxies)
		g.park(now, res, proxies)
	}
	g.publish(now, act, res)
}

func (g *proxyAuditor) observe(res proxyAuditResult, proxies []proxyAuditProxy) {
	score := map[string]float64{}
	for _, p := range proxies {
		score[p.Addr] = p.Entry.Score
	}
	current := map[string]bool{}
	for _, p := range res.Park {
		current[p.Addr] = true
		if !g.announced[p.Addr] {
			g.env.log("[proxy][audit] would-park %s score=%.2f\n", proxyKeyDisplay(p.Addr), score[p.Addr])
		}
	}
	g.announced = current
}

// park cancels the proxies the tick selected, through the live park seam (the
// reloader). A proxy that cannot be cancelled through it is not parked and is
// not recorded: the audit only claims what it actually did.
func (g *proxyAuditor) park(now time.Time, res proxyAuditResult, proxies []proxyAuditProxy) {
	if len(res.Park) == 0 || g.env.park == nil {
		return
	}
	score := map[string]float64{}
	for _, p := range proxies {
		score[p.Addr] = p.Entry.Score
	}
	toPark := make([]string, 0, len(res.Park))
	for _, p := range res.Park {
		// The direct slot is not a proxy: it carries no paid entry and no
		// identity counters, and stopping it would take the box's own egress
		// down. It can only reach this list through a corrupt paid set, so it
		// is filtered rather than trusted.
		if p.Addr == directProxyKey {
			continue
		}
		toPark = append(toPark, p.Addr)
	}
	if len(toPark) == 0 {
		return
	}
	until := map[string]time.Time{}
	parked := g.env.park(toPark, g.stillEligible, func(addr string) {
		u := g.st.commitPark(g.cfg, addr, now, g.env.rand())
		g.env.failureHistory.ExtendBackoffUntil(addr, u)
		// The hold is what keeps the reloader from re-adding the identity on
		// its next desired-set build: proxy.state still lists it, because a
		// park rests a proxy instead of removing it. It is recorded here, under
		// the reloader's cancel lock, so a reload cannot slip in between the
		// cancel and the hold.
		setReapedProxyHold(addr, u)
		until[addr] = u
	})
	for _, addr := range parked {
		g.env.log("[proxy][audit] parked %s score=%.2f until=%s\n", proxyKeyDisplay(addr), score[addr], until[addr].Format(time.RFC3339))
	}
	if len(parked) > 0 {
		g.env.markParked(parked)
	}
}

// unpark hands the named identities back: the holds that kept them out of the
// desired set are dropped and the desired set is re-applied, so they rejoin the
// pool now instead of when the hold happens to lapse.
func (g *proxyAuditor) unpark(keys []string) {
	if g.env.unpark == nil || len(keys) == 0 {
		return
	}
	g.env.unpark(keys)
}

// remarkLost re-asserts the parked mark on parks from earlier ticks whose
// proxy.state Health no longer says parked. The mark is best effort (the proxy
// lock can be busy) and the heartbeat writes a still-registered proxy back as
// up until its old goroutine unregisters, so a single mark is not enough. It
// reads the snapshot the tick already took and only writes when something is
// wrong, since every mark takes the cross-process proxy lock.
func (g *proxyAuditor) remarkLost(entries map[string]ProxyEntry, proxies []proxyAuditProxy) {
	running := make(map[string]bool, len(proxies))
	for _, p := range proxies {
		running[p.Addr] = p.Running
	}
	var lost []string
	for _, addr := range g.st.parkedAddrs() {
		// A running proxy is not parked, whatever the record says: something
		// relaunched it, and marking it parked would make proxy.state undercount
		// what is running. (Right after a park the old goroutine can still read
		// as up for a moment; the next tick re-marks it.)
		if running[addr] {
			continue
		}
		if e, ok := entries[addr]; ok && e.Health != proxyHealthParked {
			lost = append(lost, addr)
		}
	}
	if len(lost) > 0 {
		g.env.markParked(lost)
	}
}

// releaseBackoff gives an address back by clearing the backoff proxy audit set
// for it, and nothing else. A longer backoff someone else placed on the address
// (trim shed, URL give-up) stays.
func (g *proxyAuditor) releaseBackoff(addr string) {
	if until, ok := g.st.takeOwnBackoff(addr); ok {
		g.env.failureHistory.ReleaseBackoff(addr, until)
	}
}

// stillEligible re-checks, at the moment of the cancel and under the cancel
// lock, the vetoes that could have changed since the snapshot: the proxy must
// still be up, its bandwidth known and idle, and it must not have earned.
func (g *proxyAuditor) stillEligible(addr string) bool {
	if h, ok := g.env.health()[addr]; !ok || h.Health != "up" {
		return false
	}
	if n, known := g.env.clients(addr); !known || n != 0 {
		return false
	}
	return !g.env.earnedRecently(addr)
}

// proxyAuditTickInterval is how often proxy audit evaluates. Grades only change
// every few hours, so this mostly costs a cheap snapshot; it stays short so a
// self-heal toggle is honoured within minutes.
const proxyAuditTickInterval = 5 * time.Minute

// proxyAuditActEnabled reports whether proxy audit should execute parks: the
// proxy audit switch (live via `urnet-tools proxy audit on|off`, default
// off) AND hot restart. With hot restart off every relaunch mints a fresh
// client identity, so parking would churn identities instead of resting a
// proxy. When false proxy audit only observes.
func proxyAuditActEnabled(startupProxyAudit bool) bool {
	return resolveProxyAuditEnabled(startupProxyAudit) && hotRestartEnabled()
}

// hotRestartEnabled is v33's hot-restart switch (main.go hotRestartEnabled:
// the "hot_restart" control key, then URNETWORK_HOT_RESTART, default on).
// The resolution lives beside the control socket (resolveHotRestartEnabled,
// control_socket.go); this indirection keeps the value overridable in
// tests that exercise the audit's acting/observing split.
var hotRestartEnabled = resolveHotRestartEnabled

// The two reasons proxy audit can be observing only. They are machine-stable
// strings carried on the control socket; `urnet-tools` turns them into advice.
const (
	auditReasonAuditOff      = "audit-off"
	auditReasonHotRestartOff = "hot-restart-off"
)

// proxyAuditNotActingReason says why proxyAuditActEnabled is false, or "" when the
// audit acts. The fixes differ (proxy audit on, or hot-restart on), so the
// status has to name the one that applies.
func proxyAuditNotActingReason(startupProxyAudit bool) string {
	switch {
	case !resolveProxyAuditEnabled(startupProxyAudit):
		return auditReasonAuditOff
	case !hotRestartEnabled():
		return auditReasonHotRestartOff
	}
	return ""
}

// proxyAuditTrackerWarm is true once the in-memory earn tracker has watched at
// least two earn windows. Right after a restart it knows nothing, so "no
// recent earnings" would be meaningless.
func proxyAuditTrackerWarm(start, now time.Time) bool {
	return now.Sub(start) >= 2*paidEarnWindow
}

// liveReadPaid returns the tracked paid/file proxies from proxy.state. ok is
// false when the paid set cannot be trusted (state unreadable, or a source
// file that is unreadable or empty mid-edit), so the caller does nothing.
func liveReadPaid() (map[string]ProxyEntry, map[string]string, bool) {
	proxyStateMu.Lock()
	defer proxyStateMu.Unlock()
	state, err := readProxyState()
	if err != nil {
		return nil, nil, false
	}
	paid, fileOK := paidDesiredSet(state)
	if !fileOK {
		return nil, nil, false
	}
	out := make(map[string]ProxyEntry, len(paid))
	creds := make(map[string]string, len(paid))
	// state.Proxies and paid are both keyed by proxy identity (address, or
	// address+user for a credentialed proxy), so the maps line up as-is.
	for key, entry := range state.Proxies {
		if s, ok := paid[key]; ok {
			out[key] = entry
			creds[key] = proxyAuditCredFingerprint(s.Auth)
		}
	}
	return out, creds, true
}

// proxyAuditCredFingerprint is an opaque, stable stand-in for a proxy's
// credentials, taken from the same desired set the grader validates its
// results against. A proxy's identity is its address plus user, so re-pasting
// the same host:port and user with a new password (the LA7 incident,
// 2026-09-18) is a rotation proxy audit must notice: what it learned about the
// old credentials says nothing about the new ones. Near-identical
// endpoints (same host, another port, or another user) are different
// identities and never share state.
//
// Length-prefixing keeps ("ab","c") and ("a","bc") apart, and the hash keeps
// the secret out of every log line and status output. No credentials, or
// empty ones, fingerprint as "".
func proxyAuditCredFingerprint(a *proxy.Auth) string {
	if a == nil || (a.User == "" && a.Password == "") {
		return ""
	}
	sum := sha256.Sum256([]byte(strconv.Itoa(len(a.User)) + ":" + a.User + ":" + a.Password))
	return hex.EncodeToString(sum[:8])
}

// markProxiesParked sets Health to proxyHealthParked on the named tracked
// entries in proxy.state, leaving grade fields alone. The heartbeat writer only
// updates entries present in the live registry, so without a mark a parked
// proxy would read "up" forever and be counted as running and ranked shed-last
// by trim. The value is deliberately NOT "inactive": that means "unseen 7+
// days, safe to remove", and dead-proxy cleanup and `proxy remove-dead` both
// collect it and edit the operator's proxy file. A parked proxy is resting,
// not dead, and must survive both. Takes the cross-process lock and
// proxyStateMu, like the heartbeat writer.
func markProxiesParked(addrs []string) {
	if len(addrs) == 0 {
		return
	}
	release, err := acquireProxyLockWithRetry()
	if err != nil {
		tlog("[proxy][audit] warn: could not mark parked proxies: %v\n", err)
		return
	}
	defer release()
	proxyStateMu.Lock()
	defer proxyStateMu.Unlock()
	state, err := readProxyState()
	if err != nil {
		tlog("[proxy][audit] warn: could not read proxy.state to mark parked proxies: %v\n", err)
		return
	}
	for _, addr := range addrs {
		if entry, ok := state.Proxies[addr]; ok {
			entry.Health = proxyHealthParked
			state.Proxies[addr] = entry
		}
	}
	if err := writeProxyState(state); err != nil {
		tlog("[proxy][audit] warn: state write failed: %v\n", err)
	}
}

// proxyAuditClients is the live client count per proxy identity. v33 reads
// connect.ProxyBandwidthByKey(addr).Clients; the pinned full-bars/connect does
// not export the bandwidth registry, so the miner reads the counters its own
// provide loop publishes per identity (proxyEarnBandwidthRegistry), which the
// earnings recorder folds in from the same objects. Unknown (no entry) is not
// idle: the proxy's slot is not running, or it is the direct slot, which is not
// a proxy and carries no identity key, so the audit vetoes the park.
func proxyAuditClients(addr string) (int64, bool) {
	if addr == directProxyKey {
		return 0, false
	}
	bw := proxyEarnBandwidthFor(addr)
	if bw == nil {
		return 0, false
	}
	return bw.Clients.Load(), true
}

// newLiveProxyAuditor wires a proxyAuditor to the real registries. start is
// the process start, used both as the memory boundary (grades stamped earlier
// are not evidence) and for the tracker warm-up.
func newLiveProxyAuditor(startupProxyAudit bool, start time.Time) *proxyAuditor {
	g := newProxyAuditor(defaultProxyAuditConfig(), proxyAuditEnv{
		now:      time.Now,
		readPaid: liveReadPaid,
		health:   proxyLiveHealthSnapshot,
		clients:  proxyAuditClients,
		earningsScore: func(addr string, now time.Time) float64 {
			return proxyEarningsScoreFor(addr, now)
		},
		earnedRecently: func(addr string) bool {
			return globalPerProxyEarnTracker.EarnedSince(addr, paidEarnWindow)
		},
		trackerWarm:     func() bool { return proxyAuditTrackerWarm(start, time.Now()) },
		act:             func() bool { return proxyAuditActEnabled(startupProxyAudit) },
		notActingReason: func() string { return proxyAuditNotActingReason(startupProxyAudit) },
		markParked:      markProxiesParked,
		rand:            rand.Float64,
		failureHistory:  globalProxyFailureHistory,
		park:            liveProxyAuditPark,
		unpark:          liveProxyAuditUnpark,
		log:             tlog,
	})
	g.st.notBefore = start
	return g
}

// runProxyAudit is proxy audit's ticker loop.
func runProxyAudit(ctx context.Context, startupProxyAudit bool) {
	g := newLiveProxyAuditor(startupProxyAudit, time.Now())
	currentProxyAuditor.Store(g)
	defer currentProxyAuditor.Store(nil)
	// Say whether proxy audit will act right away: the first tick is minutes
	// off, and until then /metrics would read "not acting" on every restart.
	g.publish(g.env.now(), g.env.act(), proxyAuditResult{})
	ticker := time.NewTicker(proxyAuditTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		g.runOnce()
		// The audit tick is the miner's existing periodic proxy loop, so the
		// systemd STATUS line rides it instead of starting a loop of its own.
		// Park/pause changes land in the audit's publish; auth transitions
		// refresh the live half immediately (proxy_manager_auth.go).
		refreshProxySystemdStatus()
	}
}

var currentProxyAuditor atomic.Pointer[proxyAuditor]
var proxyAuditOverride atomic.Pointer[bool]

// runOnceGate bounds the immediate tick spawned after a control toggle: the
// ticker is the only steady-state caller of runOnce, and the toggle path
// must not be able to pile up unbounded goroutines on a busy node.
var runOnceGate = make(chan struct{}, 1)

// spawnRunOnce runs one immediate tick after a control-socket toggle, but
// only if no such tick is already in flight. The next ticker pass picks up
// anything skipped, so dropping a redundant spawn is safe.
func spawnRunOnce(a *proxyAuditor) {
	select {
	case runOnceGate <- struct{}{}:
		go func() {
			defer func() { <-runOnceGate }()
			a.runOnce()
		}()
	default:
	}
}

// setProxyAuditOverride sets an in-memory runtime override for whether proxy audit is enabled.
func setProxyAuditOverride(enabled bool) {
	proxyAuditOverride.Store(&enabled)
}

// clearProxyAuditOverride clears any in-memory runtime override.
func clearProxyAuditOverride() {
	proxyAuditOverride.Store(nil)
}

// proxyAuditOverridePath returns <state>/proxy_audit (v33: ~/.urnetwork/
// proxy_audit; the miner resolves the same file through URNETWORK_STATE_DIR).
func proxyAuditOverridePath() (string, error) {
	return providerStatePath("proxy_audit")
}

// resolveProxyAuditEnabled reports whether proxy audit is active, checking
// the in-memory override first, then the control-socket state ("proxy_audit"),
// then the override file (<state>/proxy_audit), then startupEnabled
// (from URNETWORK_PROXY_AUDIT).
func resolveProxyAuditEnabled(startupEnabled bool) bool {
	if ov := proxyAuditOverride.Load(); ov != nil {
		return *ov
	}
	if v, ok := globalProxyControlState.get("proxy_audit"); ok {
		return strings.EqualFold(v, "on") || v == "1" || strings.EqualFold(v, "true")
	}
	// Then the persisted control state the socket writes (provider_state.json,
	// loaded at startup). v33 resolves the switch from this state, so an
	// `audit on` survives the restart that drops the live override and the
	// in-process map; without this read the miner fell through to the marker
	// file and the startup default and the persisted value was forgotten.
	if v, ok := globalControlState.get("proxy_audit"); ok {
		return strings.EqualFold(v, "on") || v == "1" || strings.EqualFold(v, "true")
	}
	path, err := proxyAuditOverridePath()
	if err == nil {
		if data, err := os.ReadFile(path); err == nil {
			trimmed := strings.TrimSpace(string(data))
			if trimmed != "" {
				return strings.EqualFold(trimmed, "on") || trimmed == "1" || strings.EqualFold(trimmed, "true")
			}
		}
	}
	return startupEnabled
}

// proxyAuditStartupEnabled resolves the startup value the audit runner is
// seeded with (v33 main.go: resolveProxyAuditEnabled(os.Getenv("URNETWORK_PROXY_AUDIT") == "1")).
func proxyAuditStartupEnabled() bool {
	return resolveProxyAuditEnabled(os.Getenv("URNETWORK_PROXY_AUDIT") == "1")
}

// resolveAuditReleaseKeys maps what an operator typed for `proxy audit release`
// to the keys audit state is stored under. Parks, backoffs and failure counts
// are keyed by proxy identity (address, or address+user for a credentialed
// proxy), and the \x1f separator makes an identity key impossible to type. So
// an address names EVERY known identity at it (release only gives proxies back,
// which is safe to do in bulk), and an exact key names just itself. When nothing
// known matches, the input is kept so a stray backoff or failure count recorded
// under exactly that string is still cleared. Result is sorted and unique.
func resolveAuditReleaseKeys(input string, known []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range known {
		if seen[k] {
			continue
		}
		if address, _ := connect.SplitProxyKey(k); k == input || address == input {
			seen[k] = true
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return []string{input}
	}
	sort.Strings(out)
	return out
}
