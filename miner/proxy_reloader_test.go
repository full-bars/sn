package miner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// Tests for the live proxy reload: the miner's half of the reload contract that
// the port from urnetwork-3.23-fix left out, which made `urnet-tools proxy add`
// report a hot reload that never happened.

func reloadTestSettings(addr, user, password string) *connect.ProxySettings {
	s := &connect.ProxySettings{Network: "tcp", Address: addr}
	if user != "" || password != "" {
		s.Auth = &proxy.Auth{User: user, Password: password}
	}
	return s
}

// reloadTestHarness records what the reloader did to the pool.
type reloadTestHarness struct {
	mu sync.Mutex

	desired      []*connect.ProxySettings
	derr         error
	desiredCalls int

	spawned   []string
	cancelled []string
	spawnErr  error
}

// spawnedKeys and cancelledKeys copy the recorded slices under the harness lock:
// the watcher records on its own goroutine while the test reads, and an
// unsynchronised read here is a real data race (caught by -race).
func (h *reloadTestHarness) spawnedKeys() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.spawned...)
}

func (h *reloadTestHarness) cancelledKeys() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.cancelled...)
}

func (h *reloadTestHarness) desiredCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.desiredCalls
}

func (h *reloadTestHarness) setDesired(settings ...*connect.ProxySettings) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.desired = settings
}

func (h *reloadTestHarness) reloader() *minerProxyReloader {
	return newMinerProxyReloader(
		func() ([]*connect.ProxySettings, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.desiredCalls++
			if h.derr != nil {
				return nil, h.derr
			}
			return h.desired, nil
		},
		func(settings *connect.ProxySettings) (context.CancelFunc, error) {
			if h.spawnErr != nil {
				return nil, h.spawnErr
			}
			key := settings.Key()
			h.mu.Lock()
			h.spawned = append(h.spawned, key)
			h.mu.Unlock()
			return func() {
				h.mu.Lock()
				h.cancelled = append(h.cancelled, key)
				h.mu.Unlock()
			}, nil
		},
	)
}

func containsIdentity(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// A reload adds new identities, removes vanished ones, and rotates an identity
// whose credentials changed under the same address.
func TestMinerProxyReloadAddsRemovesAndRotates(t *testing.T) {
	h := &reloadTestHarness{desired: []*connect.ProxySettings{
		reloadTestSettings("198.51.100.10:443", "alice", "old-secret"),
		reloadTestSettings("198.51.100.11:443", "", ""),
	}}
	r := h.reloader()

	summary, err := r.reload()
	if err != nil {
		t.Fatalf("first reload: %v", err)
	}
	if summary.Added != 2 || summary.Removed != 0 || summary.Rotated != 0 {
		t.Fatalf("first reload summary = %s, want added 2", summary)
	}

	// The same account comes back with a new password (Key excludes the
	// password on purpose, so this is a rotation), one address is dropped, and
	// a new one appears.
	rotated := reloadTestSettings("198.51.100.10:443", "alice", "new-secret")
	added := reloadTestSettings("198.51.100.12:443", "", "")
	h.setDesired(rotated, added)

	summary, err = r.reload()
	if err != nil {
		t.Fatalf("second reload: %v", err)
	}
	if summary.Added != 1 || summary.Removed != 1 || summary.Rotated != 1 {
		t.Fatalf("second reload summary = %s, want added 1, removed 1, rotated 1", summary)
	}

	// The rotated identity was cancelled before its replacement started, and
	// the dropped one was cancelled too.
	if !containsIdentity(h.cancelledKeys(), reloadTestSettings("198.51.100.10:443", "alice", "old-secret").Key()) {
		t.Fatalf("rotated identity was not cancelled: %v", h.cancelledKeys())
	}
	if !containsIdentity(h.cancelledKeys(), reloadTestSettings("198.51.100.11:443", "", "").Key()) {
		t.Fatalf("dropped identity was not cancelled: %v", h.cancelledKeys())
	}

	keys := r.runningKeys()
	if len(keys) != 2 {
		t.Fatalf("running set = %v, want 2 identities", keys)
	}
}

// A source read failure must leave the running pool untouched: treating it as
// an empty desired set would cancel the whole fleet over one bad read.
func TestMinerProxyReloadKeepsThePoolWhenTheSourceFails(t *testing.T) {
	h := &reloadTestHarness{desired: []*connect.ProxySettings{
		reloadTestSettings("198.51.100.10:443", "", ""),
	}}
	r := h.reloader()
	if _, err := r.reload(); err != nil {
		t.Fatalf("first reload: %v", err)
	}

	h.derr = errors.New("proxy source unreadable")
	summary, err := r.reload()
	if err == nil {
		t.Fatal("reload succeeded on a failed source read")
	}
	if got := h.cancelledKeys(); len(got) != 0 {
		t.Fatalf("pool was mutated on a failed read: cancelled %v", got)
	}
	if summary != (minerReloadSummary{}) {
		t.Fatalf("summary = %s, want zero", summary)
	}
	if keys := r.runningKeys(); len(keys) != 1 {
		t.Fatalf("running set = %v, want the one identity kept", keys)
	}
}

// The watcher applies a reload when the trigger sequence changes, and does not
// re-apply one while the sequence is unchanged.
func TestMinerProxyReloadWatchAppliesATriggerChange(t *testing.T) {
	h := &reloadTestHarness{desired: []*connect.ProxySettings{
		reloadTestSettings("198.51.100.10:443", "", ""),
	}}
	r := h.reloader()

	path := filepath.Join(t.TempDir(), "proxy.reload")
	if err := os.WriteFile(path, []byte("1\n"), 0o600); err != nil {
		t.Fatalf("seed trigger: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.watch(ctx, path, 10*time.Millisecond)

	// Let the watcher take its baseline before the sequence moves.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, []byte("2\n"), 0o600); err != nil {
		t.Fatalf("bump trigger: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for len(h.spawnedKeys()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.spawnedKeys(); len(got) != 1 {
		t.Fatalf("spawned %d slots, want 1 after the trigger change", len(got))
	}

	// No new trigger, no further spawns.
	time.Sleep(80 * time.Millisecond)
	// No new trigger means no new reconcile: count the desired-set reads, not
	// the spawns, because a reload of an unchanged set spawns nothing either way.
	if got := h.desiredCount(); got != 1 {
		t.Fatalf("reconciled %d times, want 1 with no new trigger", got)
	}
}

// The launch loop seeds the identities it started itself. The first reload must
// adopt them: spawning a second provider for an identity already being served
// re-identifies the live slot, which is what the platform session must never
// see. The seeded slot's own cancel is the handle the reloader must use when
// the identity later leaves the desired set.
func TestMinerProxyReloadSeededIdentityIsNotRespawned(t *testing.T) {
	seeded := reloadTestSettings("198.51.100.10:443", "alice", "secret")
	h := &reloadTestHarness{desired: []*connect.ProxySettings{seeded}}
	r := h.reloader()

	seededCancelled := 0
	r.seed(seeded, func() { seededCancelled++ })

	summary, err := r.reload()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := h.spawnedKeys(); len(got) != 0 {
		t.Fatalf("first reload spawned %v, want nothing for the seeded identity", got)
	}
	if summary.Unchanged != 1 || summary.Added != 0 || summary.Removed != 0 || summary.Rotated != 0 {
		t.Fatalf("summary = %s, want unchanged 1", summary)
	}
	if len(h.cancelledKeys()) != 0 || seededCancelled != 0 {
		t.Fatalf("the first reload cancelled the seeded slot")
	}
	if keys := r.runningKeys(); len(keys) != 1 || keys[0] != seeded.Key() {
		t.Fatalf("running set = %v, want the seeded identity", keys)
	}

	// Once the identity leaves the desired set, the reloader stops it through
	// the cancel the launch loop handed over.
	h.desired = nil
	summary, err = r.reload()
	if err != nil {
		t.Fatalf("removal reload: %v", err)
	}
	if summary.Removed != 1 {
		t.Fatalf("summary = %s, want removed 1", summary)
	}
	if seededCancelled != 1 {
		t.Fatalf("seeded cancel called %d times, want 1", seededCancelled)
	}
}

// onChange publishes the set the run is actually serving: the OOM cap and the
// control surface's `proxy trim --preview` read it, so it must carry the
// identity a reload just added, sorted, and the empty set after stop.
func TestMinerProxyReloadPublishesRunningKeys(t *testing.T) {
	first := reloadTestSettings("198.51.100.10:443", "alice", "secret")
	h := &reloadTestHarness{desired: []*connect.ProxySettings{first}}
	r := h.reloader()
	var published [][]string
	r.onChange = func(keys []string) {
		published = append(published, append([]string(nil), keys...))
	}

	second := reloadTestSettings("198.51.100.11:443", "", "")
	h.desired = []*connect.ProxySettings{first, second}
	if _, err := r.reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if len(published) != 1 {
		t.Fatalf("onChange called %d times, want 1 after one successful reload", len(published))
	}
	want := []string{first.Key(), second.Key()}
	sort.Strings(want)
	if !slices.Equal(published[0], want) {
		t.Fatalf("published set = %v, want %v (sorted)", published[0], want)
	}

	r.stop()
	if len(published) != 2 {
		t.Fatalf("onChange called %d times, want 2 after stop", len(published))
	}
	if len(published[1]) != 0 {
		t.Fatalf("published set after stop = %v, want empty", published[1])
	}
}

// A spawn failure must not abort the reconcile: the rest of the pool is still
// applied, a dead proxy is still removed, and the summary reports only what
// really happened.
func TestProxyReloadSpawnFailureKeepsReconciling(t *testing.T) {
	h := &reloadTestHarness{spawnErr: errors.New("auth mint unavailable")}
	r := h.reloader()

	// One identity is already served and must be removed; one new identity
	// fails to start.
	seeded := reloadTestSettings("198.51.100.20:443", "", "")
	seededCancelled := 0
	r.seed(seeded, func() { seededCancelled++ })

	h.setDesired(reloadTestSettings("198.51.100.21:443", "", ""))

	summary, err := r.reload()
	if err == nil {
		t.Fatal("a failed spawn was reported as success")
	}
	if summary.Added != 0 {
		t.Fatalf("summary claims %d additions that never started: %s", summary.Added, summary)
	}
	if summary.Removed != 1 || seededCancelled != 1 {
		t.Fatalf("the removal did not run after the spawn failure: summary=%s cancelled=%d", summary, seededCancelled)
	}
	if len(h.spawnedKeys()) != 0 {
		t.Fatalf("spawn recorded a slot that failed: %v", h.spawnedKeys())
	}
}

// stop() is terminal: a reload after it (the hotswap-yield case) must not
// re-spawn the pool.
func TestProxyReloadAfterStopDoesNotResurrect(t *testing.T) {
	h := &reloadTestHarness{desired: []*connect.ProxySettings{
		reloadTestSettings("198.51.100.30:443", "", ""),
	}}
	r := h.reloader()
	if _, err := r.reload(); err != nil {
		t.Fatalf("first reload: %v", err)
	}
	before := len(h.spawnedKeys())

	r.stop()
	h.setDesired(reloadTestSettings("198.51.100.31:443", "", ""))

	if _, err := r.reload(); !errors.Is(err, errProxyReloaderStopped) {
		t.Fatalf("reload after stop = %v, want errProxyReloaderStopped", err)
	}
	if got := len(h.spawnedKeys()); got != before {
		t.Fatalf("reload after stop spawned %d slots, want none", got-before)
	}
}
