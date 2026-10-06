package provider

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// registerH3RunningEligible must read the identity's eligibility and record the
// running entry in ONE critical section. The test holds h3RunningMu so the
// registrar blocks, changes the mode while it is blocked, then releases the
// lock. With the read inside the critical section the registrar observes the
// new mode and records not-eligible. With the read outside the lock (the
// round-2 shape at provide.go) it has already read the old, eligible value and
// records it after the mode moved — the exact interleaving that lets a
// concurrent re-apply/reload skip the still-untracked key and leaves the
// identity on the wrong side of the cap until the next control update.
func TestRegisterH3RunningEligibleAtomicWithModeChange(t *testing.T) {
	previous := H3ModeName()
	defer SetH3Mode(previous)

	// This test drives the registrar directly; keep any hook another test
	// installed out of the way (SetH3Mode below would otherwise run it).
	h3ReapplyLiveMu.Lock()
	origReapply := h3ReapplyLive
	h3ReapplyLiveMu.Unlock()
	installH3ReapplyLive(nil)
	defer installH3ReapplyLive(origReapply)

	proxySettings := &connect.ProxySettings{
		Network: "tcp",
		Address: "10.9.9.9:8080",
		Auth:    &proxy.Auth{User: "u", Password: "p"},
	}
	key := proxySettings.Key()
	unregisterH3Running(key)
	defer unregisterH3Running(key)

	if _, err := SetH3Mode("all"); err != nil {
		t.Fatal(err)
	}
	if !h3EligibleForKey(key, false) {
		t.Fatal("`all` must make the probe proxy eligible")
	}

	h3RunningMu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			h3RunningMu.Unlock()
		}
	}()

	type result struct {
		eligible bool
		launch   uint64
	}
	done := make(chan result, 1)
	go func() {
		_, eligible, launch := registerH3RunningEligible(proxySettings, false)
		done <- result{eligible, launch}
	}()

	// Give the registrar time to reach the lock. With the read inside the lock
	// it blocks before reading; with the read outside (the defect) it has
	// already read the eligible value by the time it blocks here.
	time.Sleep(200 * time.Millisecond)

	// Flip the mode while the registrar is blocked, then let it proceed.
	if _, err := SetH3Mode("off"); err != nil {
		t.Fatal(err)
	}
	h3RunningMu.Unlock()
	unlocked = true

	res := <-done
	if res.eligible {
		t.Fatal("registrar recorded eligible=true after the mode was changed to off: " +
			"it read the eligibility outside the registration lock, so a concurrent " +
			"re-apply/reload can skip the untracked key and strand the identity on the " +
			"wrong side of the cap")
	}
	elig, tracked := h3RunningEligibleOf(key)
	if !tracked || elig {
		t.Fatalf("registered running entry = tracked:%v eligible:%v, want tracked:true eligible:false", tracked, elig)
	}
}

// The reload path must register the PROMOTED proxy through the real
// registration function and track it as eligible, and the displaced proxy must
// leave the set, so the running H3 proxy count stays at the cap across a
// promotion. The spawn hook here is the real provideWithProxy registration
// shape: evaluate-and-register atomically, release only its own launch on the
// way out. Removing the reload's re-apply leaves the displaced proxy tracked
// and eligible and the count at two under a cap of one.
func TestProxyReloadPromotedProxyRegisteredTracked(t *testing.T) {
	withTempHome(t)

	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}

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
	defer func() { unregisterH3Running(p1Key); unregisterH3Running(p2Key) }()

	src := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(src, []byte("10.0.0.2:8080:u1:p1\n"), 0600); err != nil {
		t.Fatal(err)
	}

	st := &provideState{proxyCancelMap: map[string]context.CancelFunc{}}
	// A cancellable parent, cancelled and drained on the way out: the spawned
	// goroutines block on proxyCtx.Done() and would otherwise outlive the test.
	parentCtx, parentCancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	t.Cleanup(func() {
		parentCancel()
		wg.Wait()
	})
	reloader := &ProxyReloader{
		cancelMap:       st.proxyCancelMap,
		cancelMapMu:     &st.proxyCancelMu,
		state:           &ProxyState{Proxies: map[string]ProxyEntry{}},
		sourcePath:      src,
		parentCtx:       parentCtx,
		wg:              wg,
		drainingProxies: map[string]context.CancelFunc{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			// The real launch registration path, not a manual registerH3Running.
			key, _, launch := registerH3RunningEligible(settings, isNative)
			defer unregisterH3RunningIfCurrent(key, launch)
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

	base := H3ProxySetSize()

	// First reload: only P1 is desired, so it launches and registers eligible.
	reloader.reload()
	waitFor(t, "p1 tracked eligible after the first reload", func() bool {
		eligible, tracked := h3RunningEligibleOf(p1Key)
		return tracked && eligible
	})

	// Second reload: P2 is added and outranks P1 for the single slot. The cap
	// re-resolve promotes P2 and displaces P1, reconnecting P1.
	if err := os.WriteFile(src, []byte("10.0.0.2:8080:u1:p1\n10.0.0.1:8080:u1:p1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reloader.reload()

	waitFor(t, "p2 tracked eligible after the cap re-resolve", func() bool {
		eligible, tracked := h3RunningEligibleOf(p2Key)
		return tracked && eligible
	})
	if eligible, tracked := h3RunningEligibleOf(p1Key); tracked && eligible {
		t.Fatal("p1 stayed tracked+eligible after being displaced from the cap; " +
			"the reload re-resolved the cap but did not reconnect the displaced proxy")
	}
	if got := H3ProxySetSize() - base; got > 1 {
		t.Fatalf("H3 proxy set size grew by %d across the promotion, want at most the cap of 1; "+
			"a displaced proxy was not reconnected", got)
	}
}

// A reload that re-resolves the cap to the SAME proxy set must be a no-op:
// sameH3EligibleKeys short-circuits so the live re-apply does not run and no
// running proxy is cancelled. Removing the guard lets the CAS succeed and run
// the re-apply for an unchanged set.
func TestReResolveActiveH3CapSameKeysIsNoOp(t *testing.T) {
	previous := H3ModeName()
	defer SetH3Mode(previous)

	h3ProxyCandidatesMu.Lock()
	prevSet, prevKnown := h3ProxyCandidatesSet, h3ProxyCandidatesKnown
	h3ProxyCandidatesMu.Unlock()
	defer func() {
		h3ProxyCandidatesMu.Lock()
		h3ProxyCandidatesSet, h3ProxyCandidatesKnown = prevSet, prevKnown
		h3ProxyCandidatesMu.Unlock()
	}()
	h3ReapplyLiveMu.Lock()
	origReapply := h3ReapplyLive
	h3ReapplyLiveMu.Unlock()
	defer installH3ReapplyLive(origReapply)

	c1 := &connect.ProxySettings{
		Network: "tcp",
		Address: "10.4.4.4:8080",
		Auth:    &proxy.Auth{User: "u", Password: "p"},
	}
	c1Key := c1.Key()
	publishH3ProxyCandidates([]*connect.ProxySettings{c1})
	if _, err := SetH3Mode("1"); err != nil {
		t.Fatal(err)
	}
	// Register c1 as a live, tracked running identity (it is in the cancel map
	// below). Without this the key is untracked, so reapplyH3ModeLive skips it
	// regardless of the guard and the cancelled assertion pins nothing.
	registerH3Running(c1Key, true)
	defer unregisterH3Running(c1Key)

	var reapplies int32
	cancelled := false
	st := &provideState{proxyCancelMap: map[string]context.CancelFunc{
		c1Key: func() { cancelled = true },
	}}
	installH3ReapplyLive(func() {
		atomic.AddInt32(&reapplies, 1)
		reapplyH3ModeLive(st)
	})

	reResolveActiveH3Cap()

	if got := atomic.LoadInt32(&reapplies); got != 0 {
		t.Fatalf("reResolveActiveH3Cap ran the re-apply %d times for an unchanged cap set; "+
			"the sameH3EligibleKeys no-op guard did not short-circuit", got)
	}
	if cancelled {
		t.Fatal("a running proxy was cancelled for an unchanged cap set")
	}
	// The tracked proxy must be left exactly as it was: the guard protects a
	// LIVE identity, not merely an untracked cancel-map entry.
	if eligible, tracked := h3RunningEligibleOf(c1Key); !tracked || !eligible {
		t.Fatalf("the unchanged cap set changed the tracked proxy's running entry = tracked:%v "+
			"eligible:%v; a live identity was cancelled or dropped despite no membership change",
			tracked, eligible)
	}
}

// reResolveActiveH3Cap swaps the stored mode with a compare-and-swap against the
// pointer it read. A control-socket `h3` update that lands between that read and
// the swap replaces the pointer, so the CAS fails and the operator's newer mode
// must be preserved: the stale re-resolve must NOT overwrite it and must NOT
// reconnect anyone. The window is opened by holding h3ProxyCandidatesMu —
// buildH3ResolvedMode reads the candidates under that lock, so the re-resolve
// blocks there after it has read the mode — and the control update is landed
// while it is blocked.
func TestReResolveActiveH3CapLosesToConcurrentControlUpdate(t *testing.T) {
	withTempHome(t)

	h3ReapplyLiveMu.Lock()
	origReapply := h3ReapplyLive
	h3ReapplyLiveMu.Unlock()
	var reapplies int32
	installH3ReapplyLive(func() { atomic.AddInt32(&reapplies, 1) })
	defer installH3ReapplyLive(origReapply)

	h3ProxyCandidatesMu.Lock()
	prevSet, prevKnown := h3ProxyCandidatesSet, h3ProxyCandidatesKnown
	h3ProxyCandidatesMu.Unlock()
	defer func() {
		h3ProxyCandidatesMu.Lock()
		h3ProxyCandidatesSet, h3ProxyCandidatesKnown = prevSet, prevKnown
		h3ProxyCandidatesMu.Unlock()
	}()
	prevMode := currentH3Mode()
	defer h3ModeValue.Store(prevMode)

	// The mode the re-resolve reads: a cap whose stored set is {"m1-key"}. The
	// published candidates resolve the same cap to a DIFFERENT key, so the
	// re-resolve does not short-circuit on sameH3EligibleKeys and reaches the CAS.
	h3ModeValue.Store(&h3ResolvedMode{
		kind:         h3ModeCap,
		cap:          1,
		raw:          "1",
		eligibleKeys: map[string]bool{"m1-key": true},
	})
	cand := &connect.ProxySettings{Network: "tcp", Address: "10.6.6.6:8080"}
	if cand.Key() == "m1-key" {
		t.Fatal("test premise: the candidate key must differ from the stale resolved key")
	}
	publishH3ProxyCandidates([]*connect.ProxySettings{cand})

	// Freeze buildH3ResolvedMode at its candidate read.
	h3ProxyCandidatesMu.Lock()
	done := make(chan struct{})
	go func() {
		reResolveActiveH3Cap()
		close(done)
	}()
	// Let the re-resolve read the cap mode and block on the candidate lock.
	time.Sleep(250 * time.Millisecond)

	// The operator's concurrent control update, landing in the read→CAS window.
	operator := &h3ResolvedMode{kind: h3ModeDirect, raw: h3ModeDirectName}
	h3ModeValue.Store(operator)
	h3ProxyCandidatesMu.Unlock()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reResolveActiveH3Cap did not return")
	}

	if got := currentH3Mode(); got != operator {
		t.Fatalf("reResolveActiveH3Cap overwrote the concurrent control update: stored mode is now "+
			"%q (%p), want the operator's %q (%p); the CAS-failure branch did not abort the stale "+
			"re-resolve and clobbered the operator's newer mode",
			got.name(), got, operator.name(), operator)
	}
	if got := atomic.LoadInt32(&reapplies); got != 0 {
		t.Fatalf("reResolveActiveH3Cap ran the live reconnect %d times after losing the CAS; "+
			"the stale re-resolve must abort without reconnecting anyone", got)
	}
}

// The live re-apply hook is installed by the launcher while the control socket
// and the reload goroutine run and read it, so install and run must be
// synchronized. Run under -race: reverting to a bare unsynchronized variable
// (the round-2 shape) trips the detector here.
func TestH3ReapplyLiveInstallAndRunAreSynchronized(t *testing.T) {
	h3ReapplyLiveMu.Lock()
	orig := h3ReapplyLive
	h3ReapplyLiveMu.Unlock()
	defer installH3ReapplyLive(orig)

	var runs int64
	hook := func() { atomic.AddInt64(&runs, 1) }

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					runH3ReapplyLive()
					runtime.Gosched()
				}
			}
		}()
	}
	// Install the hook and confirm the readers observe it before flipping it.
	installH3ReapplyLive(hook)
	waitFor(t, "runH3ReapplyLive to invoke the installed hook", func() bool {
		return atomic.LoadInt64(&runs) > 0
	})
	for i := 0; i < 500; i++ {
		installH3ReapplyLive(nil)
		installH3ReapplyLive(hook)
	}
	close(stop)
	wg.Wait()
	if atomic.LoadInt64(&runs) == 0 {
		t.Fatal("runH3ReapplyLive never invoked the installed hook")
	}
}
