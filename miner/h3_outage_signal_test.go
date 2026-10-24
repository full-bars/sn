package miner

import (
	"context"
	"testing"
	"time"
)

type signalClock struct{ t time.Time }

func (c *signalClock) now() time.Time { return c.t }

func newSignalForTest() (*h3OutageSignal, *signalClock) {
	clock := &signalClock{t: time.Unix(2_000_000, 0)}
	return newH3OutageSignal(clock.now), clock
}

func (c *signalClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func failures(s *h3OutageSignal, c *signalClock, n int, over time.Duration) {
	for i := 0; i < n; i++ {
		s.failure()
		c.advance(over / time.Duration(n))
	}
}

func TestH3OutageSignal_AnOutageIsEnoughFailuresAndNoSuccess(t *testing.T) {
	s, c := newSignalForTest()
	c.advance(h3OutageShedWindow)
	failures(s, c, h3OutageShedMinFailures-1, 30*time.Second)
	if s.outage(c.now()) {
		t.Fatal("too few failures are a dead proxy or two, not an outage")
	}
	failures(s, c, 1, time.Second)
	if !s.outage(c.now()) {
		t.Fatal("enough failures and no success is an outage")
	}
}

func TestH3OutageSignal_OneSuccessInTheWindowIsNotAnOutage(t *testing.T) {
	s, c := newSignalForTest()
	c.advance(h3OutageShedWindow)
	failures(s, c, 200, 60*time.Second)
	s.success()
	if s.outage(c.now()) {
		t.Fatal("the platform answered a sign-in in this window: not an outage")
	}
}

func TestH3OutageSignal_FailuresAgeOutOfTheWindow(t *testing.T) {
	s, c := newSignalForTest()
	c.advance(h3OutageShedWindow)
	failures(s, c, 50, 20*time.Second)
	c.advance(h3OutageShedWindow)
	if s.outage(c.now()) {
		t.Fatal("failures from before the window must not count")
	}
}

// A start-up stampede of slow first sign-ins fails a lot with no success yet.
func TestH3OutageSignal_NoOutageBeforeItHasWatchedLongEnough(t *testing.T) {
	s, c := newSignalForTest()
	failures(s, c, 100, 30*time.Second)
	if s.outage(c.now()) {
		t.Fatal("a signal that has only just started must not call an outage")
	}
	c.advance(h3OutageShedWindow)
	failures(s, c, 100, 30*time.Second)
	if !s.outage(c.now()) {
		t.Fatal("after watching a full window the same failures are an outage")
	}
}

func TestH3OutageSignal_RecoveryNeedsSeveralSuccessesAndAMajority(t *testing.T) {
	s, c := newSignalForTest()
	c.advance(h3OutageShedWindow)
	// the case the reaper's detector clears on: one success among a flood of failures
	failures(s, c, 400, 100*time.Second)
	s.success()
	if s.recovered(c.now()) {
		t.Fatal("one success among 400 failures is not a recovery")
	}
	// several successes, but still a small share
	for i := 0; i < h3OutageRestoreMinSuccesses; i++ {
		s.success()
	}
	if s.recovered(c.now()) {
		t.Fatalf("%d successes among 400 failures is a small share, not a recovery", h3OutageRestoreMinSuccesses+1)
	}
}

func TestH3OutageSignal_RecoveryWhenMostAttemptsSucceed(t *testing.T) {
	s, c := newSignalForTest()
	c.advance(h3OutageShedWindow)
	for i := 0; i < 30; i++ {
		s.success()
		c.advance(time.Second)
	}
	for i := 0; i < 10; i++ {
		s.failure()
		c.advance(time.Second)
	}
	if !s.recovered(c.now()) {
		t.Fatal("30 successes against 10 failures is a recovery")
	}
}

func TestH3OutageSignal_ManySecondsOfTrafficStayWithinTheRing(t *testing.T) {
	s, c := newSignalForTest()
	for i := 0; i < 10*h3OutageSignalSeconds; i++ {
		s.failure()
		c.advance(time.Second)
	}
	ok, failed := func() (int, int) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.countsLocked(c.now(), h3OutageRestoreWindow)
	}()
	if ok != 0 || failed < h3OutageSignalSeconds-2 || h3OutageSignalSeconds < failed {
		t.Fatalf("the ring kept ok=%d failed=%d, want about %d failures", ok, failed, h3OutageSignalSeconds)
	}
}

// Restoring H3 relaunches every identity as the platform returns, so it is paced
// by the caller, not by the fixed fast interval.
func TestRelaunchPassUsesTheIntervalItIsGiven(t *testing.T) {
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
	h3LaunchMutex.Lock()
	h3LaunchEligible = map[string]bool{}
	h3LaunchMutex.Unlock()
	for _, c := range testH3Proxies(3) {
		recordH3Launch(c.Key(), false)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeH3Relauncher{ctx: ctx, busy: map[string]bool{}}
	const slow = 600 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		relaunchH3ProxiesPaced(ctx, fake, h3Setting{Mode: h3ModeAll}, slow)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		fake.mu.Lock()
		n := len(fake.detached)
		fake.mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("only %d identities were relaunched", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for i := 1; i < len(fake.detachedAt); i++ {
		if gap := fake.detachedAt[i].Sub(fake.detachedAt[i-1]); gap < slow-50*time.Millisecond {
			t.Fatalf("relaunches %d and %d were %s apart, want about %s", i-1, i, gap, slow)
		}
	}
	if slow <= h3RelaunchInterval || h3RelaunchInterval*5 > h3OutageRestoreRelaunchInterval {
		t.Fatal("the restore pace must be much slower than the fast relaunch pace")
	}
}
