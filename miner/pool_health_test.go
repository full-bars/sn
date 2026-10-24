package miner

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestNewProxyPoolHealthWindowTickSizing pins the interval-to-ticks conversion:
// the window is a duration, so a caller ticking at 10m must still reason over one
// hour rather than over six ticks.
func TestNewProxyPoolHealthWindowTickSizing(t *testing.T) {
	cases := []struct {
		interval time.Duration
		window   int
	}{
		{time.Minute, 60},
		{5 * time.Minute, 12},
		{10 * time.Minute, 6},
		{45 * time.Minute, 2}, // ceil, minimum 2
		{4 * time.Hour, 2},    // ceil, minimum 2
		{0, 60},               // zero falls back to the health cadence
		{-time.Minute, 60},    // negative likewise
	}
	for _, c := range cases {
		w := newProxyPoolHealthWindow(c.interval)
		if w.window != c.window {
			t.Errorf("interval %s: got window=%d, want %d", c.interval, w.window, c.window)
		}
	}
}

// TestProxyPoolHealthReflectsMemberHealth walks the three shapes the pool has to
// tell apart from the outside: everything up, a mixed pool, and everything down.
// They share a member count, so only the aggregate distinguishes them.
func TestProxyPoolHealthReflectsMemberHealth(t *testing.T) {
	cases := []struct {
		name        string
		counts      proxyPoolMemberCounts
		wantVerdict string
		wantScope   string
	}{
		{
			name:        "all up",
			counts:      proxyPoolMemberCounts{Up: 4},
			wantVerdict: poolVerdictHealthy,
			wantScope:   poolScopeNone,
		},
		{
			name:        "mixed",
			counts:      proxyPoolMemberCounts{Up: 3, Degraded: 1},
			wantVerdict: poolVerdictNeedsAttention,
			wantScope:   poolScopeIsolated,
		},
		{
			name:        "all down",
			counts:      proxyPoolMemberCounts{Degraded: 4},
			wantVerdict: poolVerdictDown,
			wantScope:   poolScopePool,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newProxyPoolHealthWindow(time.Minute)
			r := w.observe(c.counts, time.Minute)
			if r.Verdict != c.wantVerdict {
				t.Errorf("verdict = %q, want %q", r.Verdict, c.wantVerdict)
			}
			if r.Scope != c.wantScope {
				t.Errorf("scope = %q, want %q", r.Scope, c.wantScope)
			}
			if r.Members != 4 || r.Down != c.counts.down() {
				t.Errorf("counts = members %d down %d, want members 4 down %d", r.Members, r.Down, c.counts.down())
			}
			if r.Floor != c.counts.Up {
				t.Errorf("floor = %d, want %d on the first tick", r.Floor, c.counts.Up)
			}
		})
	}
}

// TestProxyPoolHealthDistinguishesWholesaleFromOneBadProxy is the reason this
// view exists: the provider's percentage threshold reads "needs attention" for
// both of these, and only the scope says which one is a network problem.
func TestProxyPoolHealthDistinguishesWholesaleFromOneBadProxy(t *testing.T) {
	w := newProxyPoolHealthWindow(time.Minute)

	one := w.observe(proxyPoolMemberCounts{Up: 9, Degraded: 1}, time.Minute)
	wholesale := w.observe(proxyPoolMemberCounts{Up: 5, Dead: 5}, time.Minute)

	if one.Verdict != wholesale.Verdict {
		t.Fatalf("setup: verdicts differ (%q vs %q); this test is about what the verdict cannot say",
			one.Verdict, wholesale.Verdict)
	}
	if one.Verdict != poolVerdictNeedsAttention {
		t.Fatalf("setup: verdict = %q, want %q for both", one.Verdict, poolVerdictNeedsAttention)
	}
	if one.Scope != poolScopeIsolated {
		t.Errorf("one bad proxy: scope = %q, want %q", one.Scope, poolScopeIsolated)
	}
	if wholesale.Scope != poolScopePool {
		t.Errorf("half the pool down: scope = %q, want %q", wholesale.Scope, poolScopePool)
	}

	// The sentences must say the difference in words, not just in a field name.
	oneSentence := proxyPoolVerdictSentence(one)
	wholesaleSentence := proxyPoolVerdictSentence(wholesale)
	if !strings.Contains(oneSentence, "those proxies rather than the pool") {
		t.Errorf("one bad proxy sentence does not blame the members: %q", oneSentence)
	}
	if !strings.Contains(wholesaleSentence, "network or this host") {
		t.Errorf("wholesale sentence does not blame the pool: %q", wholesaleSentence)
	}
}

// TestProxyPoolHealthMajorityIsPoolScope pins the scope boundary: half the pool
// down is already the pool failing, one short of half is still individual
// proxies. A two-member pool has no minority, so it can never be isolated, and a
// member that has never been observed is not down at all.
func TestProxyPoolHealthMajorityIsPoolScope(t *testing.T) {
	cases := []struct {
		name   string
		counts proxyPoolMemberCounts
		want   string
	}{
		{"one of ten", proxyPoolMemberCounts{Up: 9, Degraded: 1}, poolScopeIsolated},
		{"four of ten", proxyPoolMemberCounts{Up: 6, Dead: 4}, poolScopeIsolated},
		{"half of ten", proxyPoolMemberCounts{Up: 5, Dead: 5}, poolScopePool},
		{"six of ten", proxyPoolMemberCounts{Up: 4, Dead: 6}, poolScopePool},
		{"all of ten", proxyPoolMemberCounts{Dead: 10}, poolScopePool},
		{"one of two", proxyPoolMemberCounts{Up: 1, Degraded: 1}, poolScopePool},
		{"nobody down", proxyPoolMemberCounts{Up: 10}, poolScopeNone},
		{"one not yet observed", proxyPoolMemberCounts{Up: 1, Connecting: 1}, poolScopeNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newProxyPoolHealthWindow(time.Minute)
			if got := w.observe(c.counts, time.Minute).Scope; got != c.want {
				t.Errorf("scope = %q, want %q", got, c.want)
			}
		})
	}
}

// TestProxyPoolHealthFloorRemembersADip is the per-member view's blind spot. A
// pool that bottoms out to zero and recovers is healthy on the tick you look at
// it, and on every scrape after that; the floor is the only thing that remembers.
func TestProxyPoolHealthFloorRemembersADip(t *testing.T) {
	w := newProxyPoolHealthWindow(time.Minute)

	for _, counts := range []proxyPoolMemberCounts{
		{Up: 4},
		{Up: 3, Degraded: 1},
		{Degraded: 4},
		{Up: 4},
	} {
		w.observe(counts, time.Minute)
	}

	r := w.observe(proxyPoolMemberCounts{Up: 4}, time.Minute)
	if r.Verdict != poolVerdictHealthy {
		t.Fatalf("verdict = %q, want %q", r.Verdict, poolVerdictHealthy)
	}
	if r.Floor != 0 {
		t.Errorf("floor = %d, want 0: the pool reached zero inside the hour", r.Floor)
	}
	if r.Dips != 2 {
		t.Errorf("dips = %d, want 2 (the one-down tick and the all-down tick)", r.Dips)
	}
	if !strings.Contains(proxyPoolVerdictSentence(r), "down to 0 in the last hour") {
		t.Errorf("healthy sentence hides the dip: %q", proxyPoolVerdictSentence(r))
	}

	// The floor must age out of the window, not persist forever.
	for i := 0; i < w.window; i++ {
		r = w.observe(proxyPoolMemberCounts{Up: 4}, time.Minute)
	}
	if r.Floor != 4 {
		t.Errorf("floor = %d after a full window of healthy ticks, want 4", r.Floor)
	}
	if r.Dips != 0 {
		t.Errorf("dips = %d after a full window of healthy ticks, want 0", r.Dips)
	}
}

// TestProxyPoolHealthVerdictLadder pins every rung, including the small pools
// where any single failure clears the provider's 10% threshold.
func TestProxyPoolHealthVerdictLadder(t *testing.T) {
	cases := []struct {
		name   string
		counts proxyPoolMemberCounts
		want   string
	}{
		{"no members", proxyPoolMemberCounts{}, poolVerdictStartingUp},
		{"nothing observed yet", proxyPoolMemberCounts{Connecting: 4}, poolVerdictStartingUp},
		{"none up", proxyPoolMemberCounts{Dead: 3, Connecting: 2}, poolVerdictDown},
		{"all up", proxyPoolMemberCounts{Up: 8}, poolVerdictHealthy},
		{"one of twenty", proxyPoolMemberCounts{Up: 19, Degraded: 1}, poolVerdictMostlyHealthy},
		{"one of five", proxyPoolMemberCounts{Up: 4, Degraded: 1}, poolVerdictNeedsAttention},
		{"half of ten", proxyPoolMemberCounts{Up: 5, Dead: 5}, poolVerdictNeedsAttention},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newProxyPoolHealthWindow(time.Minute)
			if got := w.observe(c.counts, time.Minute).Verdict; got != c.want {
				t.Errorf("verdict = %q, want %q", got, c.want)
			}
		})
	}
}

// TestProxyPoolHealthWarmingIsReportedNotTrusted pins the one deviation from the
// provider's window: the provider refuses a verdict until a full hour of samples
// exists, because a leak verdict guessed from three samples is worse than none.
// A pool verdict is a statement about this tick, so it is true immediately; only
// the trend fields wait, and WarmingRemaining says how long.
func TestProxyPoolHealthWarmingIsReportedNotTrusted(t *testing.T) {
	w := newProxyPoolHealthWindow(time.Minute)

	r := w.observe(proxyPoolMemberCounts{Up: 4}, time.Minute)
	if r.Full {
		t.Fatal("window reported full after one sample")
	}
	if r.Verdict != poolVerdictHealthy {
		t.Errorf("verdict = %q, want an immediate %q", r.Verdict, poolVerdictHealthy)
	}
	if want := time.Duration(w.window-1) * time.Minute; r.WarmingRemaining != want {
		t.Errorf("warming remaining = %s, want %s", r.WarmingRemaining, want)
	}

	for i := 1; i < w.window; i++ {
		r = w.observe(proxyPoolMemberCounts{Up: 4}, time.Minute)
	}
	if !r.Full {
		t.Fatalf("window not full after %d samples", w.window)
	}
	if r.WarmingRemaining != 0 {
		t.Errorf("warming remaining = %s on a full window, want 0", r.WarmingRemaining)
	}
}

// TestProxyPoolHealthFloorIgnoresADifferentPool pins the floor's denominator: a
// low-water mark recorded when the pool held a different number of members
// describes a pool this one is not, so it must not be reported as a dip here.
func TestProxyPoolHealthFloorIgnoresADifferentPool(t *testing.T) {
	w := newProxyPoolHealthWindow(time.Minute)

	// A two-member pool bottoms out at zero.
	w.observe(proxyPoolMemberCounts{Up: 2}, time.Minute)
	w.observe(proxyPoolMemberCounts{Degraded: 2}, time.Minute)

	// The operator then adds six members. The new pool has never been down, and
	// the old dip cannot be attributed to it.
	for i := 0; i < 3; i++ {
		r := w.observe(proxyPoolMemberCounts{Up: 8}, time.Minute)
		if r.Floor != 8 {
			t.Errorf("tick %d: floor = %d, want 8: the two-member dip is not this pool's", i, r.Floor)
		}
		if r.Dips != 0 {
			t.Errorf("tick %d: dips = %d, want 0", i, r.Dips)
		}
	}
}

// TestProxyPoolHealthMetricsCarryTheAggregate asserts the aggregate reaches a
// scrape: the floor, the dip count, and one-hot scope values a dashboard can
// alert on.
func TestProxyPoolHealthMetricsCarryTheAggregate(t *testing.T) {
	w := newProxyPoolHealthWindow(time.Minute)
	w.observe(proxyPoolMemberCounts{Up: 4}, time.Minute)
	r := w.observe(proxyPoolMemberCounts{Up: 2, Degraded: 2}, time.Minute)

	var b strings.Builder
	writeProxyPoolHealthMetrics(&b, r)
	out := b.String()

	for _, want := range []string{
		`urnet_proxy_pool_up_floor 2`,
		`urnet_proxy_pool_degraded_ticks 1`,
		`urnet_proxy_pool_scope{scope="none"} 0`,
		`urnet_proxy_pool_scope{scope="isolated"} 0`,
		`urnet_proxy_pool_scope{scope="pool"} 1`,
		`# TYPE urnet_proxy_pool_scope gauge`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
}

// TestProxyPoolHealthLineMatchesProviderFieldNames pins the machine line's
// vocabulary: the provider's [health][proxies] fields, plus floor/dips/scope.
func TestProxyPoolHealthLineMatchesProviderFieldNames(t *testing.T) {
	w := newProxyPoolHealthWindow(time.Minute)
	r := w.observe(proxyPoolMemberCounts{Up: 3, Dead: 1, Connecting: 1}, time.Minute)
	line := proxyPoolHealthLine(r)

	if !strings.HasPrefix(line, "❤️ [health][proxies] ") {
		t.Errorf("line prefix = %q, want the provider's [health][proxies] prefix", line)
	}
	for _, want := range []string{
		"up=3", "down=1", "dead=1", "degraded=0", "connecting=1", "members=5",
		"floor=3", "dips=1", `verdict="needs attention"`, "scope=isolated",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("line missing %q: %s", want, line)
		}
	}
}

// TestProxyPoolHealthReadsRegistryState is the wiring test: a member's health in
// the live registry is what the aggregate reports, so the pool view is built on
// the existing registry rather than a second copy of it.
func TestProxyPoolHealthReadsRegistryState(t *testing.T) {
	// The pool reads proxy.state for its member list, so the test must own the
	// state directory. Without this the suite writes the operator's real
	// ~/.urnetwork/proxy.state, which a live provider then has to prune.
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	resetProxyLiveHealthRegistryForTest()
	resetProxyPoolHealthForTest()
	t.Cleanup(func() {
		resetProxyLiveHealthRegistryForTest()
		resetProxyPoolHealthForTest()
	})

	state, err := readProxyState()
	if err != nil {
		t.Fatalf("read proxy state: %v", err)
	}
	state.Proxies = map[string]ProxyEntry{}
	for i := 0; i < 4; i++ {
		state.Proxies[fmt.Sprintf("10.0.0.%d:443", i)] = ProxyEntry{ID: i}
	}
	if err := writeProxyState(state); err != nil {
		t.Fatalf("write proxy state: %v", err)
	}

	// Nothing observed yet: every member is "connecting", which is not a
	// failure, so the pool must not read as down.
	if r := proxyPoolHealth(time.Now(), readProxyPoolComposition()); r.Members != 4 || r.Up != 0 || r.Down != 0 {
		t.Fatalf("before any signal: members=%d up=%d down=%d, want 4/0/0", r.Members, r.Up, r.Down)
	} else if r.Verdict != poolVerdictStartingUp {
		t.Fatalf("before any signal: verdict = %q, want %q", r.Verdict, poolVerdictStartingUp)
	}

	noteLiveProxyHealth("10.0.0.0:443", "up", time.Time{})
	noteLiveProxyHealth("10.0.0.1:443", "up", time.Time{})
	noteLiveProxyHealth("10.0.0.2:443", "up", time.Time{})
	noteLiveProxyHealth("10.0.0.3:443", "recently_offline", time.Now())

	r := proxyPoolHealth(time.Now().Add(2*time.Minute), readProxyPoolComposition())
	if r.Up != 3 || r.Down != 1 || r.Degraded != 1 {
		t.Fatalf("after one failure: up=%d down=%d degraded=%d, want 3/1/1", r.Up, r.Down, r.Degraded)
	}
	if r.Scope != poolScopeIsolated {
		t.Errorf("scope = %q, want %q: one member down is not the pool failing", r.Scope, poolScopeIsolated)
	}

	// The same registry now takes the rest of the pool: the aggregate must
	// follow, and must switch to the pool scope.
	noteLiveProxyHealth("10.0.0.0:443", "recently_offline", time.Now())
	noteLiveProxyHealth("10.0.0.1:443", "recently_offline", time.Now())
	noteLiveProxyHealth("10.0.0.2:443", "dead", time.Now())

	r = proxyPoolHealth(time.Now().Add(4*time.Minute), readProxyPoolComposition())
	if r.Up != 0 || r.Members != 4 {
		t.Fatalf("after the pool failed: up=%d members=%d, want 0/4", r.Up, r.Members)
	}
	if r.Verdict != poolVerdictDown || r.Scope != poolScopePool {
		t.Errorf("verdict/scope = %q/%q, want %q/%q",
			r.Verdict, r.Scope, poolVerdictDown, poolScopePool)
	}
	if r.Floor != 0 {
		t.Errorf("floor = %d, want 0", r.Floor)
	}
}

// TestProxyPoolHealthCachesWithinATick asserts a fast scrape loop cannot add
// samples: the window has to reason over an hour of ticks, not over an hour of
// HTTP requests.
func TestProxyPoolHealthCachesWithinATick(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	resetProxyLiveHealthRegistryForTest()
	resetProxyPoolHealthForTest()
	t.Cleanup(func() {
		resetProxyLiveHealthRegistryForTest()
		resetProxyPoolHealthForTest()
	})

	now := time.Now()
	comp := readProxyPoolComposition()
	first := proxyPoolHealth(now, comp)
	second := proxyPoolHealth(now.Add(proxyPoolHealthInterval/2), comp)
	if first.Members != second.Members || first.Floor != second.Floor {
		t.Fatalf("a second call inside the tick changed the reading: %+v vs %+v", first, second)
	}

	third := proxyPoolHealth(now.Add(proxyPoolHealthInterval), comp)
	if third.Members != second.Members {
		t.Fatalf("a call after the tick should still see the same members: %+v vs %+v", second, third)
	}
}

// TestProxyPoolHealthFullRequiresMatchingSamples pins the other half of the
// resize rule: the ring can be full of the OLD pool's ticks while this pool has
// one. Full must describe the samples Floor and Dips were derived from, not the
// slice length, or a resize would claim an hour of history it does not have.
func TestProxyPoolHealthFullRequiresMatchingSamples(t *testing.T) {
	w := newProxyPoolHealthWindow(time.Minute)
	for i := 0; i < w.window; i++ {
		w.observe(proxyPoolMemberCounts{Up: 4}, time.Minute)
	}

	// The pool doubles. The ring still holds a full window of four-member
	// ticks; they are not evidence about the eight-member pool.
	r := w.observe(proxyPoolMemberCounts{Up: 8}, time.Minute)
	if r.Full {
		t.Fatalf("Full after a resize: the reading came from 1 matching sample, not %d", w.window)
	}
	if r.Floor != 8 || r.Dips != 0 {
		t.Errorf("floor/dips = %d/%d after the resize, want 8/0", r.Floor, r.Dips)
	}
	if want := time.Duration(w.window-1) * time.Minute; r.WarmingRemaining != want {
		t.Errorf("warming remaining = %s after the resize, want %s", r.WarmingRemaining, want)
	}

	// Only a full window of the new pool fills it.
	for i := 0; i < w.window-1; i++ {
		r = w.observe(proxyPoolMemberCounts{Up: 8}, time.Minute)
	}
	if !r.Full || r.WarmingRemaining != 0 {
		t.Errorf("after a full window of matching samples: full=%v remaining=%s, want true/0",
			r.Full, r.WarmingRemaining)
	}
}

// TestProxyPoolHealthMetricsOmittedForEmptyPool pins the difference between
// "nothing configured" and "the pool bottomed out": urnet_proxy_pool_up_floor 0
// is the outage signal, so an empty pool must publish no aggregate at all.
func TestProxyPoolHealthMetricsOmittedForEmptyPool(t *testing.T) {
	w := newProxyPoolHealthWindow(time.Minute)
	r := w.observe(proxyPoolMemberCounts{}, time.Minute)
	if r.Members != 0 {
		t.Fatalf("setup: members = %d, want 0", r.Members)
	}

	var b strings.Builder
	writeProxyPoolHealthMetrics(&b, r)
	if b.Len() != 0 {
		t.Fatalf("an empty pool published a pool-health aggregate:\n%s", b.String())
	}
}

// TestProxyPoolHealthWriterTickSurvivesAScrape pins the cadence split: a scrape
// inside the interval gets the cached reading, and the health-log writer still
// records its own tick, so its line describes the snapshot it just wrote rather
// than a pre-snapshot reading left behind by Prometheus.
func TestProxyPoolHealthWriterTickSurvivesAScrape(t *testing.T) {
	resetProxyPoolHealthForTest()
	t.Cleanup(resetProxyPoolHealthForTest)

	now := time.Now()
	healthy := proxyPoolComposition{Counts: proxyPoolMemberCounts{Up: 4}}
	failed := proxyPoolComposition{Counts: proxyPoolMemberCounts{Up: 3, Degraded: 1}}

	if r := proxyPoolHealth(now, healthy); r.Up != 4 || r.Down != 0 {
		t.Fatalf("first tick = up %d down %d, want 4/0", r.Up, r.Down)
	}
	// A scrape 15s into the tick must return the cached reading and must not
	// consume the writer's tick.
	if r := proxyPoolHealth(now.Add(15*time.Second), failed); r.Down != 0 {
		t.Fatalf("a scrape inside the tick observed: down = %d, want the cached 0", r.Down)
	}
	// The writer's own tick records unconditionally.
	r := observeProxyPoolHealth(now.Add(proxyPoolHealthInterval), failed)
	if r.Down != 1 || r.Dips != 1 || r.Floor != 3 {
		t.Fatalf("writer tick = down %d dips %d floor %d, want 1/1/3", r.Down, r.Dips, r.Floor)
	}
}

// TestProxyPoolHealthScopeExcludesConnectingMembers pins the denominator: a
// member that has not been observed yet is neither down nor a reason to blame
// the pool. Before this, 2*Up <= Members counted connecting members as if they
// were failures, so one dead proxy during startup read as a pool-wide outage.
func TestProxyPoolHealthScopeExcludesConnectingMembers(t *testing.T) {
	cases := []struct {
		name   string
		counts proxyPoolMemberCounts
		want   string
	}{
		// Startup: one failure, everything else still connecting. One proxy
		// cannot be half the pool.
		{"one dead among ninety-nine connecting", proxyPoolMemberCounts{Dead: 1, Connecting: 99}, poolScopeIsolated},
		{"one up one down two connecting", proxyPoolMemberCounts{Up: 1, Degraded: 1, Connecting: 2}, poolScopeIsolated},
		// Connecting members do not pad the denominator either: when the
		// observed half is down, the pool is down.
		{"half dead half connecting", proxyPoolMemberCounts{Dead: 50, Connecting: 50}, poolScopePool},
		{"all connecting", proxyPoolMemberCounts{Connecting: 4}, poolScopeNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newProxyPoolHealthWindow(time.Minute)
			if got := w.observe(c.counts, time.Minute).Scope; got != c.want {
				t.Errorf("scope = %q, want %q", got, c.want)
			}
		})
	}

	// And the reason scope matters: the fully-down pool is still the pool.
	w := newProxyPoolHealthWindow(time.Minute)
	if got := w.observe(proxyPoolMemberCounts{Dead: 4}, time.Minute).Scope; got != poolScopePool {
		t.Errorf("fully down scope = %q, want %q", got, poolScopePool)
	}
}
