package miner

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func slotIdentityReloader(startup []*connect.ProxySettings, desired *[]*connect.ProxySettings) *minerProxyReloader {
	identities := newProviderSlotIdentities(startup)
	r := newMinerProxyReloader(func() ([]*connect.ProxySettings, int, error) { return *desired, 0, nil }, nil)
	r.retireTimeout = 50 * time.Millisecond
	r.slotIdentity = func(s *connect.ProxySettings) string { return providerRegistrationSlot(identities, s) }
	return r
}

// Two accounts configured at one address hold different platform slots. A
// stuck worker of one must not block the other's credential rotation.
func TestReloadSharedAddressRotationIgnoresOtherAccountsRetirement(t *testing.T) {
	alice := reloadTestSettings("198.51.100.70:443", "alice", "pw")
	bob := reloadTestSettings("198.51.100.70:443", "bob", "pw")
	aliceNew := reloadTestSettings("198.51.100.70:443", "alice", "pw2")
	desired := []*connect.ProxySettings{alice, bob}
	r := slotIdentityReloader(desired, &desired)
	spawned := 0
	r.spawnGeneration = func(*connect.ProxySettings, uint64) (context.CancelFunc, <-chan struct{}, error) {
		spawned++
		return func() {}, nil, nil
	}
	aliceDone := make(chan struct{})
	bobDone := make(chan struct{}) // bob's worker never returns
	r.seedWorker(alice, func() {}, aliceDone)
	r.seedWorker(bob, func() {}, bobDone)

	desired = []*connect.ProxySettings{alice}
	if _, err := r.reload(); err != nil {
		t.Fatal(err)
	}
	close(aliceDone)
	desired = []*connect.ProxySettings{aliceNew}
	if _, err := r.reload(); err != nil {
		t.Fatalf("alice's rotation waited for bob's stuck worker: %v", err)
	}
	if spawned != 1 {
		t.Fatalf("spawned %d replacements, want 1", spawned)
	}
}

// An account added at runtime to an address that was single-use at startup
// collapses onto the running account's platform slot, so it waits for it.
func TestReloadRuntimeAccountWaitsForCollapsedPlatformSlot(t *testing.T) {
	alice := reloadTestSettings("198.51.100.71:443", "alice", "pw")
	bob := reloadTestSettings("198.51.100.71:443", "bob", "pw")
	desired := []*connect.ProxySettings{alice}
	r := slotIdentityReloader(desired, &desired)
	spawned := 0
	r.spawnGeneration = func(*connect.ProxySettings, uint64) (context.CancelFunc, <-chan struct{}, error) {
		spawned++
		return func() {}, nil, nil
	}
	aliceDone := make(chan struct{})
	r.seedWorker(alice, func() {}, aliceDone)
	if r.platformSlot(alice) != r.platformSlot(bob) {
		t.Fatal("test premise: both accounts must share one platform slot")
	}

	desired = []*connect.ProxySettings{bob}
	if _, err := r.reload(); err == nil || spawned != 0 {
		t.Fatalf("bob started over alice's unfinished worker: err=%v spawned=%d", err, spawned)
	}
	close(aliceDone)
	if _, err := r.reload(); err != nil || spawned != 1 {
		t.Fatalf("bob did not start after alice returned: err=%v spawned=%d", err, spawned)
	}
}

// Only the password changes: the identity is unchanged, so the old worker is
// retired and awaited before the replacement with the new credential starts.
func TestReloadPasswordOnlyRotationWaitsForOldWorker(t *testing.T) {
	old := reloadTestSettings("198.51.100.72:443", "alice", "old")
	next := reloadTestSettings("198.51.100.72:443", "alice", "new")
	desired := []*connect.ProxySettings{old}
	r := slotIdentityReloader(desired, &desired)
	var started []string
	r.spawnGeneration = func(s *connect.ProxySettings, _ uint64) (context.CancelFunc, <-chan struct{}, error) {
		started = append(started, s.Auth.Password)
		return func() {}, nil, nil
	}
	oldDone := make(chan struct{})
	r.seedWorker(old, func() {}, oldDone)

	desired = []*connect.ProxySettings{next}
	if _, err := r.reload(); err == nil || len(started) != 0 {
		t.Fatalf("new password started over the old worker: err=%v started=%v", err, started)
	}
	close(oldDone)
	if _, err := r.reload(); err != nil || len(started) != 1 || started[0] != "new" {
		t.Fatalf("rotation did not start with the new password: err=%v started=%v", err, started)
	}
}
