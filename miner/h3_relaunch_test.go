package miner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urfoundation/sn/miner/bandwidth"
	"github.com/urnetwork/connect"
)

// Tests for the two halves of the live h3 switch:
//
//   - a mode change reconnects the running identities whose eligibility moved,
//     through the reloader that owns the slots (so nothing is detached outside
//     its bookkeeping), and
//   - the desired-set read (proxyManager.desired) is side-effect free: the
//     reload watcher calls it on every reload, and a relaunch started from a
//     read used to detach slots the reloader still listed as unchanged, which
//     it then never re-spawned.

// h3ResetTestState snapshots and restores the process-global state these tests
// touch, so one test's relauncher, ranking or UDP verdicts cannot leak into the
// next (TestRelaunchNowIsANoOpWithoutARunningProvideRun asserts the seam starts
// nil).
func h3ResetTestState(t *testing.T) {
	t.Helper()
	oldState := globalControlState
	oldUDPCheck := globalProxyUDPCheck
	t.Cleanup(func() {
		globalControlState = oldState
		globalProxyUDPCheck = oldUDPCheck
		setH3IdentityRelauncher(nil)
		h3RankingValue.Store(nil)
		h3CandidatesMutex.Lock()
		h3Candidates = nil
		h3CandidatesMutex.Unlock()
		h3LaunchMutex.Lock()
		h3LaunchEligible = map[string]bool{}
		h3LaunchMutex.Unlock()
		h3AppliedSetting.Store(nil)
		resetProxyEarnBandwidthForTest()
	})
}

// h3TestControlState installs a control state whose h3 key is value, which is
// what resolveH3Mode reads on every path.
func h3TestControlState(t *testing.T, value string) {
	t.Helper()
	state := newControlState()
	if err := state.set("h3", value); err != nil {
		t.Fatal(err)
	}
	globalControlState = state
}

// h3TestUDPAdmitsAll pins the UDP gate open for the given identities: a fresh
// "relay works" verdict is remembered for each, so noting candidates starts no
// probe and eligibility never depends on a dial or on a probe racing the test.
func h3TestUDPAdmitsAll(t *testing.T, candidates []*connect.ProxySettings) {
	t.Helper()
	store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{Status: proxyUDPOK}
	})
	now := time.Now()
	for _, settings := range candidates {
		store.entries[settings.Key()] = &proxyUDPCheckEntry{
			at:     now,
			result: proxyUDPResult{Status: proxyUDPOK},
		}
	}
	globalProxyUDPCheck = store
}

// h3TestManagerProxies gives the manager a real external source file holding the
// given identities, so proxyManager.desired() builds a non-empty desired set from
// disk like the provide run's does.
func h3TestManagerProxies(t *testing.T, candidates []*connect.ProxySettings) {
	t.Helper()
	source := filepath.Join(os.Getenv("URNETWORK_STATE_DIR"), "proxies.txt")
	lines := make([]string, 0, len(candidates))
	for _, settings := range candidates {
		lines = append(lines, fmt.Sprintf("%s:%s:%s", settings.Address, settings.Auth.User, settings.Auth.Password))
	}
	if err := os.WriteFile(source, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{Source: source, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
}

// The desired-set read is pure: two reads in a row (the reload watcher's own
// cadence) neither re-rank nor relaunch anything, even with every running
// identity recorded as launched without the fallbacks and a count setting that
// would move them.
func TestDesiredReadPerformsNoRelaunch(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	h3ResetTestState(t)
	candidates := testH3Proxies(3)
	h3TestUDPAdmitsAll(t, candidates)
	h3TestManagerProxies(t, candidates)
	h3TestControlState(t, "1")
	for _, settings := range candidates {
		recordH3Launch(settings.Key(), false)
	}
	relauncher := &fakeH3Relauncher{ctx: context.Background(), busy: map[string]bool{}}
	setH3IdentityRelauncher(relauncher)

	manager := newProxyManager(defaultProxyManagerConfig())
	for i := 0; i < 2; i++ {
		launch, _, _, err := manager.desired(false)
		if err != nil {
			t.Fatalf("desired #%d: %v", i+1, err)
		}
		if len(launch) != len(candidates) {
			t.Fatalf("desired #%d read %d proxies, want %d", i+1, len(launch), len(candidates))
		}
	}

	// The ranking is the apply path's job (applyH3CandidateSet): a read that
	// built it would also have relaunched the identities it moved.
	if ranking := h3RankingValue.Load(); ranking != nil {
		t.Fatalf("desired() built the ranking %v; the read must not re-rank or relaunch", ranking.keys)
	}
	// A relaunch pass detaches its first identity immediately, so this window is
	// enough to catch one started by a read.
	time.Sleep(2 * h3RelaunchInterval)
	if detached := relauncher.detachedKeys(); len(detached) != 0 {
		t.Fatalf("desired() relaunched %v; the read must be side-effect free", detached)
	}
}

// A reload never detaches a slot it did not cancel: the reloader's own diff is
// the only thing that stops a slot, so the identity it left "unchanged" is
// still running and still in its bookkeeping afterwards.
func TestReloadDoesNotDetachASlotItDidNotCancel(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	h3ResetTestState(t)
	candidates := testH3Proxies(3)
	h3TestUDPAdmitsAll(t, candidates)
	h3TestManagerProxies(t, candidates)
	h3TestControlState(t, "1")
	for _, settings := range candidates {
		recordH3Launch(settings.Key(), false)
	}
	relauncher := &fakeH3Relauncher{ctx: context.Background(), busy: map[string]bool{}}
	setH3IdentityRelauncher(relauncher)

	var mu sync.Mutex
	spawned := 0
	reloader := newMinerProxyReloader(
		func() ([]*connect.ProxySettings, error) {
			// The provide run's own desired closure: the manager-backed set,
			// read fresh, exactly as run.go builds it.
			launch, _, _, err := newProxyManager(defaultProxyManagerConfig()).desired(false)
			return launch, err
		},
		func(settings *connect.ProxySettings) (context.CancelFunc, error) {
			mu.Lock()
			spawned++
			mu.Unlock()
			return func() {}, nil
		},
	)
	var seededDetached atomic.Bool
	seededKey := candidates[0].Key()
	reloader.seed(candidates[0], func() { seededDetached.Store(true) })

	summary, err := reloader.reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if summary.Unchanged != 1 || summary.Added != 2 || summary.Removed != 0 {
		t.Fatalf("summary = %s, want the seeded identity unchanged and the other two added", summary)
	}
	if seededDetached.Load() {
		t.Fatalf("the reload detached %s, a slot it did not cancel", seededKey)
	}
	// A relaunch issued from the read runs on its own goroutine, so give one a
	// chance to land before concluding the reload did not start it.
	time.Sleep(2 * h3RelaunchInterval)
	if detached := relauncher.detachedKeys(); len(detached) != 0 {
		t.Fatalf("the reload relaunched %v; only an apply path may relaunch", detached)
	}
	keys := reloader.runningKeys()
	if len(keys) != 3 || !containsIdentity(keys, seededKey) {
		t.Fatalf("running set = %v, want all three including the untouched %s", keys, seededKey)
	}
	// The seeded slot is still the one the reloader holds: a later reload must
	// still see it as unchanged rather than spawn a second session for it.
	before, _ := reloader.reload()
	if before.Added != 0 || before.Unchanged != 3 {
		t.Fatalf("second reload = %s, want nothing added and all three unchanged", before)
	}
}

// The apply path relaunches exactly the identities the ranking moved, through the
// running provide run's own relauncher: the changed identity is stopped and
// spawned again from the reloader's bookkeeping, and its unchanged peers are
// never touched.
func TestApplyH3CandidateSetRelaunchesOnlyChangedIdentities(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	h3ResetTestState(t)
	candidates := testH3Proxies(3)
	h3TestUDPAdmitsAll(t, candidates)
	h3TestControlState(t, "1")
	for _, settings := range candidates {
		recordH3Launch(settings.Key(), false)
	}

	var mu sync.Mutex
	cancels := map[string]int{}
	spawns := map[string]int{}
	reloader := newMinerProxyReloader(
		func() ([]*connect.ProxySettings, error) { return candidates, nil },
		func(settings *connect.ProxySettings) (context.CancelFunc, error) {
			key := settings.Key()
			mu.Lock()
			spawns[key]++
			mu.Unlock()
			return func() {
				mu.Lock()
				cancels[key]++
				mu.Unlock()
			}, nil
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, settings := range candidates {
		key := settings.Key()
		reloader.seed(settings, func() {
			mu.Lock()
			cancels[key]++
			mu.Unlock()
		})
	}
	setH3IdentityRelauncher(&minerH3Relauncher{ctx: ctx, reloader: reloader})

	// A count of one over the noted candidates: two identities leave the
	// admitted set (they were launched H1-only and stay that way) and one joins
	// it (launched H1-only, now admitted), so only that one moves.
	if !applyH3CandidateSet(candidates, h3Setting{Mode: h3ModeTop, Cap: 1}) {
		t.Fatal("a count of one over three H1-only identities must move the ranking")
	}

	ranking := h3RankingValue.Load()
	if ranking == nil || len(ranking.keys) != 1 {
		t.Fatalf("ranking = %+v, want exactly one admitted identity", ranking)
	}
	changed := ""
	for key := range ranking.keys {
		changed = key
	}

	waitFor(t, 5*time.Second, "the changed identity to be relaunched", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return cancels[changed] >= 1 && spawns[changed] >= 1
	})

	counts := func(key string) (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return cancels[key], spawns[key]
	}
	if c, s := counts(changed); c != 1 || s != 1 {
		t.Fatalf("changed identity %s: cancels=%d spawns=%d, want exactly one relaunch", changed, c, s)
	}
	for _, settings := range candidates {
		key := settings.Key()
		if key == changed {
			continue
		}
		if c, s := counts(key); c != 0 || s != 0 {
			t.Fatalf("unchanged identity %s was relaunched (cancels=%d spawns=%d)", key, c, s)
		}
	}
	if keys := reloader.runningKeys(); len(keys) != len(candidates) {
		t.Fatalf("running set = %v, want the relaunched identity back alongside its peers", keys)
	}

	// Re-applying the same set moves nothing, so it must not start a second
	// pass over identities that are already where they belong.
	if applyH3CandidateSet(candidates, h3Setting{Mode: h3ModeTop, Cap: 1}) {
		t.Fatal("re-applying an unchanged candidate set must not report a ranking move")
	}
	time.Sleep(2 * h3RelaunchInterval)
	if c, s := counts(changed); c != 1 || s != 1 {
		t.Fatalf("an unchanged re-apply relaunched %s again (cancels=%d spawns=%d)", changed, c, s)
	}
}

// The operator's own entry point — `urnet-tools set h3 ...` through the control
// socket's live side effect — reconnects the identities the ranking moved and
// reports no restart. Without a published relauncher this was the F-04 defect:
// the command returned OK and nothing happened.
func TestH3ControlKeyRelaunchesTheMovedIdentitiesLive(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	h3ResetTestState(t)
	candidates := testH3Proxies(3)
	h3TestUDPAdmitsAll(t, candidates)
	for _, settings := range candidates {
		recordH3Launch(settings.Key(), false)
	}
	// What the provide run does at startup: note the served candidates under
	// the setting in force, so a count set later ranks the real pool.
	applyH3CandidateSet(candidates, h3Setting{Mode: h3ModeDirect})
	fake := &fakeH3Relauncher{ctx: context.Background(), busy: map[string]bool{}}
	setH3IdentityRelauncher(fake)

	// Same sequence the control socket runs for a `set`: persist the value into
	// the shared control state, then apply it live.
	state := newControlState()
	globalControlState = state
	for _, value := range []string{"direct", "1"} {
		if err := state.set("h3", value); err != nil {
			t.Fatalf("set h3 %s: %v", value, err)
		}
		if err := applyControlLiveSideEffect("h3", value); err != nil {
			t.Fatalf("apply h3 %s: %v", value, err)
		}
	}
	if controlNeedsRestart("h3") {
		t.Fatal("h3 is live; the command must not report needs_restart")
	}

	ranking := h3RankingValue.Load()
	if ranking == nil || len(ranking.keys) != 1 {
		t.Fatalf("ranking = %+v, want exactly one admitted identity", ranking)
	}
	changed := ""
	for key := range ranking.keys {
		changed = key
	}
	waitFor(t, 5*time.Second, "the admitted identity to be relaunched", func() bool {
		return len(fake.detachedKeys()) >= 1
	})
	// A pass paces its detaches, so give the second one time to show up if the
	// change wrongly moved an identity that stayed put.
	time.Sleep(2 * h3RelaunchInterval)
	detached := fake.detachedKeys()
	if len(detached) != 1 || detached[0] != changed {
		t.Fatalf("set h3 1 relaunched %v, want only the admitted identity %s", detached, changed)
	}
}

// The provide run publishes its relauncher at startup and clears it on the way
// out. This is the piece F-04 was missing: the seam existed, but nothing in the
// provide run ever installed it, so `set h3 ...` had no effect to apply.
func TestInstallProvideH3RelauncherPublishesAndClearsTheSeam(t *testing.T) {
	h3ResetTestState(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A profile run has no reloader and must publish nothing: it owns no slot
	// bookkeeping, so it cannot honestly detach an identity.
	clear := installProvideH3Relauncher(ctx, nil)
	if currentH3IdentityRelauncher() != nil {
		t.Fatal("a run without a reloader must not publish a relauncher")
	}
	clear()
	if currentH3IdentityRelauncher() != nil {
		t.Fatal("clearing a run that published nothing must not publish one")
	}

	reloader := newMinerProxyReloader(
		func() ([]*connect.ProxySettings, error) { return nil, nil },
		func(*connect.ProxySettings) (context.CancelFunc, error) { return func() {}, nil },
	)
	clear = installProvideH3Relauncher(ctx, reloader)
	relauncher := currentH3IdentityRelauncher()
	if relauncher == nil {
		t.Fatal("the provide run must publish its relauncher")
	}
	if relauncher.runContext() != ctx {
		t.Fatal("the relauncher must run with the provide run's context")
	}
	if relauncher.detachForRelaunch("not-a-running-identity") {
		t.Fatal("detaching an identity this run does not serve must report false")
	}
	clear()
	if currentH3IdentityRelauncher() != nil {
		t.Fatal("the run must clear its relauncher on the way out")
	}
}

// hasClients is what keeps a relaunch from cutting a live session: it reads the
// identity's published counters, so a proxy serving clients is left alone.
func TestH3IdentityHasClientsReadsTheLiveCounters(t *testing.T) {
	h3ResetTestState(t)
	key := testH3Proxies(1)[0].Key()
	if h3IdentityHasClients(key) {
		t.Fatal("an identity that publishes no counter is not running, so it is not busy")
	}
	bw := &bandwidth.ProxyBandwidth{}
	publishProxyEarnBandwidth(key, bw)
	t.Cleanup(func() { unpublishProxyEarnBandwidth(key) })
	if h3IdentityHasClients(key) {
		t.Fatal("an idle identity must not count as busy")
	}
	bw.Clients.Store(2)
	if !h3IdentityHasClients(key) {
		t.Fatal("an identity with clients must count as busy")
	}
}
