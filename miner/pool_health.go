package miner

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// pool_health.go is the miner's counterpart to provider/pool_health.go, and
// carries the aggregate half of provider/health_heartbeat.go's proxy block.
//
// The provider's file watches a MESSAGE-buffer pool: it tracks the idle floor of
// in-use buffers over a trailing hour and escalates warming -> ok -> watch ->
// leak as that floor climbs, which is how a buffer that is taken and never given
// back is caught without watching a return percentage that stays pinned near
// 100% while the leak grows. The miner runs no message pool of its own (the
// pinned connect allocates those buffers), so a literal port would be dead code
// here: in this fork the provider's own [health][pool] line is skipped for the
// same reason, because messagePoolSummary() returns nil in v2026.
//
// What does transfer is the accounting shape, and the miner needs it for the
// pool it does have: the proxy pool. The miner grades members one at a time
// (proxy_manager_health_registry.go) and reports them one at a time, so a pool
// where every proxy is failing reads exactly like a pool with a few bad proxies
// once the per-member lines scroll past. provider/health_heartbeat.go already
// answers that for its own pool with a percentage threshold and a
// plain-sentence Verdict ladder; this file brings that ladder to the miner and
// adds the one axis the threshold cannot express: whether the trouble is the
// POOL (members failing as a unit, which is a network or host problem) or
// ISOLATED (a minority down while the rest carry traffic, which is bad
// proxies).
//
// Two facts carry the aggregate:
//
//   - Scope, from the current cross-section: when half or more of the pool is
//     down at once, no single proxy can be blamed for it.
//   - Floor, from the trailing hour: the lowest up-count seen. A pool that dips
//     to zero and recovers reads healthy on every per-member line, and on every
//     scrape taken after it; only the floor remembers.
//
// Member state comes from where the existing per-member report gets it —
// proxy.state with the live registry overlaid — so this file adds no registry
// and no counters. The counters that mean the same thing are reused verbatim:
// up/degraded/dead/connecting are the `status` label values urnet_proxy_pool_size
// already publishes (metrics_traffic.go), and the verdict strings are
// provider/health_heartbeat.go's ladder word for word, so one dashboard and one
// grep read both processes.

// poolVerdict* are provider/health_heartbeat.go's Verdict ladder word for word.
// "healthy but idle" is the one rung this file does not produce: it needs a
// right-now serving count, which belongs to the traffic path ([earn]
// proxies_up= serving= idle=), not to pool composition.
const (
	poolVerdictStartingUp     = "starting up"
	poolVerdictHealthy        = "healthy"
	poolVerdictMostlyHealthy  = "mostly healthy"
	poolVerdictNeedsAttention = "needs attention"
	poolVerdictDown           = "down"
)

// poolScope* answers "how much of the pool is this?" — the axis the provider's
// percentage threshold cannot express, because one bad proxy out of five and
// five bad proxies out of five both clear it.
const (
	poolScopeNone     = "none"
	poolScopeIsolated = "isolated"
	poolScopePool     = "pool"
)

// poolAttentionDownPct is the provider's healthVerdictAttentionDownPct: the
// share of the pool that may be down before the verdict starts with "needs
// attention".
const poolAttentionDownPct = 10

// poolFloorWindowDuration is how far back the up-floor looks. One hour, like the
// provider's idle floor: long enough that any real traffic or network pattern
// shows its true low point at least once inside it.
const poolFloorWindowDuration = time.Hour

// proxyPoolHealthInterval is the tick this window reasons in: the miner's health
// cadence (proxyHealthLogInterval). A caller that asks for a reading inside the
// current tick gets the previous one back, so a dashboard scraped every 5s can
// neither shrink the window nor manufacture ticks.
const proxyPoolHealthInterval = proxyHealthLogInterval

// The bucket names are exactly the `status` label values urnet_proxy_pool_size
// publishes (metrics_traffic.go).
const (
	proxyPoolHealthUp         = "up"
	proxyPoolHealthDegraded   = "degraded"
	proxyPoolHealthDead       = "dead"
	proxyPoolHealthConnecting = "connecting"
)

// proxyPoolMemberBucket classifies one member's persisted-or-live health the way
// snapshotProxyCounts always has, and is the one classifier this file and that
// function share, so the aggregate and the composition metric can never disagree
// about a member.
//
// An empty health is "connecting", not "dead": nothing has observed this member
// yet, and a member that simply has not been tried cannot be counted against the
// pool. The v33 report classifier (proxyHealthBucket) maps "" to dead instead;
// that is a rule for a report that only covers members it has already seen, and
// the two must not be merged.
func proxyPoolMemberBucket(health string) string {
	switch health {
	case "up":
		return proxyPoolHealthUp
	case "dead", "inactive":
		return proxyPoolHealthDead
	case "recently_offline", "offline", "long_offline", "parked":
		return proxyPoolHealthDegraded
	default:
		return proxyPoolHealthConnecting
	}
}

// proxyPoolMemberCounts is the cross-section the verdict is computed from.
type proxyPoolMemberCounts struct {
	Up         int
	Degraded   int
	Dead       int
	Connecting int
}

func (c proxyPoolMemberCounts) members() int {
	return c.Up + c.Degraded + c.Dead + c.Connecting
}

func (c proxyPoolMemberCounts) down() int { return c.Degraded + c.Dead }

func countProxyPoolMembers(members map[string]string) proxyPoolMemberCounts {
	var counts proxyPoolMemberCounts
	for _, bucket := range members {
		switch bucket {
		case proxyPoolHealthUp:
			counts.Up++
		case proxyPoolHealthDegraded:
			counts.Degraded++
		case proxyPoolHealthDead:
			counts.Dead++
		default:
			counts.Connecting++
		}
	}
	return counts
}

// proxyPoolMembers returns each member's bucket keyed by identity. This is the
// same composition snapshotProxyCounts counts: proxy.state is the member list and
// the live registry overrides its health, because a signal observed in this
// process is fresher than the file (the precedence seedLiveProxyHealthFromState
// uses).
//
// It deliberately takes no extra identities from the registry. The registry can
// still hold an entry for an identity the state no longer lists, and admitting
// one here would make the aggregate disagree with the composition metric it is
// published beside.
//
// The returned string is the state read error, empty when the file was read. On
// error the live registry is used alone, because a process that cannot read its
// own state file is better off reporting what it has observed than reporting an
// empty pool; the reading carries the error so the line can say the composition
// may be missing members.
func proxyPoolMembers() (map[string]string, string) {
	members := map[string]string{}
	live := liveProxyHealthByKey()
	state, err := readProxyState()
	if err != nil {
		for key, status := range live {
			if status.Health == "" {
				continue
			}
			members[key] = proxyPoolMemberBucket(status.Health)
		}
		return members, err.Error()
	}
	for key, entry := range state.Proxies {
		health := entry.Health
		if status, ok := live[key]; ok && status.Health != "" {
			health = status.Health
		}
		members[key] = proxyPoolMemberBucket(health)
	}
	return members, ""
}

// proxyPoolComposition is one read of the pool's member list: proxy.state with
// the live registry overlaid, counted by the one classifier both this file and
// snapshotProxyCounts use. The composition metric and the aggregate are built
// from the same value, so a scrape cannot publish a composition and a verdict
// taken from different reads, and a scrape performs one proxy.state read rather
// than one per consumer.
type proxyPoolComposition struct {
	Counts   proxyPoolMemberCounts
	StateErr string
}

// readProxyPoolComposition performs the single proxy.state read a caller needs
// for both the composition and the aggregate.
func readProxyPoolComposition() proxyPoolComposition {
	members, stateErr := proxyPoolMembers()
	return proxyPoolComposition{
		Counts:   countProxyPoolMembers(members),
		StateErr: stateErr,
	}
}

// proxyPoolSample is one tick's cross-section. The ring holds these rather than
// provider/pool_health.go's two parallel rings (in-use and floor): there the
// floor is independent of the in-use history, here it is derived from the same
// samples, and one ring cannot drift from the other.
type proxyPoolSample struct {
	members int
	up      int
	down    int
}

// proxyPoolHealthWindow turns the member counts into a reading an operator can
// act on without opening a single per-member line.
type proxyPoolHealthWindow struct {
	// window is how many ticks cover poolFloorWindowDuration.
	window  int
	samples []proxyPoolSample
}

// newProxyPoolHealthWindow sizes the ring from the tick interval, so a caller
// that ticks every 10 minutes still reasons over one hour rather than over six
// ticks (the same conversion provider/pool_health.go does).
func newProxyPoolHealthWindow(interval time.Duration) *proxyPoolHealthWindow {
	if interval <= 0 {
		interval = proxyPoolHealthInterval
	}
	ticks := int((poolFloorWindowDuration + interval - 1) / interval) // ceil
	if ticks < 2 {
		ticks = 2
	}
	return &proxyPoolHealthWindow{window: ticks}
}

// proxyPoolHealthReading is one tick's assessment of the whole pool.
type proxyPoolHealthReading struct {
	Verdict string
	// Scope says whether the down members are the pool failing as a unit or
	// individual proxies failing: poolScopePool, poolScopeIsolated, or
	// poolScopeNone when everything is up.
	Scope string

	Members    int
	Up         int
	Down       int
	Degraded   int
	Dead       int
	Connecting int

	// Floor is the lowest up-count over the window: the worst the pool got,
	// not the worst it is.
	Floor int
	// Dips counts the ticks in the window at which at least one member was
	// down. The current tick says nothing about the last hour; this does.
	Dips int
	// Full is true once the ring holds a complete hour of samples for the
	// CURRENT member count. It bounds how much of the hour Floor and Dips
	// describe; it does not gate the verdict, because the verdict is a
	// statement about this tick and is true the moment the tick is observed.
	Full bool
	// WarmingRemaining is how long until the ring fills.
	WarmingRemaining time.Duration

	// StateError is a proxy.state read failure. The counts then come from the
	// live registry alone, which cannot know about members it has never seen.
	StateError string
}

// observe records one tick and returns the resulting reading.
func (w *proxyPoolHealthWindow) observe(counts proxyPoolMemberCounts, interval time.Duration) proxyPoolHealthReading {
	if interval <= 0 {
		interval = proxyPoolHealthInterval
	}
	w.samples = appendCappedPoolSamples(w.samples, proxyPoolSample{
		members: counts.members(),
		up:      counts.Up,
		down:    counts.down(),
	}, w.window)

	r := proxyPoolHealthReading{
		Members:    counts.members(),
		Up:         counts.Up,
		Down:       counts.down(),
		Degraded:   counts.Degraded,
		Dead:       counts.Dead,
		Connecting: counts.Connecting,
		Floor:      counts.Up,
	}
	// The floor is only comparable across ticks with the same member count. A
	// pool that grew or shrank has a different denominator, and a low-water mark
	// from the smaller era is not a dip in this pool; those samples are skipped,
	// so the window refills instead of reporting a floor that describes a pool
	// this one no longer is. The current tick always matches, so Floor never
	// exceeds the current up-count.
	//
	// Full counts the matching samples, not the ring length: after a resize the
	// ring still holds the old pool's ticks, and they say nothing about how much
	// of the hour this pool's floor and dip count describe. Reporting Full on
	// ring length would claim an hour of history for a window one tick old.
	matching := 0
	for _, s := range w.samples {
		if s.members != r.Members {
			continue
		}
		matching++
		if s.up < r.Floor {
			r.Floor = s.up
		}
		if s.down > 0 {
			r.Dips++
		}
	}
	r.Full = matching >= w.window
	if !r.Full {
		if remaining := time.Duration(w.window-matching) * interval; remaining < time.Minute {
			r.WarmingRemaining = time.Minute
		} else {
			r.WarmingRemaining = remaining
		}
	}

	switch {
	case r.Members == 0:
		// Nothing to judge. The same rung the provider's heartbeat uses before
		// it trusts a failure; the sentence says which of the two cases it is.
		r.Verdict = poolVerdictStartingUp
	case r.Up == 0 && r.Down == 0:
		// Every member is still connecting: nothing has been observed yet, so
		// "none is up" would be true but misleading. No member has failed
		// either, so this is the same "not judged yet" rung.
		r.Verdict = poolVerdictStartingUp
	case r.Up == 0:
		r.Verdict = poolVerdictDown
	case r.Down == 0:
		r.Verdict = poolVerdictHealthy
	case r.Down*100 >= r.Members*poolAttentionDownPct:
		r.Verdict = poolVerdictNeedsAttention
	default:
		r.Verdict = poolVerdictMostlyHealthy
	}

	switch {
	case r.Down == 0:
		r.Scope = poolScopeNone
	case 2*r.Down >= r.Members:
		// Half or more of the pool is down at the same time. One bad proxy
		// cannot do that, so the pool itself is the thing to look at. The
		// comparison is against Members, not Up, so a member that is still
		// connecting neither pads the denominator nor counts as a failure: on
		// its own a proxy that has not been observed cannot make the pool look
		// down.
		r.Scope = poolScopePool
	default:
		r.Scope = poolScopeIsolated
	}
	return r
}

// proxyPoolHealth reports the pool aggregate for now, built from a composition
// the caller read. Callers on different cadences — the health log loop and every
// metrics scrape — share one window, and a caller inside the current tick gets
// the previous reading rather than adding a sample, so the trailing hour stays
// an hour whatever the scrape rate is.
//
// The composition is passed in rather than read here, so the pool-health mutex
// is never held across a proxy.state read and a scrape reads the file once for
// both urnet_proxy_pool_size and this aggregate.
func proxyPoolHealth(now time.Time, comp proxyPoolComposition) proxyPoolHealthReading {
	return proxyPoolHealthAt(now, comp, false)
}

// observeProxyPoolHealth records a tick unconditionally, for the health-log
// writer. The writer owns the window's cadence: a scrape that landed inside the
// same interval must not consume the tick and leave the log printing a
// pre-snapshot reading, and the writer must keep advancing samples on its own
// cadence whether or not anyone scraped.
func observeProxyPoolHealth(now time.Time, comp proxyPoolComposition) proxyPoolHealthReading {
	return proxyPoolHealthAt(now, comp, true)
}

func proxyPoolHealthAt(now time.Time, comp proxyPoolComposition, force bool) proxyPoolHealthReading {
	proxyPoolHealthState.Lock()
	defer proxyPoolHealthState.Unlock()
	if proxyPoolHealthState.window == nil {
		proxyPoolHealthState.window = newProxyPoolHealthWindow(proxyPoolHealthInterval)
	}
	if !force && proxyPoolHealthState.seen && now.Sub(proxyPoolHealthState.lastAt) < proxyPoolHealthInterval {
		return proxyPoolHealthState.last
	}
	reading := proxyPoolHealthState.window.observe(comp.Counts, proxyPoolHealthInterval)
	reading.StateError = comp.StateErr
	proxyPoolHealthState.last = reading
	proxyPoolHealthState.lastAt = now
	proxyPoolHealthState.seen = true
	return reading
}

var proxyPoolHealthState = struct {
	sync.Mutex
	window *proxyPoolHealthWindow
	last   proxyPoolHealthReading
	lastAt time.Time
	seen   bool
}{}

// resetProxyPoolHealthForTest drops the shared window so a test starts from an
// empty hour. Tests only.
func resetProxyPoolHealthForTest() {
	proxyPoolHealthState.Lock()
	defer proxyPoolHealthState.Unlock()
	proxyPoolHealthState.window = nil
	proxyPoolHealthState.last = proxyPoolHealthReading{}
	proxyPoolHealthState.lastAt = time.Time{}
	proxyPoolHealthState.seen = false
}

// proxyPoolHealthLine renders the machine-readable aggregate. It uses the same
// prefix and the same field names as provider/health_heartbeat.go's
// [health][proxies] line, then the two facts that line has no field for: the
// hour's floor and the scope.
func proxyPoolHealthLine(r proxyPoolHealthReading) string {
	line := fmt.Sprintf(
		"❤️ [health][proxies] up=%d down=%d dead=%d degraded=%d connecting=%d members=%d floor=%d dips=%d verdict=%q scope=%s",
		r.Up, r.Down, r.Dead, r.Degraded, r.Connecting, r.Members, r.Floor, r.Dips, r.Verdict, r.Scope,
	)
	if r.StateError != "" {
		line += fmt.Sprintf(" state_error=%q", r.StateError)
	}
	return line
}

// proxyPoolVerdictSentence is the plain-sentence verdict: the provider's ladder
// and wording, plus the clause that says whether the operator is looking at the
// network or at specific proxies. This is the line that makes the distinction
// readable without knowing what a scope is.
func proxyPoolVerdictSentence(r proxyPoolHealthReading) string {
	switch r.Verdict {
	case poolVerdictStartingUp:
		if r.Members == 0 {
			return "❤️ [health] Verdict: starting up — no proxies configured yet; there is no pool to judge."
		}
		return fmt.Sprintf(
			"❤️ [health] Verdict: starting up — %d of %d proxies up so far; the hour-long pool history needs %s more before it reports a trend.",
			r.Up, r.Members, roundPoolDuration(r.WarmingRemaining),
		)
	case poolVerdictDown:
		return fmt.Sprintf(
			"❤️ [health] Verdict: down — none of the %d proxies is up; nothing can earn. %s",
			r.Members, r.scopeSentence(),
		)
	case poolVerdictHealthy:
		line := fmt.Sprintf("❤️ [health] Verdict: healthy — all %d proxies up, nothing needs attention.", r.Members)
		if r.Floor < r.Up {
			line += fmt.Sprintf(
				" The pool was down to %d in the last hour, so this is a recovery rather than a steady state.",
				r.Floor,
			)
		}
		return line
	case poolVerdictNeedsAttention:
		return fmt.Sprintf(
			"❤️ [health] Verdict: needs attention — only %d of %d proxies up, %d down (%d dead, %d failing). %s",
			r.Up, r.Members, r.Down, r.Dead, r.Degraded, r.scopeSentence(),
		)
	default:
		return fmt.Sprintf(
			"❤️ [health] Verdict: mostly healthy — %d of %d proxies up, %d down (%d dead, %d failing). %s",
			r.Up, r.Members, r.Down, r.Dead, r.Degraded, r.scopeSentence(),
		)
	}
}

// scopeSentence is the part of the verdict the provider's percentage threshold
// cannot say: whether the down members failed as a pool or as individual
// proxies.
func (r proxyPoolHealthReading) scopeSentence() string {
	switch r.Scope {
	case poolScopePool:
		return "Half or more of the pool is down at once, which points at the network or this host before it points at any one proxy."
	case poolScopeIsolated:
		return "Only a minority of the pool is down, so this is about those proxies rather than the pool."
	default:
		return ""
	}
}

// logProxyPoolHealth writes the aggregate on the health cadence, so a tail of the
// log carries the pool verdict beside the per-member report.
func logProxyPoolHealth(r proxyPoolHealthReading) {
	tlog("%s\n", proxyPoolHealthLine(r))
	tlog("%s\n", proxyPoolVerdictSentence(r))
}

// writeProxyPoolHealthMetrics emits the two facts the urnet_proxy_pool_size
// composition cannot carry. Members and the up ratio are deliberately not
// re-emitted: sum(urnet_proxy_pool_size) and its status="up" series already give
// both, and a second copy is a second thing that can drift.
func writeProxyPoolHealthMetrics(b *strings.Builder, r proxyPoolHealthReading) {
	// No pool, no aggregate. urnet_proxy_pool_up_floor == 0 is the "the pool
	// bottomed out inside the hour" outage signal, so emitting it for an
	// unconfigured, direct-only or freshly installed node would fire that alert
	// forever. Absent is the honest answer: urnet_proxy_pool_size already says
	// the pool is empty.
	if r.Members == 0 {
		return
	}
	fmt.Fprintf(b, "# HELP urnet_proxy_pool_up_floor Lowest number of proxies up in the last hour: the worst the pool got, not the worst it is.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_pool_up_floor gauge\n")
	fmt.Fprintf(b, "urnet_proxy_pool_up_floor %d\n", r.Floor)
	fmt.Fprintf(b, "# HELP urnet_proxy_pool_degraded_ticks Health ticks in the last hour at which at least one proxy was down.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_pool_degraded_ticks gauge\n")
	fmt.Fprintf(b, "urnet_proxy_pool_degraded_ticks %d\n", r.Dips)
	fmt.Fprintf(b, "# HELP urnet_proxy_pool_scope Whether the pool is failing as a unit (pool) or individual proxies are (isolated); none when everything is up.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_pool_scope gauge\n")
	for _, scope := range []string{poolScopeNone, poolScopeIsolated, poolScopePool} {
		value := 0
		if r.Scope == scope {
			value = 1
		}
		fmt.Fprintf(b, "urnet_proxy_pool_scope{scope=%q} %d\n", scope, value)
	}
}

// roundPoolDuration renders a duration the way the sentence reads it: "47m",
// never "47m12.3s".
func roundPoolDuration(d time.Duration) string {
	if d < time.Minute {
		return "1m"
	}
	return fmt.Sprintf("%dm", int(d.Round(time.Minute)/time.Minute))
}

func appendCappedPoolSamples(s []proxyPoolSample, v proxyPoolSample, limit int) []proxyPoolSample {
	s = append(s, v)
	if len(s) > limit {
		s = s[len(s)-limit:]
	}
	return s
}
