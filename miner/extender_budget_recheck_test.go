package miner

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeExtenderSessionHolder records what the recheck tells the device.
type fakeExtenderSessionHolder struct {
	held chan bool
}

func (self *fakeExtenderSessionHolder) HoldProvideExtenderSession(held bool) {
	select {
	case self.held <- held:
	default:
	}
}

func shortBudgetRecheckInterval(t *testing.T) {
	t.Helper()
	restore := extenderBudgetRecheckInterval
	extenderBudgetRecheckInterval = 2 * time.Millisecond
	t.Cleanup(func() { extenderBudgetRecheckInterval = restore })
}

// A withheld launch asks the budget again and releases the hold the moment a
// post is allowed. Without that the role stays off for the life of the process,
// because the slot's settings are its own copy.
func TestExtenderBudgetRecheckReleasesTheHold(t *testing.T) {
	shortBudgetRecheckInterval(t)

	holder := &fakeExtenderSessionHolder{held: make(chan bool, 4)}

	var allowedLock sync.Mutex
	allowed := false
	stop := startExtenderBudgetRecheck(context.Background(), holder, func() bool {
		allowedLock.Lock()
		defer allowedLock.Unlock()
		return allowed
	})
	defer stop()

	// While the budget withholds, the role stays held.
	select {
	case held := <-holder.held:
		t.Fatalf("released a held role before the budget allowed it: %t", held)
	case <-time.After(30 * time.Millisecond):
	}

	allowedLock.Lock()
	allowed = true
	allowedLock.Unlock()

	select {
	case held := <-holder.held:
		if held {
			t.Fatal("the recheck must release the hold, not re-apply it")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the recheck never released the hold")
	}
}

// A stopped recheck asks nothing more, so a slot that has ended cannot release
// a role on a device that is being torn down.
func TestExtenderBudgetRecheckStops(t *testing.T) {
	shortBudgetRecheckInterval(t)

	holder := &fakeExtenderSessionHolder{held: make(chan bool, 4)}
	stop := startExtenderBudgetRecheck(context.Background(), holder, func() bool { return true })
	stop()

	select {
	case held := <-holder.held:
		t.Fatalf("a stopped recheck released the hold: %t", held)
	case <-time.After(30 * time.Millisecond):
	}
}

// The recheck also ends when the slot's context does.
func TestExtenderBudgetRecheckEndsWithTheSlot(t *testing.T) {
	shortBudgetRecheckInterval(t)

	ctx, cancel := context.WithCancel(context.Background())
	holder := &fakeExtenderSessionHolder{held: make(chan bool, 4)}
	stop := startExtenderBudgetRecheck(ctx, holder, func() bool { return false })
	defer stop()

	cancel()

	select {
	case held := <-holder.held:
		t.Fatalf("a cancelled recheck released the hold: %t", held)
	case <-time.After(30 * time.Millisecond):
	}
}
