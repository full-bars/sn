package provider

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// TestReload_URLOnlySource_NoEarlyExit is a regression test for a bug found
// during live deployment testing: when there is no --proxy_file and no
// internal proxies (Workflow A/B empty), reload() used to bail out before
// ever merging in the URL-sourced cache, so a URL-only deployment could
// never start any proxies via hot-reload.
func TestReload_URLOnlySource_NoEarlyExit(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"5.5.5.5:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: cancelMapMu,
		state:       state,
		sourcePath:  "",
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}

	reloader.reload()

	cancelMapMu.Lock()
	_, started := reloader.cancelMap["5.5.5.5:1080"]
	cancelMapMu.Unlock()
	if !started {
		t.Fatal("expected URL-sourced proxy to be started by reload() even with no file/internal proxies configured")
	}
}

// TestReload_AddedProxies_NoPerProxyEnumeration verifies reload() does NOT
// print one line per added proxy. The per-proxy enumeration was found in fleet
// log analysis to be a dominant ramlog flooder: a reload of hundreds/thousands
// of (mostly dead, churning) proxies dumped that many lines per reload via raw
// fmt.Printf, flushing high-value lines ([profit]/[earn]/[contract]) out of the
// small in-RAM buffer within seconds. The terse "[proxy] reloaded: +N -M"
// summary carries the operator-relevant signal; the per-proxy roster on every
// reload is noise. (The cold-start "Using N proxy servers:" roster is kept; it
// prints once, before any earning, and documents what the provider started
// with.)
func TestReload_AddedProxies_NoPerProxyEnumeration(t *testing.T) {
	withTempHome(t)

	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	tmpFile := t.TempDir() + "/proxy.txt"
	if err := os.WriteFile(tmpFile, []byte("5.5.5.5:1080:alice:secret\n6.6.6.6:1080:bob:hunter2\n"), 0600); err != nil {
		t.Fatal(err)
	}

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: cancelMapMu,
		state:       state,
		sourcePath:  tmpFile,
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	reloader.reload()
	os.Stdout = origStdout
	w.Close()
	out, _ := io.ReadAll(r)

	got := string(out)
	if !strings.Contains(got, "reloaded: +2 added") {
		t.Fatalf("expected reload to print the summary count, got: %q", got)
	}
	if strings.Contains(got, "5.5.5.5:1080") || strings.Contains(got, "6.6.6.6:1080") {
		t.Fatalf("reload must NOT enumerate each added proxy address (ramlog flood), got: %q", got)
	}
}

// TestReload_AddedProxies_UseJitteredBackoffPacer is a regression test for a
// second issue found in the same live deployment test: reload()'s "start
// added proxies" loop staggered startups by a fixed, unjittered 100ms * i —
// ten times faster than the jittered ~1s default used by the initial startup
// path (backoffPacer in main.go). A large batch merged in at once (e.g.
// hundreds of proxies from a URL source) would burst far faster than a fresh
// process start, overwhelming the auth API with simultaneous requests.
// reload() must use the same backoffPacer so a hot-reload ramp-up is exactly
// as gradual as a cold start.
func TestReload_AddedProxies_UseJitteredBackoffPacer(t *testing.T) {
	withTempHome(t)
	// This test verifies the URL-source stagger timing only. Disable the
	// native [direct] transport so it does not add a 7th spawn to the count
	// (reload() spawns direct as an extra proxy when enabled). DISABLE_DIRECT_IP
	// is the startup default toggle; the direct-feature commit added this path.
	t.Setenv("DISABLE_DIRECT_IP", "1")

	urlCache := map[string]ProxyURLEntry{}
	for i := range 6 {
		urlCache[fmt.Sprintf("10.0.0.%d:1080", i)] = ProxyURLEntry{}
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: urlCache}); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	var spawnCount atomic.Int32
	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: cancelMapMu,
		state:       state,
		sourcePath:  "",
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			spawnCount.Add(1)
			<-proxyCtx.Done()
		},
		drainingProxies: make(map[string]context.CancelFunc),
	}

	reloader.reload()

	// URL proxies use 500ms stagger. Position 5 at 500ms*5 = 2500ms base
	// with ±250ms jitter = [2250, 2750]ms. Wait 3000ms, all 6 should be up.
	time.Sleep(3000 * time.Millisecond)
	if got := spawnCount.Load(); got != 6 {
		t.Fatalf("spawnProxy calls after 3000ms: got %d, want 6 (URL stagger is 500ms, position 5 starts at ~2500ms)", got)
	}
}

// TestReload_WarmupGate_DefersThenLaunchesURLProxies verifies that URL-
// sourced proxies are deferred during warmup and launched once warmup
// completes, confirming the warmup gate + reload trigger work together.
func TestReload_WarmupGate_DefersThenLaunchesURLProxies(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"9.9.9.9:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	// Start with warmup NOT done — URL proxies should be deferred
	proxyWarmupDone.Store(false)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: cancelMapMu,
		state:       state,
		sourcePath:  "",
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}

	// First reload — warmup not done, URL proxy should be deferred
	reloader.reload()

	cancelMapMu.Lock()
	_, deferred := reloader.cancelMap["9.9.9.9:1080"]
	cancelMapMu.Unlock()
	if deferred {
		t.Fatal("URL proxy must NOT be launched during warmup")
	}

	// Mark warmup done and trigger a reload
	proxyWarmupDone.Store(true)
	if reloadPath, err := proxyReloadPath(); err == nil {
		_ = writeReloadTrigger(reloadPath)
	}
	reloader.reload()

	cancelMapMu.Lock()
	_, launched := reloader.cancelMap["9.9.9.9:1080"]
	cancelMapMu.Unlock()
	if !launched {
		t.Fatal("URL proxy must be launched after warmup completes")
	}
}

// TestReload_PrunesGhostStateEntries_NotRunningNotDesired is a regression
// test for the LA2 ghost-proxy incident: a proxy that is both no longer
// running (already offline before this reload — never in cancelMap) and no
// longer desired (removed from the source, e.g. by `proxy remove-dead`)
// must be pruned from proxy.state. Before this fix, reload()'s prune only
// covered addresses in running-but-not-desired, so an already-dead address
// removed from the source was never in `running` to begin with and its
// ghost entry in state.Proxies was never deleted — it accumulated forever
// and was re-reported by every subsequent `remove-dead` run.
func TestReload_PrunesGhostStateEntries_NotRunningNotDesired(t *testing.T) {
	withTempHome(t)

	tmpFile := t.TempDir() + "/proxy.txt"
	if err := os.WriteFile(tmpFile, []byte("1.1.1.1:1080:alice:secret\n"), 0600); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {ID: 1, Health: "up"},               // still desired — must survive
		"9.8.7.6:1080": {ID: 2, Health: "recently_offline"}, // removed from source, never running — must be pruned
	}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: cancelMapMu,
		state:       state,
		sourcePath:  tmpFile,
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}

	reloader.reload()

	after, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Proxies["9.8.7.6:1080"]; ok {
		t.Fatal("ghost entry (not running, not desired) should have been pruned from proxy.state")
	}
	// The still-desired entry was seeded under its bare address (legacy
	// shape); the reload migrates it to its identity key (address+user).
	key := (&connect.ProxySettings{Network: "tcp", Address: "1.1.1.1:1080", Auth: &proxy.Auth{User: "alice"}}).Key()
	kept, ok := after.Proxies[key]
	if !ok {
		t.Fatal("still-desired proxy's state entry must survive reload (adopted under its identity key)")
	}
	if kept.ID != 1 {
		t.Fatalf("expected still-desired proxy to keep its original ID 1, got %d", kept.ID)
	}
	if kept.Health != "up" {
		t.Fatalf("expected still-desired proxy's persisted Health %q to survive reload, got %q", "up", kept.Health)
	}
	if kept.Source != "file" {
		t.Fatalf("expected still-desired proxy to be tagged source=file, got %q", kept.Source)
	}
}

// TestReload_PreservesBackoffURLProxyState verifies the ghost-prune added
// above does not delete state entries for URL-sourced proxies that are
// mid give-up-backoff: such a proxy is not running (its goroutine already
// exited on give-up) but IS still desired, because mergeProxyURLCache keeps
// it in the URL cache until it is explicitly evicted. Pruning must key off
// desiredSet, not the running set, or this case would wipe ID/health
// history for every proxy currently backing off.
func TestReload_PreservesBackoffURLProxyState(t *testing.T) {
	withTempHome(t)

	addr := "5.6.7.8:1080"
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		addr: {},
	}}); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{Proxies: map[string]ProxyEntry{
		addr: {ID: 7, Health: "dead", Source: "url"},
	}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	// Put the address in backoff so reload() must not relaunch it — it
	// stays absent from `running` for this whole reload, same as a
	// give-up'd proxy in production.
	globalProxyFailureHistory.SetBackoffUntil(addr, time.Now().Add(time.Hour))
	t.Cleanup(func() { globalProxyFailureHistory.Reset(addr) })

	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: cancelMapMu,
		state:       state,
		sourcePath:  "",
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}

	reloader.reload()

	cancelMapMu.Lock()
	_, launched := reloader.cancelMap[addr]
	cancelMapMu.Unlock()
	if launched {
		t.Fatal("test setup invalid: address in backoff should not have been launched")
	}

	after, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Proxies[addr]; !ok {
		t.Fatal("state entry for a URL proxy in give-up backoff must survive reload (still desired via URL cache)")
	}
}

// TestReload_SkipsPruneOnURLCacheReadFailure is a regression test for the ghost-prune above: if proxy_url.json fails to
// read (corrupt file, transient I/O error — NOT the normal "no URL sources
// configured" case, which returns an empty cache with no error), desiredSet
// silently excludes every URL-sourced address for that reload cycle. Pruning
// against that incomplete desiredSet would delete state entries for
// still-desired URL proxies, including ones mid give-up-backoff, over a
// transient hiccup. The prune must be skipped entirely for cycles where the
// URL cache failed to load.
func TestReload_SkipsPruneOnURLCacheReadFailure(t *testing.T) {
	home := withTempHome(t)

	urlPath := filepath.Join(home, ".urnetwork", "proxy_url.json")
	if err := os.MkdirAll(filepath.Dir(urlPath), 0700); err != nil {
		t.Fatal(err)
	}
	// Malformed JSON makes readProxyURLState() return an error (distinct
	// from a missing file, which is treated as "no URL sources").
	if err := os.WriteFile(urlPath, []byte("{not valid json"), 0600); err != nil {
		t.Fatal(err)
	}

	tmpFile := t.TempDir() + "/proxy.txt"
	if err := os.WriteFile(tmpFile, []byte("1.1.1.1:1080:alice:secret\n"), 0600); err != nil {
		t.Fatal(err)
	}

	urlAddr := "5.6.7.8:1080"
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {ID: 1, Health: "up", Source: "file"},
		urlAddr:        {ID: 2, Health: "dead", Source: "url"},
	}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: cancelMapMu,
		state:       state,
		sourcePath:  tmpFile,
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}

	reloader.reload()

	after, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Proxies[urlAddr]; !ok {
		t.Fatal("URL proxy's state entry must survive a reload where proxy_url.json failed to read, even though it's absent from this cycle's desiredSet")
	}
}

func TestWriteReloadTrigger_DebounceSuppressesRapidWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.reload")

	// Enable debounce at 500ms for fast test
	writeReloadTriggerDebounce = 500 * time.Millisecond
	t.Cleanup(func() { writeReloadTriggerDebounce = 0 })
	lastReloadTriggerTime.ts = time.Time{}

	// First write succeeds
	if err := writeReloadTrigger(path); err != nil {
		t.Fatal(err)
	}
	seq1, _ := readReloadSeq(path)
	if seq1 != 1 {
		t.Fatalf("first write: expected seq 1, got %d", seq1)
	}

	// Second write within debounce window is suppressed
	if err := writeReloadTrigger(path); err != nil {
		t.Fatal(err)
	}

	// After debounced second write: trailing edge scheduled but not yet fired → seq still 1
	seqAfter := func() int { s, _ := readReloadSeq(path); return s }()
	if seqAfter != 1 {
		t.Fatalf("expected seq 1 (trailing not yet fired), got %d", seqAfter)
	}

	// Wait for trailing edge to fire
	time.Sleep(600 * time.Millisecond)
	seqAfter = func() int { s, _ := readReloadSeq(path); return s }()
	if seqAfter != 2 {
		t.Fatalf("expected seq 2 (trailing edge fired), got %d", seqAfter)
	}
}

func TestWriteReloadTrigger_TrailingEdgeFires(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.reload")

	writeReloadTriggerDebounce = 100 * time.Millisecond
	t.Cleanup(func() { writeReloadTriggerDebounce = 0 })
	lastReloadTriggerTime.ts = time.Time{}

	// First write
	if err := writeReloadTrigger(path); err != nil {
		t.Fatal(err)
	}

	// Second write within window — suppressed, schedules trailing edge
	if err := writeReloadTrigger(path); err != nil {
		t.Fatal(err)
	}

	// Sequence should still be 1 (trailing edge hasn't fired yet)
	seq, _ := readReloadSeq(path)
	if seq != 1 {
		t.Fatalf("expected seq 1 (trailing not yet fired), got %d", seq)
	}

	// Wait for trailing edge + some margin
	time.Sleep(200 * time.Millisecond)

	seq, _ = readReloadSeq(path)
	if seq != 2 {
		t.Fatalf("expected seq 2 (trailing edge fired), got %d", seq)
	}
}

func TestDoWriteReloadTrigger_IncrementsSequence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.reload")

	if err := doWriteReloadTrigger(path); err != nil {
		t.Fatal(err)
	}
	seq, _ := readReloadSeq(path)
	if seq != 1 {
		t.Fatalf("expected seq 1, got %d", seq)
	}

	if err := doWriteReloadTrigger(path); err != nil {
		t.Fatal(err)
	}
	seq, _ = readReloadSeq(path)
	if seq != 2 {
		t.Fatalf("expected seq 2, got %d", seq)
	}
	// Confirm doWriteReloadTrigger bypasses debounce (no suppression)
}

// TestRunReloadReconciler_FiresOnInterval is a regression test for a fleet
// incident: a mass-failure event (a transient backend outage) can leave a
// batch of still-desired proxies stuck out of the running set with no future
// event scheduled to bring them back, since reload() only runs on an
// explicit trigger. Confirmed live: ~3300 proxies sat recently_offline for
// ~20+ hours on a production node until an unrelated add-source call forced
// a reload. runReloadReconciler is the safety net — it must fire a reload
// trigger on its own cadence regardless of whether anything else requested
// one, and must stop cleanly when its context is cancelled.
func TestRunReloadReconciler_FiresOnInterval(t *testing.T) {
	withTempHome(t)

	lastReloadTriggerTime.Lock()
	origTs := lastReloadTriggerTime.ts
	origPending := lastReloadTriggerTime.pending
	lastReloadTriggerTime.ts = time.Time{}
	lastReloadTriggerTime.Unlock()

	origInterval := reconciliationReloadInterval
	reconciliationReloadInterval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runReloadReconciler(ctx)
		close(done)
	}()

	// Registered after the goroutine starts so it always runs, including on
	// a t.Fatal from the polling loop below — otherwise the reconciler
	// goroutine would leak and the debounce state would stay mutated for
	// later tests.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("expected runReloadReconciler to return promptly after context cancellation")
		}
		reconciliationReloadInterval = origInterval
		lastReloadTriggerTime.Lock()
		lastReloadTriggerTime.ts = origTs
		lastReloadTriggerTime.pending = origPending
		lastReloadTriggerTime.Unlock()
	})

	reloadPath, err := proxyReloadPath()
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		seq, _ := readReloadSeq(reloadPath)
		if seq >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expected runReloadReconciler to write at least one reload trigger before the deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A reload that re-resolves an active cap must also reconnect the running
// identities whose membership changed. The cap ranking shifts as the desired
// set changes (a replacement proxy ranks in, or the pool shrinks), and the
// proxies that join or leave the top N only pick up the new H3 decision if the
// reapply runs -- the same path SetH3Mode uses. Without it, a proxy displaced
// from the cap keeps running H3 (N+1 proxies under a cap of N) and a promoted
// proxy stays on H1 until the next control update.
//
// The test drives the real ProxyReloader.reload() twice: first with only P1 in
// the source, then with P2 added and outranking P1 for the single slot. It
// asserts P1's tracked running entry flips to not-eligible, which only
// reapplyH3ModeLive does. Dropping the reapply from the reload's cap
// re-resolution leaves P1 tracked and eligible, failing this test.
func TestProxyReloadReResolvesActiveCap(t *testing.T) {
	withTempHome(t)

	// proxy_url.json is absent, which reads as an empty cache with no error, so
	// the reload's candidate publication (and cap re-resolution) is not gated
	// off this cycle.
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}

	// Distinct keys so the ranking tie-break (by key) is deterministic: with
	// both proxies ungraded and scoreless, the lexicographically smaller key
	// wins the slot. P2's address sorts ahead of P1's.
	p1 := &connect.ProxySettings{
		Network: "tcp",
		Address: "10.0.0.2:8080",
		Auth:    &proxy.Auth{User: "u1", Password: "p1"},
	}
	p2 := &connect.ProxySettings{
		Network: "tcp",
		Address: "10.0.0.1:8080",
		Auth:    &proxy.Auth{User: "u1", Password: "p1"},
	}
	p1Key, p2Key := p1.Key(), p2.Key()

	src := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(src, []byte("10.0.0.2:8080:u1:p1\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Wire the reloader and the live re-apply exactly as provideLauncherLoop
	// does: the reloader shares the provideState's cancel map and mutex, and
	// h3ReapplyLive reconnects through that same state.
	st := &provideState{proxyCancelMap: map[string]context.CancelFunc{}}
	cancelMapKeys := func() []string {
		st.proxyCancelMu.Lock()
		defer st.proxyCancelMu.Unlock()
		out := make([]string, 0, len(st.proxyCancelMap))
		for k := range st.proxyCancelMap {
			out = append(out, k)
		}
		return out
	}
	reloader := &ProxyReloader{
		cancelMap:       st.proxyCancelMap,
		cancelMapMu:     &st.proxyCancelMu,
		state:           &ProxyState{Proxies: map[string]ProxyEntry{}},
		sourcePath:      src,
		parentCtx:       context.Background(),
		wg:              &sync.WaitGroup{},
		drainingProxies: map[string]context.CancelFunc{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
	}
	h3ReapplyLiveMu.Lock()
	origReapply := h3ReapplyLive
	h3ReapplyLiveMu.Unlock()
	installH3ReapplyLive(func() { reapplyH3ModeLive(st) })
	defer installH3ReapplyLive(origReapply)

	// Save/restore the published candidate set and the mode for other tests.
	h3ProxyCandidatesMu.Lock()
	prevSet, prevKnown := h3ProxyCandidatesSet, h3ProxyCandidatesKnown
	h3ProxyCandidatesMu.Unlock()
	defer func() {
		h3ProxyCandidatesMu.Lock()
		h3ProxyCandidatesSet, h3ProxyCandidatesKnown = prevSet, prevKnown
		h3ProxyCandidatesMu.Unlock()
	}()
	publishH3ProxyCandidates(nil)
	prevMode, err := SetH3Mode("1")
	if err != nil {
		t.Fatalf("SetH3Mode(1): %v", err)
	}
	defer SetH3Mode(prevMode)

	// First reload: only P1 is desired, so the cap resolves over P1 and P1 is
	// launched. Simulate the launched goroutine registering its H3 running
	// entry (the fake spawnProxy does not).
	reloader.reload()
	st.proxyCancelMu.Lock()
	_, p1Running := st.proxyCancelMap[p1Key]
	st.proxyCancelMu.Unlock()
	if !p1Running {
		t.Fatalf("reload did not launch p1 (cancel map keys: %v)", cancelMapKeys())
	}
	registerH3Running(p1Key, true)
	defer unregisterH3Running(p1Key)
	if !h3EligibleForKey(p1Key, false) {
		t.Fatal("p1 should hold the single cap slot after the first reload")
	}

	// Second reload: P2 is added and outranks P1 for the single slot, so P1 is
	// displaced and must be reconnected (cancelled and re-spawned on H1).
	if err := os.WriteFile(src, []byte("10.0.0.2:8080:u1:p1\n10.0.0.1:8080:u1:p1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reloader.reload()

	if !h3EligibleForKey(p2Key, false) {
		t.Fatal("p2 should take the cap slot after the reload re-resolves over the new set")
	}
	if h3EligibleForKey(p1Key, false) {
		t.Fatal("p1 should be displaced from the cap slot")
	}
	eligible, tracked := h3RunningEligibleOf(p1Key)
	if !tracked || eligible {
		t.Fatalf("p1 running entry after reload = tracked:%v eligible:%v; want tracked:true eligible:false — the reload re-resolved the cap but did not reconnect the displaced proxy", tracked, eligible)
	}
}
