package miner

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Tests for the running provide's native direct slot: `provider direct off`
// used to persist the toggle and print DISABLED while the live slot kept
// serving. The handle below is the half that acts on the running run.

// directSlotFake is one fake provideWithProxy launch: it blocks until its slot
// context is cancelled, the way the real direct provider does, and records
// starts and exits.
type directSlotFake struct {
	starts atomic.Int32
	exits  atomic.Int32
}

func newDirectSlotFake() *directSlotFake {
	return &directSlotFake{}
}

func (f *directSlotFake) launch(ctx context.Context, index uint64) error {
	f.starts.Add(1)
	<-ctx.Done()
	f.exits.Add(1)
	return nil
}

// waitStarted blocks until at least n launches have begun.
func (f *directSlotFake) waitStarted(t *testing.T, n int32) {
	t.Helper()
	waitFor(t, 2*time.Second, "the direct launch to start", func() bool {
		return f.starts.Load() >= n
	})
}

// directSlotServe starts the handle's controller and returns a channel carrying
// its (eventual) return, so a test can assert the run did not end on a toggle.
func directSlotServe(slot *directSlot) chan error {
	done := make(chan error, 1)
	go func() { done <- slot.serve() }()
	return done
}

func TestDirectSlotOffStopsTheRunningSlotOnce(t *testing.T) {
	fake := newDirectSlotFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slot := newDirectSlot(ctx, fake.launch, 0, true, nil)
	serveDone := directSlotServe(slot)

	fake.waitStarted(t, 1)
	waitFor(t, time.Second, "the direct slot to be running", slot.running)

	if !slot.setEnabled(false) {
		t.Fatal("direct off must report a change")
	}
	waitFor(t, time.Second, "the direct launch to stop", func() bool { return fake.exits.Load() == 1 })
	waitFor(t, time.Second, "the direct slot to be idle", func() bool { return !slot.running() })

	if slot.setEnabled(false) {
		t.Fatal("a second direct off must not report a change")
	}
	time.Sleep(50 * time.Millisecond)
	if got := fake.starts.Load(); got != 1 {
		t.Fatalf("direct off must not start a session: starts = %d", got)
	}
	if got := fake.exits.Load(); got != 1 {
		t.Fatalf("direct off must cancel the running slot exactly once: exits = %d", got)
	}
	select {
	case err := <-serveDone:
		t.Fatalf("the run ended on a direct toggle: %v", err)
	default:
	}
}

func TestDirectSlotOnAfterOffStartsExactlyOne(t *testing.T) {
	fake := newDirectSlotFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slot := newDirectSlot(ctx, fake.launch, 0, true, nil)
	directSlotServe(slot)

	fake.waitStarted(t, 1)
	slot.setEnabled(false)
	waitFor(t, time.Second, "the direct launch to stop", func() bool { return fake.exits.Load() == 1 })

	if !slot.setEnabled(true) {
		t.Fatal("direct on after off must report a change")
	}
	fake.waitStarted(t, 2)
	waitFor(t, time.Second, "the direct slot to be running again", slot.running)

	if slot.setEnabled(true) {
		t.Fatal("a second direct on must not report a change")
	}
	time.Sleep(50 * time.Millisecond)
	if got := fake.starts.Load(); got != 2 {
		t.Fatalf("direct on twice must start one session: starts = %d, want 2", got)
	}
	if got := fake.exits.Load(); got != 1 {
		t.Fatalf("the first session must not be stopped twice: exits = %d", got)
	}
}

func TestDirectSlotOnTwiceAtStartDoesNotDuplicate(t *testing.T) {
	fake := newDirectSlotFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slot := newDirectSlot(ctx, fake.launch, 0, true, nil)
	directSlotServe(slot)

	fake.waitStarted(t, 1)
	if slot.setEnabled(true) {
		t.Fatal("direct on with the slot already on must be a no-op")
	}
	time.Sleep(50 * time.Millisecond)
	if got := fake.starts.Load(); got != 1 {
		t.Fatalf("direct on twice must start one session: starts = %d", got)
	}
}

// A launch that fails for a reason other than the operator toggle keeps the
// pre-handle behaviour: the run reports the error so the supervisor restarts it.
func TestDirectSlotLaunchFailureEndsTheRun(t *testing.T) {
	boom := errors.New("direct provider refused")
	slot := newDirectSlot(context.Background(), func(context.Context, uint64) error {
		return boom
	}, 0, true, nil)
	if err := slot.serve(); !errors.Is(err, boom) {
		t.Fatalf("serve = %v, want the launch error", err)
	}
}

// A stop that makes the launch return an error is still an operator stop: the
// slot serves nothing, but the provider must stay up so `direct on` can be
// applied without a restart.
func TestDirectSlotOperatorStopIgnoresTheLaunchError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := make(chan struct{})
	slot := newDirectSlot(ctx, func(slotCtx context.Context, index uint64) error {
		<-slotCtx.Done()
		close(cancelled)
		return errors.New("device teardown joined the cancellation")
	}, 0, true, nil)
	serveDone := directSlotServe(slot)
	waitFor(t, time.Second, "the direct slot to be running", slot.running)

	slot.setEnabled(false)
	<-cancelled
	select {
	case err := <-serveDone:
		t.Fatalf("an operator stop ended the run: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	waitFor(t, time.Second, "the direct slot to be idle", func() bool { return !slot.running() })
}

func TestDirectSlotIndexFindsTheNilEntry(t *testing.T) {
	proxy := reloadTestSettings("198.51.100.30:443", "", "")
	if index, ok := directSlotIndex([]*connect.ProxySettings{proxy}); ok || index != 0 {
		t.Fatalf("a proxies-only list must have no direct slot, got (%d, %v)", index, ok)
	}
	if index, ok := directSlotIndex([]*connect.ProxySettings{nil}); !ok || index != 0 {
		t.Fatalf("direct-only list index = (%d, %v), want (0, true)", index, ok)
	}
	if index, ok := directSlotIndex([]*connect.ProxySettings{proxy, nil}); !ok || index != 1 {
		t.Fatalf("direct slot after one proxy index = (%d, %v), want (1, true)", index, ok)
	}
}

// The reloader applies the operator's direct toggle and leaves the proxy pool
// alone: the direct slot is not a proxy identity and never enters the
// reloader's registry.
func TestDirectToggleThroughReloadLeavesProxiesAlone(t *testing.T) {
	fake := newDirectSlotFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slot := newDirectSlot(ctx, fake.launch, 0, true, nil)
	directSlotServe(slot)
	fake.waitStarted(t, 1)

	h := &reloadTestHarness{desired: []*connect.ProxySettings{
		reloadTestSettings("198.51.100.31:443", "", ""),
	}}
	r := h.reloader()
	directOn := true
	r.onReload = func() { slot.setEnabled(directOn) }

	if _, err := r.reload(); err != nil {
		t.Fatalf("first reload: %v", err)
	}
	if got := len(h.spawnedKeys()); got != 1 {
		t.Fatalf("first reload spawned %d proxies, want 1", got)
	}

	directOn = false
	if _, err := r.reload(); err != nil {
		t.Fatalf("reload after direct off: %v", err)
	}
	waitFor(t, time.Second, "the direct launch to stop", func() bool { return fake.exits.Load() == 1 })

	if got := len(h.spawnedKeys()); got != 1 {
		t.Fatalf("direct off must not start a proxy: spawned = %v", h.spawnedKeys())
	}
	if got := h.cancelledKeys(); len(got) != 0 {
		t.Fatalf("direct off must not cancel a proxy: cancelled = %v", got)
	}
	keys := r.runningKeys()
	if len(keys) != 1 {
		t.Fatalf("proxy registry = %v, want exactly the one proxy", keys)
	}
	for _, key := range keys {
		if key == directProxyKey {
			t.Fatalf("the direct slot entered the proxy registry: %v", keys)
		}
	}

	// Turning it back on through the same path starts one direct session and
	// still leaves the proxy registry untouched.
	directOn = true
	if _, err := r.reload(); err != nil {
		t.Fatalf("reload after direct on: %v", err)
	}
	fake.waitStarted(t, 2)
	waitFor(t, time.Second, "the direct slot to be running again", slot.running)
	if got := len(h.spawnedKeys()); got != 1 {
		t.Fatalf("direct on must not start a proxy: spawned = %v", h.spawnedKeys())
	}
	if got := h.cancelledKeys(); len(got) != 0 {
		t.Fatalf("direct on must not cancel a proxy: cancelled = %v", got)
	}
}

// An unreadable proxy source aborts the proxy diff, but not the operator's
// direct off: the two are independent, and the legal/abuse case cannot wait for
// a proxy file to become readable.
func TestDirectToggleAppliesWhenTheProxyReadFails(t *testing.T) {
	h := &reloadTestHarness{derr: errors.New("proxy.state is unreadable")}
	r := h.reloader()
	calls := 0
	r.onReload = func() { calls++ }

	if _, err := r.reload(); err == nil {
		t.Fatal("reload with an unreadable desired set must report the error")
	}
	if calls != 1 {
		t.Fatalf("direct hook calls = %d, want 1 before the desired-set read", calls)
	}
}

// The reported symptom, end to end: `provider direct off` writes <state>/direct
// and the reload path reads that same file. applyDirectToggle is the join, so a
// persisted off must stop the live slot and a persisted on must start it again.
func TestDirectToggleFollowsThePersistedFile(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	t.Setenv("DISABLE_DIRECT_IP", "")

	fake := newDirectSlotFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slot := newDirectSlot(ctx, fake.launch, 0, isDirectEnabled(), nil)
	directSlotServe(slot)
	fake.waitStarted(t, 1)

	if err := writeDirectEnabled(false); err != nil {
		t.Fatalf("write the direct toggle: %v", err)
	}
	if !applyDirectToggle(slot) {
		t.Fatal("a persisted off must stop the running direct slot")
	}
	waitFor(t, time.Second, "the direct launch to stop", func() bool { return fake.exits.Load() == 1 })
	if slot.running() {
		t.Fatal("the direct slot kept running after a persisted off")
	}

	if err := writeDirectEnabled(true); err != nil {
		t.Fatalf("write the direct toggle: %v", err)
	}
	if !applyDirectToggle(slot) {
		t.Fatal("a persisted on must start the direct slot")
	}
	fake.waitStarted(t, 2)
	waitFor(t, time.Second, "the direct slot to be running again", slot.running)

	// A second reconcile of the same persisted state is a no-op.
	if applyDirectToggle(slot) {
		t.Fatal("reconciling an unchanged toggle must not change the slot")
	}
	time.Sleep(50 * time.Millisecond)
	if got := fake.starts.Load(); got != 2 {
		t.Fatalf("starts = %d, want 2 (no duplicate session)", got)
	}
}
