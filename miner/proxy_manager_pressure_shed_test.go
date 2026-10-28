package miner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Tests for the pressure cut's cancel half (F-03): shedPoolToTarget used to edit
// proxy.state and the URL cache and log a cut, but nothing stopped the running
// slots, so the memory stayed committed and the persisted TargetPoolSize
// ratcheted down for work that never happened. These pin the cancel through the
// live reloader and the target that is written from it.

// pressureShedTestPool is the live reloader a test installs as the pressure
// cut's cancel target: one running slot per identity, with every cancel
// recorded so a test can prove exactly what was stopped.
type pressureShedTestPool struct {
	reloader *minerProxyReloader

	mu        sync.Mutex
	cancelled []string
}

// installShedPoolSettings installs a reloader seeded with the given identities
// and returns it. The global is cleared on cleanup; the miner's tests do not run
// in parallel, so one installed reloader is unambiguous.
func installShedPoolSettings(t *testing.T, settings ...*connect.ProxySettings) *pressureShedTestPool {
	t.Helper()
	pool := &pressureShedTestPool{}
	pool.reloader = newMinerProxyReloader(
		func() ([]*connect.ProxySettings, int, error) { return nil, 0, nil },
		func(settings *connect.ProxySettings) (context.CancelFunc, error) { return func() {}, nil },
	)
	for _, settingsAt := range settings {
		key := settingsAt.Key()
		pool.reloader.seed(settingsAt, func() {
			pool.mu.Lock()
			pool.cancelled = append(pool.cancelled, key)
			pool.mu.Unlock()
		})
	}
	setPressureShedReloader(pool.reloader)
	t.Cleanup(func() { setPressureShedReloader(nil) })
	return pool
}

// installShedPool is installShedPoolSettings for tests that only care about
// bare addresses.
func installShedPool(t *testing.T, addresses ...string) *pressureShedTestPool {
	t.Helper()
	settings := make([]*connect.ProxySettings, 0, len(addresses))
	for _, address := range addresses {
		settings = append(settings, reloadTestSettings(address, "", ""))
	}
	return installShedPoolSettings(t, settings...)
}

func (p *pressureShedTestPool) cancelledKeys() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.cancelled...)
}

// writeURLPoolForShed writes n url-sourced proxies into proxy.state and the URL
// cache, so a shed has a pool to cut.
func writeURLPoolForShed(t *testing.T, n int) []string {
	t.Helper()
	proxies := map[string]ProxyEntry{}
	cache := map[string]ProxyURLEntry{}
	addresses := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		address := fmt.Sprintf("10.0.%d.%d:1080", i/256, i%256)
		proxies[address] = ProxyEntry{ID: i, Health: "up", Source: "url"}
		cache[address] = ProxyURLEntry{}
		addresses = append(addresses, address)
	}
	if err := writeProxyState(&ProxyState{Proxies: proxies}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: cache}); err != nil {
		t.Fatal(err)
	}
	return addresses
}

// The whole point of the fix: a cut down to target stops exactly the over-target
// number of running slots through the reloader, not just their cache entries.
func TestPressureShedCancelsExactlyTheExcess(t *testing.T) {
	withTempHome(t)
	addresses := writeURLPoolForShed(t, 6)
	pool := installShedPool(t, addresses...)

	result := shedPoolToTarget(2)

	if !result.Applied {
		t.Fatal("a shed with an installed reloader must report itself applied")
	}
	if result.Running != 6 || result.Requested != 4 || result.Cancelled != 4 || result.Pool != 2 {
		t.Fatalf("result = %+v, want running 6, requested 4, cancelled 4, pool 2", result)
	}
	cancelled := pool.cancelledKeys()
	if len(cancelled) != 4 {
		t.Fatalf("reloader cancelled %d slots (%v), want exactly 4", len(cancelled), cancelled)
	}
	// The surviving slots are still served.
	running := pool.reloader.runningKeys()
	if len(running) != 2 {
		t.Fatalf("reloader still runs %d slots (%v), want 2", len(running), running)
	}
	// And the cache holds exactly the two that were not cancelled.
	urlState, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if len(urlState.Cache) != 2 {
		t.Fatalf("cache size = %d, want 2: %v", len(urlState.Cache), urlState.Cache)
	}
	for _, key := range cancelled {
		if _, still := urlState.Cache[key]; still {
			t.Fatalf("cancelled proxy %s is still in the URL cache", key)
		}
	}
}

// A shed is measured on the RUNNING pool, not on proxy.state. State keeps dead,
// backed-off and trim-held entries; counting those used to cut past the target
// and blame the wrong number of slots.
func TestPressureShedCountsRunningNotState(t *testing.T) {
	withTempHome(t)
	addresses := writeURLPoolForShed(t, 40)
	// Only half the state entries are actually running.
	pool := installShedPool(t, addresses[:20]...)

	result := shedPoolToTarget(10)

	if result.Running != 20 || result.Cancelled != 10 || result.Pool != 10 {
		t.Fatalf("result = %+v, want running 20, cancelled 10, pool 10", result)
	}
	if got := len(pool.cancelledKeys()); got != 10 {
		t.Fatalf("cancelled %d slots, want 10", got)
	}
	urlState, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if len(urlState.Cache) != 30 {
		t.Fatalf("cache size = %d, want 30 (only the 10 cancelled were removed)", len(urlState.Cache))
	}
}

// A pool already at or below the target must not cancel anything: there is no
// over-target slot to remove.
func TestPressureShedDoesNotCancelWhenRunningAtOrBelowTarget(t *testing.T) {
	withTempHome(t)
	addresses := writeURLPoolForShed(t, 3)
	pool := installShedPool(t, addresses...)

	result := shedPoolToTarget(5)

	if !result.Applied || result.Running != 3 || result.Requested != 0 || result.Cancelled != 0 || result.Pool != 3 {
		t.Fatalf("result = %+v, want applied, running 3, nothing requested or cancelled", result)
	}
	if got := len(pool.cancelledKeys()); got != 0 {
		t.Fatalf("cancelled %d slots, want 0", got)
	}
	urlState, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if len(urlState.Cache) != 3 {
		t.Fatalf("cache size = %d, want the pool untouched at 3", len(urlState.Cache))
	}
	for _, address := range addresses {
		if !globalProxyFailureHistory.Eligible(address, time.Now().Add(shedBackoff-time.Minute)) {
			t.Fatalf("%s was backed off by a cut that cancelled nothing", address)
		}
	}
}

// A second cut on the same pool has nothing left to do: the first one already
// brought the running set to the target, so no slot is cancelled twice and no
// further cache entry disappears.
func TestPressureShedSecondCutIsIdempotent(t *testing.T) {
	withTempHome(t)
	addresses := writeURLPoolForShed(t, 6)
	pool := installShedPool(t, addresses...)

	first := shedPoolToTarget(2)
	if first.Cancelled != 4 {
		t.Fatalf("first cut cancelled %d, want 4", first.Cancelled)
	}
	second := shedPoolToTarget(2)
	if second.Running != 2 || second.Requested != 0 || second.Cancelled != 0 {
		t.Fatalf("second cut = %+v, want running 2 and nothing to cancel", second)
	}
	if got := len(pool.cancelledKeys()); got != 4 {
		t.Fatalf("reloader cancelled %d slots after two cuts, want 4 (the second is a no-op)", got)
	}
	urlState, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if len(urlState.Cache) != 2 {
		t.Fatalf("cache size = %d after two cuts, want 2", len(urlState.Cache))
	}
}

// With no live reloader nothing owns a slot, so a cut must be a complete no-op:
// no cache eviction, no backoff, and - through pressureShedTarget - no lowered
// target claiming memory was returned.
func TestPressureShedWithoutReloaderIsANoOp(t *testing.T) {
	withTempHome(t)
	addresses := writeURLPoolForShed(t, 5)
	setPressureShedReloader(nil)
	t.Cleanup(func() { setPressureShedReloader(nil) })

	result := shedPoolToTarget(1)

	if result.Applied || result.Running != 0 || result.Cancelled != 0 {
		t.Fatalf("result = %+v, want an unapplied no-op", result)
	}
	urlState, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if len(urlState.Cache) != len(addresses) {
		t.Fatalf("cache size = %d, want the pool untouched at %d", len(urlState.Cache), len(addresses))
	}
	for _, address := range addresses {
		if !globalProxyFailureHistory.Eligible(address, time.Now().Add(shedBackoff-time.Minute)) {
			t.Fatalf("%s was backed off by a cut that could not cancel anything", address)
		}
	}
}

// The persisted target is written from the result, never from the AIMD step.
// Case by case: a full cut lands on the step, a partial cut on the pool that
// remains, and a cut that cancelled nothing on the unchanged target.
func TestPressureShedTargetNeverClaimsMoreThanCancelled(t *testing.T) {
	cases := []struct {
		name string
		// target is the recorded target before the cut, next the AIMD step.
		target, next int
		shed         pressureShedResult
		want         int
	}{
		{
			name:   "full cut",
			target: 6, next: 2,
			shed: pressureShedResult{Applied: true, Running: 6, Requested: 4, Cancelled: 4, Pool: 2},
			want: 2,
		},
		{
			name:   "partial cut persists the remaining pool",
			target: 6, next: 2,
			shed: pressureShedResult{Applied: true, Running: 6, Requested: 4, Cancelled: 1, Pool: 5},
			want: 5,
		},
		{
			name:   "nothing cancelled leaves the target alone",
			target: 6, next: 2,
			shed: pressureShedResult{Applied: true, Running: 6, Requested: 4, Cancelled: 0, Pool: 6},
			want: 6,
		},
		{
			name:   "no reloader leaves the target alone",
			target: 6, next: 2,
			shed: pressureShedResult{},
			want: 6,
		},
		{
			name:   "a cut never raises the target",
			target: 6, next: 8,
			shed: pressureShedResult{Applied: true, Running: 10, Requested: 0, Cancelled: 2, Pool: 8},
			want: 6,
		},
		{
			name:   "the pool below the step is the floor",
			target: 100, next: 40,
			shed: pressureShedResult{Applied: true, Running: 60, Requested: 20, Cancelled: 20, Pool: 40},
			want: 40,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pressureShedTarget(tc.target, tc.next, tc.shed); got != tc.want {
				t.Fatalf("pressureShedTarget(%d, %d, %+v) = %d, want %d", tc.target, tc.next, tc.shed, got, tc.want)
			}
		})
	}
}

// The provider applies its shed backoff and no blacklist, so a shed address can
// re-enter through a normal fetch once the box recovers. In the miner that same
// backoff is the desired-set hold (dropBackoffHeldURLProxies), which is why the
// shed needs no second hold mechanism.
func TestPressureShedHoldsCancelledIdentitiesOutOfTheDesiredSet(t *testing.T) {
	withTempHome(t)
	addresses := writeURLPoolForShed(t, 3)
	pool := installShedPool(t, addresses...)

	result := shedPoolToTarget(1)
	if result.Cancelled != 2 {
		t.Fatalf("cancelled %d, want 2", result.Cancelled)
	}

	desiredSet := map[string]*connect.ProxySettings{}
	sourceOf := map[string]string{}
	cancelled := pool.cancelledKeys()
	for _, key := range cancelled {
		address, _ := connect.SplitProxyKey(key)
		desiredSet[key] = reloadTestSettings(address, "", "")
		sourceOf[key] = "url"
	}
	if held := dropBackoffHeldURLProxies(desiredSet, sourceOf, time.Now()); held != len(cancelled) {
		t.Fatalf("held %d of %d cancelled identities out of the desired set, want all of them", held, len(cancelled))
	}
	if len(desiredSet) != 0 {
		t.Fatalf("desired set still offers cancelled identities: %v", desiredSet)
	}
}

// A sustained-pressure step with no live reloader must not touch the persisted
// target. F-03: the controller wrote the AIMD step first and shed afterwards, so
// a box with nothing cancellable still ratcheted its target down every episode.
func TestPressureShedControllerLeavesTargetAloneWithoutReloader(t *testing.T) {
	dir := withTempHome(t)
	if err := os.WriteFile(filepath.Join(dir, "proxy_self_heal"), []byte("on\n"), 0600); err != nil {
		t.Fatal(err)
	}
	writeURLPoolForShed(t, 145)
	urlState, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	urlState.TargetPoolSize = 145
	if err := writeProxyURLState(urlState); err != nil {
		t.Fatal(err)
	}
	setPressureShedReloader(nil)

	cancel, done := withTempAimdController(t, 0)
	defer func() {
		cancel()
		<-done
	}()
	setPressure(0.9)
	setPressureNoCPU(0.9)

	// Several 20ms controller ticks: enough for the two high samples a cut
	// needs, and for a second cut if the first one had written a target.
	time.Sleep(250 * time.Millisecond)

	after, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if after.TargetPoolSize != 145 {
		t.Fatalf("persisted target = %d, want it left at 145: nothing was cancelled", after.TargetPoolSize)
	}
	if len(after.Cache) != 145 {
		t.Fatalf("cache size = %d, want the pool untouched at 145", len(after.Cache))
	}
}
