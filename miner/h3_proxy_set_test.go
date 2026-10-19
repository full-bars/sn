package miner

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// h3_proxy_set_test.go ports the v33 provider's h3_proxy_set_test.go. The
// platform-settings test is the one piece that waits for the transport seam:
// the pinned full-bars/connect builds an identity's platform transports inside
// the SDK, so "a proxy gets its relay factory" lands with the h3_direct /
// h3_packet_conn port; the ranking and relaunch behaviour is covered here.

func testH3Proxies(n int) []*connect.ProxySettings {
	out := make([]*connect.ProxySettings, n)
	for i := range out {
		out[i] = &connect.ProxySettings{Network: "tcp", Address: fmt.Sprintf("192.0.2.%d:1080", i+1), Auth: &proxy.Auth{User: "u", Password: "p"}}
	}
	return out
}

func gradedState(candidates []*connect.ProxySettings, scores map[int]float64) *ProxyState {
	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	for i, c := range candidates {
		if score, ok := scores[i]; ok {
			state.Proxies[c.Key()] = ProxyEntry{Graded: true, Score: score}
		}
	}
	return state
}

func TestRankH3ProxiesPrefersGradedThenScore(t *testing.T) {
	candidates := testH3Proxies(6)
	state := gradedState(candidates, map[int]float64{4: 0.9, 2: 0.8, 5: 0.1})
	got := rankH3Proxies(2, nil, candidates, state)
	if len(got) != 2 || !got[candidates[4].Key()] || !got[candidates[2].Key()] {
		t.Fatalf("top 2 = %v, want the two best graded proxies", got)
	}
	// ungraded ones fill when there are not enough graded
	got = rankH3Proxies(4, nil, candidates, state)
	if len(got) != 4 || !got[candidates[5].Key()] {
		t.Fatalf("top 4 = %v, want three graded plus one ungraded", got)
	}
	if got := rankH3Proxies(0, nil, candidates, state); len(got) != 0 {
		t.Fatalf("a count of zero ranks nothing, got %v", got)
	}
	if got := rankH3Proxies(100, nil, candidates, state); len(got) != 6 {
		t.Fatalf("a count above the pool ranks all %d, got %d", 6, len(got))
	}
}

// Members stay while they remain candidates, so drifting grades do not
// reconnect a working identity.
func TestRankH3ProxiesIsSticky(t *testing.T) {
	candidates := testH3Proxies(5)
	state := gradedState(candidates, map[int]float64{0: 0.9, 1: 0.8, 2: 0.1, 3: 0.95})
	previous := map[string]bool{candidates[0].Key(): true, candidates[2].Key(): true}
	got := rankH3Proxies(2, previous, candidates, state)
	if !sameKeySet(got, previous) {
		t.Fatalf("a better proxy displaced a working member: %v", got)
	}
	// a member that left the pool is replaced by the best remaining
	got = rankH3Proxies(2, previous, []*connect.ProxySettings{candidates[0], candidates[1], candidates[3]}, state)
	if !got[candidates[0].Key()] || !got[candidates[3].Key()] || len(got) != 2 {
		t.Fatalf("vacancy not filled by the best candidate: %v", got)
	}
}

func TestH3ProxyEligibleByMode(t *testing.T) {
	t.Cleanup(func() { h3RankingValue.Store(nil) })
	candidates := testH3Proxies(3)
	key := candidates[0].Key()
	for _, setting := range []h3Setting{{Mode: h3ModeOff}, {Mode: h3ModeDirect}, {Mode: h3ModeOn}} {
		if h3ProxyEligible(setting, key) {
			t.Fatalf("%v must never admit a proxy", setting)
		}
	}
	if !h3ProxyEligible(h3Setting{Mode: h3ModeAll}, key) {
		t.Fatal("all admits every proxy")
	}
	top := h3Setting{Mode: h3ModeTop, Cap: 1}
	if h3ProxyEligible(top, key) {
		t.Fatal("a count admits nothing before the ranking exists")
	}
	h3RankingValue.Store(&h3Ranking{cap: 1, keys: map[string]bool{key: true}})
	if !h3ProxyEligible(top, key) || h3ProxyEligible(top, candidates[1].Key()) {
		t.Fatal("a count admits exactly the ranked proxies")
	}
	if h3ProxyEligible(h3Setting{Mode: h3ModeTop, Cap: 2}, key) {
		t.Fatal("a ranking for another count must not be used")
	}
}

func TestRelaunchListFollowsEligibility(t *testing.T) {
	t.Cleanup(func() {
		h3RankingValue.Store(nil)
		h3LaunchMutex.Lock()
		h3LaunchEligible = map[string]bool{}
		h3LaunchMutex.Unlock()
	})
	h3LaunchMutex.Lock()
	h3LaunchEligible = map[string]bool{}
	h3LaunchMutex.Unlock()
	candidates := testH3Proxies(3)
	for _, c := range candidates {
		recordH3Launch(c.Key(), false)
	}
	if got := h3ProxiesToRelaunch(h3Setting{Mode: h3ModeDirect}); len(got) != 0 {
		t.Fatalf("nothing changes under direct, got %v", got)
	}
	if got := h3ProxiesToRelaunch(h3Setting{Mode: h3ModeAll}); len(got) != 3 {
		t.Fatalf("all must relaunch the three H1-only proxies, got %v", got)
	}
	h3RankingValue.Store(&h3Ranking{cap: 1, keys: map[string]bool{candidates[1].Key(): true}})
	got := h3ProxiesToRelaunch(h3Setting{Mode: h3ModeTop, Cap: 1})
	if len(got) != 1 || got[0] != candidates[1].Key() {
		t.Fatalf("a count of one must relaunch only its member, got %v", got)
	}
	recordH3Launch(candidates[1].Key(), true)
	if got := h3ProxiesToRelaunch(h3Setting{Mode: h3ModeTop, Cap: 1}); len(got) != 0 {
		t.Fatalf("already launched as eligible, got %v", got)
	}
}

// Candidates are noted under every setting, so switching to a count later ranks
// over the real set instead of an empty one.
func TestCandidatesNotedBeforeACountSetting(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	t.Cleanup(func() {
		h3RankingValue.Store(nil)
		noteH3Candidates(nil, h3Setting{Mode: h3ModeDirect})
	})
	candidates := testH3Proxies(5)
	if noteH3Candidates(candidates, h3Setting{Mode: h3ModeDirect}) {
		t.Fatal("no ranking exists under direct, so nothing changed")
	}
	if !rerankH3(h3Setting{Mode: h3ModeTop, Cap: 2}) {
		t.Fatal("switching to a count must rank the candidates noted earlier")
	}
	ranking := h3RankingValue.Load()
	if ranking == nil || len(ranking.keys) != 2 {
		t.Fatalf("ranking = %+v, want 2 members", ranking)
	}
	if !rerankH3(h3Setting{Mode: h3ModeDirect}) || h3RankingValue.Load() != nil {
		t.Fatal("leaving the count must drop the ranking")
	}
}

// fakeH3Relauncher is the test double for the running provide run's per-identity
// relaunch facility (see h3IdentityRelauncher).
type fakeH3Relauncher struct {
	ctx context.Context

	mu         sync.Mutex
	busy       map[string]bool
	detached   []string
	detachedAt []time.Time
}

func (f *fakeH3Relauncher) detachForRelaunch(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detached = append(f.detached, key)
	f.detachedAt = append(f.detachedAt, time.Now())
	return true
}

func (f *fakeH3Relauncher) hasClients(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy[key]
}

func (f *fakeH3Relauncher) runContext() context.Context { return f.ctx }

// A live change detaches the identities whose eligibility moved, one per
// h3RelaunchInterval, and leaves an identity that is serving clients alone for
// the later retry.
func TestRelaunchPassDetachesChangedIdentitiesOneAtATime(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	oldState := globalControlState
	t.Cleanup(func() {
		globalControlState = oldState
		h3LaunchMutex.Lock()
		h3LaunchEligible = map[string]bool{}
		h3LaunchMutex.Unlock()
		h3RankingValue.Store(nil)
	})
	state := newControlState()
	if err := state.set("h3", "all"); err != nil {
		t.Fatal(err)
	}
	globalControlState = state
	candidates := testH3Proxies(3)
	h3LaunchMutex.Lock()
	h3LaunchEligible = map[string]bool{}
	h3LaunchMutex.Unlock()
	for _, c := range candidates {
		recordH3Launch(c.Key(), false)
	}
	busyKey := candidates[1].Key()

	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeH3Relauncher{ctx: ctx, busy: map[string]bool{busyKey: true}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		relaunchH3Proxies(ctx, fake, h3Setting{Mode: h3ModeAll})
	}()

	deadline := time.Now().Add(15 * time.Second)
	for {
		fake.mu.Lock()
		n := len(fake.detached)
		fake.mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the relaunch pass detached %d identities, want the 2 whose eligibility moved", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relaunch pass did not end with the run context")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.detached) != 2 {
		t.Fatalf("detached %v, want exactly the two non-busy identities", fake.detached)
	}
	for _, key := range fake.detached {
		if key == busyKey {
			t.Fatalf("an identity with clients was detached: %v", fake.detached)
		}
	}
	if gap := fake.detachedAt[1].Sub(fake.detachedAt[0]); gap < h3RelaunchInterval-50*time.Millisecond {
		t.Fatalf("relaunches were %s apart, want about %s", gap, h3RelaunchInterval)
	}
}

// A pass with nothing to move returns without touching anything.
func TestRelaunchPassIsANoOpWithNothingToMove(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	oldState := globalControlState
	t.Cleanup(func() {
		globalControlState = oldState
		h3LaunchMutex.Lock()
		h3LaunchEligible = map[string]bool{}
		h3LaunchMutex.Unlock()
	})
	state := newControlState()
	if err := state.set("h3", "direct"); err != nil {
		t.Fatal(err)
	}
	globalControlState = state
	h3LaunchMutex.Lock()
	h3LaunchEligible = map[string]bool{}
	h3LaunchMutex.Unlock()
	for _, c := range testH3Proxies(2) {
		recordH3Launch(c.Key(), false)
	}
	fake := &fakeH3Relauncher{ctx: context.Background(), busy: map[string]bool{}}
	relaunchH3Proxies(context.Background(), fake, h3Setting{Mode: h3ModeDirect})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.detached) != 0 {
		t.Fatalf("detached %v, want nothing under an unchanged setting", fake.detached)
	}
}

// A newer setting ends a pass that is already running, and outside a running
// provide run there is no relauncher published at all: the live-change entry
// point must be a quiet no-op rather than a panic (offline tools, unit tests).
func TestRelaunchNowIsANoOpWithoutARunningProvideRun(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	if currentH3IdentityRelauncher() != nil {
		t.Fatal("no test may publish a relauncher globally; the seam must start nil")
	}
	oldState := globalControlState
	t.Cleanup(func() { globalControlState = oldState })
	state := newControlState()
	if err := state.set("h3", "all"); err != nil {
		t.Fatal(err)
	}
	globalControlState = state
	relaunchH3ProxiesNow(h3Setting{Mode: h3ModeAll})
}
