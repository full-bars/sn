package miner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"

	"github.com/urfoundation/sn/provider/bandwidth"
)

// T-07 tests: the AIMD pool controller's ceiling and step math, the URL shed
// ranking, the shed backoff, and the controller loop that computes and
// persists TargetPoolSize (the write side of the target the URL fetcher reads).

// After a pressure cut the pool must not regrow past a ceiling just under the
// level where pressure appeared, or it sawtooths: grow, hit the same wall, shed
// earners, repeat.
func TestAimdCeilingCapsRegrowthAfterCut(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var c aimdCeilingState
	c.record(400, now) // pressure hit at a pool of 400, then the pool was cut to 280

	ceiling := c.effective(now)
	if ceiling != 360 {
		t.Fatalf("ceiling = %d, want 360 (0.9 x 400)", ceiling)
	}
	target := 280
	for i := 0; i < 20; i++ {
		target = aimdStep(target, 10_000, 0.05, ceiling)
	}
	if target != 360 {
		t.Fatalf("regrowth settled at %d, want it held at the ceiling 360", target)
	}
}

// The ceiling is a memory of where trouble was, not a permanent cap: it relaxes
// toward the level pressure hit and is released after 24h so a box whose
// capacity has changed can rediscover it.
func TestAimdCeilingRelaxesAndReleases(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var c aimdCeilingState
	c.record(400, now)

	if got := c.effective(now.Add(12 * time.Hour)); got != 380 {
		t.Fatalf("ceiling at 12h = %d, want 380 (halfway from 360 to 400)", got)
	}
	if got := c.effective(now.Add(24 * time.Hour)); got != 0 {
		t.Fatalf("ceiling at 24h = %d, want 0 (released)", got)
	}
	if got := c.effective(now.Add(48 * time.Hour)); got != 0 {
		t.Fatalf("ceiling at 48h = %d, want 0 (released)", got)
	}
}

func TestAimdCeilingNeverBelowFloor(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var c aimdCeilingState
	c.record(aimdFloor-10, now)
	if got := c.effective(now); got < aimdFloor {
		t.Fatalf("ceiling = %d, want at least the floor %d", got, aimdFloor)
	}
}

func TestAimdCeilingUnsetMeansNoCeiling(t *testing.T) {
	var c aimdCeilingState
	if got := c.effective(time.Unix(1_700_000_000, 0)); got != 0 {
		t.Fatalf("zero-value ceiling = %d, want 0 (none)", got)
	}
}

func TestCombineCeilingsTakesTightestPositive(t *testing.T) {
	cases := []struct{ configured, learned, want int }{
		{0, 0, 0},
		{500, 0, 500},
		{0, 360, 360},
		{500, 360, 360},
		{300, 360, 300},
	}
	for _, tc := range cases {
		if got := combineCeilings(tc.configured, tc.learned); got != tc.want {
			t.Errorf("combineCeilings(%d, %d) = %d, want %d", tc.configured, tc.learned, got, tc.want)
		}
	}
}

// Pressure is held for many minutes, and each sustained-high sample cuts again
// at 0.7x. The ceiling must remember where pressure FIRST hit, not the last,
// lowest cut of the cascade, or a transient host event pins the pool near the
// floor for a day.
func TestAimdCeilingCascadeKeepsTheFirstHit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var c aimdCeilingState
	c.record(1000, now)
	c.record(700, now.Add(10*time.Minute))
	c.record(490, now.Add(20*time.Minute))
	if got := c.effective(now.Add(20 * time.Minute)); got != 900 {
		t.Fatalf("ceiling after a cascade = %d, want 900 (0.9 x the first hit of 1000)", got)
	}
}

// Once the ceiling has been released, a new episode starts fresh at its own level.
func TestAimdCeilingNewEpisodeAfterReleaseStartsFresh(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var c aimdCeilingState
	c.record(1000, now)
	later := now.Add(25 * time.Hour)
	c.record(300, later)
	if got := c.effective(later); got != 270 {
		t.Fatalf("ceiling for a fresh episode = %d, want 270", got)
	}
}

// The recorded hit is the real pool, not a target the cache never reached.
func TestCeilingHitUsesTheSmallerOfTargetAndPool(t *testing.T) {
	cases := []struct{ target, pool, want int }{
		{1000, 600, 600},
		{500, 900, 500},
		{500, 0, 500}, // pool size unknown
	}
	for _, tc := range cases {
		if got := ceilingHit(tc.target, tc.pool); got != tc.want {
			t.Errorf("ceilingHit(%d, %d) = %d, want %d", tc.target, tc.pool, got, tc.want)
		}
	}
}

// The trim/OOM cap bounds ALL running proxies, so the URL target may only use
// what paid proxies leave over.
func TestCapURLTargetLeavesRoomForPaid(t *testing.T) {
	cases := []struct{ next, cap, paid, want int }{
		{500, 0, 40, 500},   // no cap: untouched
		{500, 300, 40, 260}, // cap minus paid
		{200, 300, 40, 200}, // already under
		{500, 300, 400, 1},  // paid exceeds the cap: keep a nonzero target
	}
	for _, tc := range cases {
		if got := capURLTarget(tc.next, tc.cap, tc.paid); got != tc.want {
			t.Errorf("capURLTarget(%d, cap=%d, paid=%d) = %d, want %d", tc.next, tc.cap, tc.paid, got, tc.want)
		}
	}
}

func TestAimdStep(t *testing.T) {
	// calm growth, capped by ceiling
	if v := aimdStep(100, 100, 0.1, 500); v != 125 {
		t.Fatalf("grow: %v", v)
	}
	if v := aimdStep(490, 490, 0.1, 500); v != 500 {
		t.Fatalf("ceiling: %v", v)
	}
	// ceiling 0 = unlimited growth allowed
	if v := aimdStep(1000, 1000, 0.1, 0); v != 1025 {
		t.Fatalf("unlimited: %v", v)
	}
	// sustained pressure: multiplicative decrease with floor
	if v := aimdStep(500, 500, 0.8, 500); v != 350 {
		t.Fatalf("cut: %v", v)
	}
	if v := aimdStep(60, 60, 0.8, 500); v != 50 {
		t.Fatalf("floor: %v", v)
	}
	// middle band: hold
	if v := aimdStep(200, 200, 0.5, 500); v != 200 {
		t.Fatalf("hold: %v", v)
	}
	// never run the target far ahead of what is actually cached
	if v := aimdStep(400, 100, 0.1, 500); v != 125 {
		t.Fatalf("target tracks cache+increment when cache lags: %v", v)
	}
}

// The lowered-target line must not blame a cap change for a cache-tracking cut:
// aimdStep's grow branch catches the target down to cacheSize plus the
// increment when the pool lags far behind, and blaming a cap sends an operator
// chasing a cause that never existed.
func TestAimdMoveMessageAttributesCause(t *testing.T) {
	msg := aimdMoveMessage(1000, 100+aimdIncrement, 100, 0.1)
	if !strings.Contains(msg, "only 100 proxies are cached") || !strings.Contains(msg, "follows the live pool") {
		t.Fatalf("a cache-tracking cut must say so: %q", msg)
	}
	if strings.Contains(msg, "cap or ceiling") {
		t.Fatalf("a cache-tracking cut must not blame a cap or ceiling: %q", msg)
	}
	// A genuine cap or ceiling cut keeps its original attribution.
	if plain := aimdMoveMessage(1000, 700, 900, 0.5); !strings.Contains(plain, "cap or ceiling lowered") {
		t.Fatalf("a cap cut keeps its wording: %q", plain)
	}
	// A pressure shrink keeps its wording too.
	if shrink := aimdMoveMessage(1000, 500, 1000, 0.9); !strings.Contains(shrink, "pressure has been high") {
		t.Fatalf("a pressure shrink keeps its wording: %q", shrink)
	}
	// And a low-pressure raise keeps its wording.
	if grow := aimdMoveMessage(100, 100+aimdIncrement, 100, 0.1); !strings.Contains(grow, "pressure is low") {
		t.Fatalf("a low-pressure raise keeps its wording: %q", grow)
	}
	// A raise at mid pressure is attributed to a cap or ceiling change.
	if capRaise := aimdMoveMessage(100, 140, 100, 0.5); !strings.Contains(capRaise, "a cap or ceiling changed") {
		t.Fatalf("a mid-pressure raise keeps its wording: %q", capRaise)
	}
}

func TestSelectURLProxiesToShed(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"2.2.2.2:1080": {Health: "dead", Source: "url"},
		"3.3.3.3:1080": {Health: "offline", Source: "url"},
		"4.4.4.4:1080": {Health: "up", Source: "url"},
		"5.5.5.5:1080": {Health: "dead", Source: "file"}, // never shed: not url
	}}
	traffic := map[string]uint64{"1.1.1.1:1080": 100, "4.4.4.4:1080": 5}
	got := selectURLProxiesToShed(state, traffic, nil, 3)
	want := []string{"2.2.2.2:1080", "3.3.3.3:1080", "4.4.4.4:1080"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// n larger than pool: return everything url-sourced, still ordered
	if got := selectURLProxiesToShed(state, traffic, nil, 99); len(got) != 4 {
		t.Fatalf("overshoot: %v", got)
	}
}

func TestSelectURLProxiesToShedEmptyState(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	got := selectURLProxiesToShed(state, nil, nil, 5)
	if len(got) != 0 {
		t.Fatalf("empty state should return nothing, got %v", got)
	}
}

func TestSelectURLProxiesToShedAllFileSourced(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "dead", Source: "file"},
		"2.2.2.2:1080": {Health: "dead", Source: "file"},
	}}
	got := selectURLProxiesToShed(state, nil, nil, 10)
	if len(got) != 0 {
		t.Fatalf("all file-sourced should return nothing, got %v", got)
	}
}

func TestSelectURLProxiesToShedTrafficTieBreaking(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"2.2.2.2:1080": {Health: "up", Source: "url"},
		"3.3.3.3:1080": {Health: "up", Source: "url"},
	}}
	// same health, no traffic data -> sorted by address (lexicographic)
	got := selectURLProxiesToShed(state, nil, nil, 2)
	want := []string{"1.1.1.1:1080", "2.2.2.2:1080"}
	if !slices.Equal(got, want) {
		t.Fatalf("tie-break by address: got %v want %v", got, want)
	}
}

func TestSelectURLProxiesToShedTrafficOrdering(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"2.2.2.2:1080": {Health: "up", Source: "url"},
		"3.3.3.3:1080": {Health: "up", Source: "url"},
	}}
	// same health, different traffic -> shed lowest-traffic first
	traffic := map[string]uint64{
		"1.1.1.1:1080": 1000,
		"2.2.2.2:1080": 5,
		"3.3.3.3:1080": 500,
	}
	got := selectURLProxiesToShed(state, traffic, nil, 2)
	want := []string{"2.2.2.2:1080", "3.3.3.3:1080"}
	if !slices.Equal(got, want) {
		t.Fatalf("shed lowest-traffic first: got %v want %v", got, want)
	}
}

func TestSelectURLProxiesToShedHealthTierOrdering(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"2.2.2.2:1080": {Health: "inactive", Source: "url"},
		"3.3.3.3:1080": {Health: "long_offline", Source: "url"},
		"4.4.4.4:1080": {Health: "recently_offline", Source: "url"},
		"5.5.5.5:1080": {Health: "dead", Source: "url"},
	}}
	got := selectURLProxiesToShed(state, nil, nil, 5)
	want := []string{"5.5.5.5:1080", "2.2.2.2:1080", "3.3.3.3:1080", "4.4.4.4:1080", "1.1.1.1:1080"}
	if !slices.Equal(got, want) {
		t.Fatalf("health tiers: got %v want %v", got, want)
	}
}

// Right after a restart the lifetime traffic map is empty, so every proxy looks
// idle. The persisted earnings score must still protect the earner.
func TestSelectURLProxiesToShedEarningsProtectEarnerAfterRestart(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"2.2.2.2:1080": {Health: "up", Source: "url"},
		"3.3.3.3:1080": {Health: "up", Source: "url"},
	}}
	earnings := map[string]float64{"1.1.1.1:1080": 5e9, "2.2.2.2:1080": 0, "3.3.3.3:1080": 1e6}
	got := selectURLProxiesToShed(state, nil, earnings, 2)
	want := []string{"2.2.2.2:1080", "3.3.3.3:1080"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v (earner 1.1.1.1 must be shed last)", got, want)
	}
}

// Health tier still outranks earnings: a dead proxy goes before a healthy non-earner.
func TestSelectURLProxiesToShedHealthBeatsEarnings(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "dead", Source: "url"},
		"2.2.2.2:1080": {Health: "up", Source: "url"},
	}}
	earnings := map[string]float64{"1.1.1.1:1080": 9e9, "2.2.2.2:1080": 0}
	got := selectURLProxiesToShed(state, nil, earnings, 1)
	if !slices.Equal(got, []string{"1.1.1.1:1080"}) {
		t.Fatalf("got %v, want the dead proxy first", got)
	}
}

// Missing earnings data falls back to the lifetime-traffic order.
func TestSelectURLProxiesToShedNoEarningsFallsBackToTraffic(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"2.2.2.2:1080": {Health: "up", Source: "url"},
	}}
	traffic := map[string]uint64{"1.1.1.1:1080": 1000, "2.2.2.2:1080": 5}
	got := selectURLProxiesToShed(state, traffic, nil, 1)
	if !slices.Equal(got, []string{"2.2.2.2:1080"}) {
		t.Fatalf("got %v, want lowest-traffic first", got)
	}
}

// The trim cap counts RUNNING non-direct proxies. proxy.state also holds dead,
// backed-off and trim-held entries; counting those drove cap - paid to nothing
// and pinned the URL pool at 1.
func TestPaidRunningCountCountsOnlyRunningPaid(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"paid-up:1":    {Health: "up", Source: "file"},
		"paid-run2:1":  {Health: "up", Source: ""},
		"paid-dead:1":  {Health: "dead", Source: "file"},
		"url-up:1":     {Health: "up", Source: "url"},
		directProxyKey: {Health: "up", Source: "file"},
	}}
	running := []string{"paid-up:1", "paid-run2:1", "url-up:1", directProxyKey, "unknown:1"}
	if got := paidRunningCount(state, running); got != 2 {
		t.Fatalf("paidRunningCount = %d, want 2 (running non-URL, non-direct, known to the state)", got)
	}
	if got := paidRunningCount(state, nil); got != 0 {
		t.Fatalf("paidRunningCount with nothing running = %d, want 0", got)
	}
}

func TestApplyShedBackoffNeverShortensAndSetsOneWhenNone(t *testing.T) {
	withTempHome(t)
	hist := globalProxyFailureHistory
	now := time.Now()

	applyShedBackoff("fresh:1", now)
	if hist.Eligible("fresh:1", now.Add(shedBackoff-time.Second)) {
		t.Fatalf("a shed must set a backoff when none existed")
	}

	// A longer window already on the address must survive intact.
	long := now.Add(24 * time.Hour)
	hist.SetBackoffUntil("long:1", long)
	applyShedBackoff("long:1", now)
	if hist.Eligible("long:1", long.Add(-time.Minute)) {
		t.Fatalf("a shed must not shorten a longer backoff")
	}

	// A shorter window is lengthened up to shedBackoff.
	hist.SetBackoffUntil("short:1", now.Add(10*time.Minute))
	applyShedBackoff("short:1", now)
	if hist.Eligible("short:1", now.Add(shedBackoff-time.Second)) {
		t.Fatalf("a shed lengthens a shorter backoff up to shedBackoff")
	}
}

// Pool shed and proxy audit's park each pick addresses from their own snapshot.
// When both land on the same address the shed's short backoff must not
// overwrite the park's long one.
func TestShedPoolToTargetDoesNotShortenALongerBackoff(t *testing.T) {
	withTempHome(t)
	hist := globalProxyFailureHistory

	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{
		"2.2.2.2:1080": {ID: 2, Health: "dead", Source: "url"}, // shed first
		"1.1.1.1:1080": {ID: 1, Health: "up", Source: "url"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"2.2.2.2:1080": {},
		"1.1.1.1:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(24 * time.Hour)
	hist.SetBackoffUntil("2.2.2.2:1080", long)

	shedPoolToTarget(1)

	if hist.Eligible("2.2.2.2:1080", long.Add(-time.Minute)) {
		t.Fatalf("a shed must not shorten a longer backoff already on the address")
	}
}

// A shed credentialed URL proxy must actually leave the URL cache. state.Proxies
// is keyed by identity while the cache is keyed by bare address; shedding used
// to hand the identity key straight to a cache delete, so the entry stayed, the
// pool logged "shed" and nothing was removed.
func TestShedPoolToTargetRemovesCredentialedURLProxyFromCache(t *testing.T) {
	withTempHome(t)
	hist := globalProxyFailureHistory

	deadKey := (&connect.ProxySettings{Address: "2.2.2.2:1080", Auth: &proxy.Auth{User: "u"}}).Key()
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{
		deadKey:        {ID: 2, Health: "dead", Source: "url"}, // shed first
		"1.1.1.1:1080": {ID: 1, Health: "up", Source: "url"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"2.2.2.2:1080": {},
		"1.1.1.1:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	shedPoolToTarget(1)

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Cache["2.2.2.2:1080"]; ok {
		t.Fatalf("shed credentialed URL proxy is still in the URL cache: %v", got.Cache)
	}
	if _, ok := got.Cache["1.1.1.1:1080"]; !ok {
		t.Fatalf("the surviving proxy must stay cached: %v", got.Cache)
	}
	// Half the backoff from now: far from both edges, so no timing race.
	if hist.Eligible(deadKey, time.Now().Add(shedBackoff/2)) {
		t.Fatalf("the shed backoff must be recorded under the identity key")
	}
}

// Among equally healthy URL proxies the shed ranking protects traffic. A
// credentialed earner's traffic is keyed by identity, so it must outrank an
// idle proxy instead of reading as zero.
func TestShedPoolToTargetKeepsCredentialedEarner(t *testing.T) {
	withTempHome(t)
	restoreEarn := withGlobalEarningsStore(t)
	defer restoreEarn()

	earnerKey := (&connect.ProxySettings{Address: "1.1.1.1:1080", Auth: &proxy.Auth{User: "u"}}).Key()
	earnerBw := &bandwidth.ProxyBandwidth{}
	earnerBw.BillableTx.Store(10 << 30)
	publishProxyEarnBandwidth(earnerKey, earnerBw)
	t.Cleanup(func() { unpublishProxyEarnBandwidth(earnerKey) })

	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{
		earnerKey:      {ID: 1, Health: "up", Source: "url"},
		"2.2.2.2:1080": {ID: 2, Health: "up", Source: "url"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.1.1.1:1080": {},
		"2.2.2.2:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	shedPoolToTarget(1)

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Cache["1.1.1.1:1080"]; !ok {
		t.Fatalf("the credentialed earner was shed: cache=%v", got.Cache)
	}
	if _, ok := got.Cache["2.2.2.2:1080"]; ok {
		t.Fatalf("the idle proxy should have been shed: cache=%v", got.Cache)
	}
}

// withTempAimdController pins the global seams the controller loop reads
// (self-heal resolution, the pressure atomics, the tick interval) and runs the
// controller until the returned stop func is invoked.
func withTempAimdController(t *testing.T, configuredMax int) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	resetGlobalControlStateForTest()
	t.Cleanup(resetGlobalControlStateForTest)
	setPressure(0)
	setPressureNoCPU(0)
	t.Cleanup(func() {
		setPressure(0)
		setPressureNoCPU(0)
	})
	prevInterval := poolControlInterval
	poolControlInterval = 20 * time.Millisecond
	t.Cleanup(func() { poolControlInterval = prevInterval })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPoolController(ctx, configuredMax, false)
	}()
	return cancel, done
}

// The calm case: with self-heal on the controller starts from the cache size,
// grows additively, clamps at the configured ceiling and PERSISTS the target
// the fetcher reads back.
func TestRunPoolControllerPersistsGrownTarget(t *testing.T) {
	dir := withTempHome(t)
	if err := os.WriteFile(filepath.Join(dir, "proxy_self_heal"), []byte("on\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cache := map[string]ProxyURLEntry{}
	for i := 0; i < 200; i++ {
		cache[fmt.Sprintf("10.0.%d.%d:1080", i/256, i%256)] = ProxyURLEntry{}
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: cache}); err != nil {
		t.Fatal(err)
	}

	cancel, done := withTempAimdController(t, 205)
	defer func() {
		cancel()
		<-done
	}()

	// First tick: target = cache size (200), then +25 clamped by the ceiling
	// 205; later ticks hold there.
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := readProxyURLState()
		if err == nil && state.TargetPoolSize == 205 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("target never reached the ceiling; last state %+v (err %v)", state, err)
		}
		time.Sleep(time.Millisecond)
	}
}

// A sustained high pressure cuts multiplicatively (145 -> 101 at 0.7x, above
// the floor), persists the lower target, and sheds the worst URL proxies back
// to the cache with a re-admission backoff.
func TestRunPoolControllerShrinksAndShedsUnderPressure(t *testing.T) {
	dir := withTempHome(t)
	if err := os.WriteFile(filepath.Join(dir, "proxy_self_heal"), []byte("on\n"), 0600); err != nil {
		t.Fatal(err)
	}

	proxies := map[string]ProxyEntry{}
	cache := map[string]ProxyURLEntry{}
	for i := 1; i <= 145; i++ {
		addr := fmt.Sprintf("10.0.%d.%d:1080", i/256, i%256)
		proxies[addr] = ProxyEntry{ID: i, Health: "up", Source: "url"}
		cache[addr] = ProxyURLEntry{}
	}
	if err := writeProxyState(&ProxyState{Proxies: proxies}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: cache, TargetPoolSize: 145}); err != nil {
		t.Fatal(err)
	}

	cancel, done := withTempAimdController(t, 0)
	defer func() {
		cancel()
		<-done
	}()
	setPressure(0.9)
	setPressureNoCPU(0.9)

	// Two consecutive high samples (the interval is 20ms in this fixture) land
	// the first cut; stop as soon as the lower target is persisted, before the
	// next sustained episode can cut again.
	deadline := time.Now().Add(5 * time.Second)
	cut := 0
	for {
		state, err := readProxyURLState()
		if err == nil && state.TargetPoolSize != 145 {
			cut = state.TargetPoolSize
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("controller never cut the target 145; last state %+v (err %v)", state, err)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if cut != 101 {
		t.Fatalf("first cut = %d, want 101 (0.7 x 145)", cut)
	}
	state, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if state.TargetPoolSize != cut {
		t.Fatalf("persisted target = %d, want the cut target %d", state.TargetPoolSize, cut)
	}
	if len(state.Cache) != cut {
		t.Fatalf("cache size = %d, want the pool shed down to the target %d", len(state.Cache), cut)
	}
	// Every shed address is held for at least the shed backoff.
	for addr := range proxies {
		if _, still := state.Cache[addr]; still {
			continue
		}
		if globalProxyFailureHistory.Eligible(addr, time.Now().Add(shedBackoff-time.Minute)) {
			t.Fatalf("shed address %s has no re-admission backoff", addr)
		}
	}
}

// The controller is stopped by cancelling its context on proxy manager
// shutdown; it must return promptly and not wait out the tick interval.
func TestRunPoolControllerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		runPoolController(ctx, 100, false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runPoolController did not exit promptly on a cancelled context")
	}
}

// With self-heal off the controller is inert: it must not write a target at all.
func TestRunPoolControllerSelfHealOffDoesNotWrite(t *testing.T) {
	dir := withTempHome(t)
	if err := os.WriteFile(filepath.Join(dir, "proxy_self_heal"), []byte("off\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.1.1.1:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	cancel, done := withTempAimdController(t, 0)
	defer func() {
		cancel()
		<-done
	}()
	setPressure(0.9)
	setPressureNoCPU(0.9)

	// Several intervals: enough for a cut if self-heal were on.
	time.Sleep(100 * time.Millisecond)

	state, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if state.TargetPoolSize != 0 {
		t.Fatalf("self-heal off must leave the target untouched, got %d", state.TargetPoolSize)
	}
}
