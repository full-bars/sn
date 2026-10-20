package miner

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestProxyFailureHistory_TracksPerAddress(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}

	if got := h.FailureCount("1.2.3.4:1080"); got != 0 {
		t.Fatalf("expected unmarked address to have 0 failures, got %d", got)
	}

	h.RecordFailure("1.2.3.4:1080")
	h.RecordFailure("1.2.3.4:1080")

	if got := h.FailureCount("1.2.3.4:1080"); got != 2 {
		t.Fatalf("expected 2 recorded failures, got %d", got)
	}
	if got := h.FailureCount("5.6.7.8:1080"); got != 0 {
		t.Fatalf("expected a different address to be unaffected, got %d", got)
	}
}

func TestProxyFailureHistory_SurvivesAcrossLaunches(t *testing.T) {
	// Simulates the 15-minute URL-source requeue: a fresh launch resets a
	// local attempt counter to zero, but the lifetime history must not
	// forget how many times this address has already failed.
	h := &proxyFailureHistory{failures: map[string]int{}}

	for i := 0; i < 3; i++ {
		h.RecordFailure("9.9.9.9:1080")
	}
	localAttemptCounter := 0 // reset on relaunch

	if got := h.FailureCount("9.9.9.9:1080"); got != 3 {
		t.Fatalf("expected lifetime history to retain 3 failures across a simulated relaunch, got %d", got)
	}
	if localAttemptCounter != 0 {
		t.Fatalf("sanity check failed: local counter should be reset")
	}
}

func TestProxyFailureHistory_RecordGiveUp_TracksPerAddress(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}

	if got := h.GiveUpCount("1.2.3.4:1080"); got != 0 {
		t.Fatalf("expected unmarked address to have 0 give-ups, got %d", got)
	}

	h.RecordGiveUp("1.2.3.4:1080")
	h.RecordGiveUp("1.2.3.4:1080")

	if got := h.GiveUpCount("1.2.3.4:1080"); got != 2 {
		t.Fatalf("expected 2 recorded give-ups, got %d", got)
	}
	if got := h.GiveUpCount("5.6.7.8:1080"); got != 0 {
		t.Fatalf("expected a different address to be unaffected, got %d", got)
	}
}

func TestProxyFailureHistory_Reset_ClearsGiveUpsToo(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}

	h.RecordFailure("1.2.3.4:1080")
	h.RecordGiveUp("1.2.3.4:1080")
	h.RecordGiveUp("1.2.3.4:1080")

	h.Reset("1.2.3.4:1080")

	if got := h.FailureCount("1.2.3.4:1080"); got != 0 {
		t.Fatalf("expected failure count cleared after Reset, got %d", got)
	}
	if got := h.GiveUpCount("1.2.3.4:1080"); got != 0 {
		t.Fatalf("expected give-up count cleared after Reset, got %d", got)
	}
}

func TestProxyFailureHistory_Prune_RemovesGiveUpsForDroppedAddresses(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}

	h.RecordGiveUp("1.2.3.4:1080")
	h.RecordGiveUp("5.6.7.8:1080")

	h.Prune(map[string]bool{"1.2.3.4:1080": true})

	if got := h.GiveUpCount("1.2.3.4:1080"); got != 1 {
		t.Fatalf("expected kept address to retain its give-up count, got %d", got)
	}
	if got := h.GiveUpCount("5.6.7.8:1080"); got != 0 {
		t.Fatalf("expected dropped address's give-up count to be pruned, got %d", got)
	}
}

func TestProxyFailureHistory_ResetClearsCount(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}

	for i := 0; i < 5; i++ {
		h.RecordFailure("1.2.3.4:1080")
	}
	if got := h.FailureCount("1.2.3.4:1080"); got != 5 {
		t.Fatalf("expected 5 recorded failures before reset, got %d", got)
	}

	h.Reset("1.2.3.4:1080")

	if got := h.FailureCount("1.2.3.4:1080"); got != 0 {
		t.Fatalf("expected failure count to be cleared after Reset, got %d", got)
	}
}

func TestProxyFailureHistory_ExtendBackoffUntil_SetsWhenNoBackoff(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}
	now := time.Now()

	if !h.ExtendBackoffUntil("1.2.3.4:1080", now.Add(6*time.Hour)) {
		t.Fatalf("expected a first backoff to be recorded")
	}
	if h.Eligible("1.2.3.4:1080", now.Add(5*time.Hour)) {
		t.Fatalf("expected address to be ineligible inside the window")
	}
	if !h.Eligible("1.2.3.4:1080", now.Add(6*time.Hour)) {
		t.Fatalf("expected address to be eligible once the window elapses")
	}
}

func TestProxyFailureHistory_ExtendBackoffUntil_LongerWindowReplacesShorter(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}
	now := time.Now()
	h.SetBackoffUntil("1.2.3.4:1080", now.Add(time.Hour))

	if !h.ExtendBackoffUntil("1.2.3.4:1080", now.Add(24*time.Hour)) {
		t.Fatalf("expected a longer window to extend the backoff")
	}
	if h.Eligible("1.2.3.4:1080", now.Add(23*time.Hour)) {
		t.Fatalf("expected the extended window to hold at 23h")
	}
}

// A proxy audit park (long) must survive a later trim-shed pass (1h) that picked
// the same address from a stale snapshot. SetBackoffUntil overwrites; the
// extend-only variant must not shorten.
func TestProxyFailureHistory_ExtendBackoffUntil_NeverShortensExisting(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}
	now := time.Now()
	h.SetBackoffUntil("1.2.3.4:1080", now.Add(24*time.Hour))

	if h.ExtendBackoffUntil("1.2.3.4:1080", now.Add(time.Hour)) {
		t.Fatalf("expected a shorter window to be refused")
	}
	if h.Eligible("1.2.3.4:1080", now.Add(23*time.Hour)) {
		t.Fatalf("expected the original 24h backoff to be intact")
	}
}

func TestProxyFailureHistory_ExtendBackoffUntil_ReplacesElapsedWindow(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}
	now := time.Now()
	h.SetBackoffUntil("1.2.3.4:1080", now.Add(-time.Hour)) // already elapsed

	if !h.ExtendBackoffUntil("1.2.3.4:1080", now.Add(time.Hour)) {
		t.Fatalf("expected a future window to replace an elapsed one")
	}
	if h.Eligible("1.2.3.4:1080", now) {
		t.Fatalf("expected address to be ineligible under the new window")
	}
}

// Proxy audit releases a park by clearing ONLY the backoff it set. A longer one
// placed by someone else (trim shed, URL give-up) must survive, and so must the
// failure counts, which are not audit's to wipe.
func TestReleaseBackoffOnlyClearsTheBackoffItSet(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}
	now := time.Now()
	ours := now.Add(6 * time.Hour)

	h.ExtendBackoffUntil("a:1", ours)
	h.RecordFailure("a:1")
	if !h.ReleaseBackoff("a:1", ours) {
		t.Fatalf("a backoff equal to the one we set must be released")
	}
	if !h.Eligible("a:1", now) {
		t.Fatalf("the address must be eligible once its own backoff is released")
	}
	if h.FailureCount("a:1") != 1 {
		t.Fatalf("release must not wipe failure counts, got %d", h.FailureCount("a:1"))
	}

	// Someone else's longer backoff is left alone.
	h.SetBackoffUntil("b:1", now.Add(24*time.Hour))
	if h.ReleaseBackoff("b:1", ours) {
		t.Fatalf("a backoff we did not set must not be released")
	}
	if h.Eligible("b:1", now.Add(7*time.Hour)) {
		t.Fatalf("the longer backoff must still hold")
	}

	// Nothing recorded: nothing to release, and not an error.
	if h.ReleaseBackoff("c:1", ours) {
		t.Fatalf("releasing an address with no backoff reports false")
	}
}

// ---- launch-time enforcement of the give-up backoff ----

func TestProxyFailureHistory_Eligible_DefaultsTrueForUnmarkedAddress(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}

	if !h.Eligible("1.2.3.4:1080", time.Now()) {
		t.Fatal("an address with no recorded backoff should be eligible to launch")
	}
}

func TestProxyFailureHistory_SetBackoffUntil_GatesUntilTimeElapses(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}
	now := time.Now()

	h.SetBackoffUntil("1.2.3.4:1080", now.Add(time.Hour))

	if h.Eligible("1.2.3.4:1080", now) {
		t.Fatal("address should NOT be eligible before its backoff window elapses")
	}
	if h.Eligible("1.2.3.4:1080", now.Add(30*time.Minute)) {
		t.Fatal("address should NOT be eligible partway through its backoff window")
	}
	if !h.Eligible("1.2.3.4:1080", now.Add(time.Hour)) {
		t.Fatal("address should be eligible exactly at its next-eligible time")
	}
	if !h.Eligible("1.2.3.4:1080", now.Add(2*time.Hour)) {
		t.Fatal("address should be eligible after its backoff window elapses")
	}
	if !h.Eligible("5.6.7.8:1080", now) {
		t.Fatal("a different address must be unaffected by another's backoff")
	}
}

func TestProxyFailureHistory_Reset_ClearsBackoff(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}
	now := time.Now()

	h.SetBackoffUntil("1.2.3.4:1080", now.Add(24*time.Hour))
	h.Reset("1.2.3.4:1080")

	if !h.Eligible("1.2.3.4:1080", now) {
		t.Fatal("a proxy that succeeds (Reset) must immediately become eligible again")
	}
}

func TestProxyFailureHistory_Prune_RemovesBackoffForDroppedAddresses(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}
	now := time.Now()

	h.SetBackoffUntil("1.2.3.4:1080", now.Add(24*time.Hour))
	h.SetBackoffUntil("5.6.7.8:1080", now.Add(24*time.Hour))

	h.Prune(map[string]bool{"1.2.3.4:1080": true})

	if h.Eligible("1.2.3.4:1080", now) {
		t.Fatal("kept address must retain its backoff window")
	}
	if !h.Eligible("5.6.7.8:1080", now) {
		t.Fatal("dropped address's backoff must be pruned (eligible again)")
	}
}

// Only still-active windows count, matching Eligible's boundary.
func TestProxyFailureHistory_AddressesInBackoff_ExcludesElapsedWindows(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}
	now := time.Now()

	h.SetBackoffUntil("1.2.3.4:1080", now.Add(time.Hour))  // still in backoff
	h.SetBackoffUntil("5.6.7.8:1080", now.Add(-time.Hour)) // already elapsed
	h.SetBackoffUntil("9.9.9.9:1080", now)                 // elapsed exactly at now

	inBackoff := h.AddressesInBackoff(now)

	if !inBackoff["1.2.3.4:1080"] {
		t.Fatal("an address mid-backoff must be returned")
	}
	if inBackoff["5.6.7.8:1080"] {
		t.Fatal("an address whose backoff already elapsed must NOT be returned")
	}
	if inBackoff["9.9.9.9:1080"] {
		t.Fatal("an address whose backoff elapses exactly at now must NOT be returned (Eligible uses the same !now.Before(until) boundary)")
	}
	if len(inBackoff) != 1 {
		t.Fatalf("expected exactly 1 address still in backoff, got %d: %v", len(inBackoff), inBackoff)
	}
}

// ---- proven-proxy set ----

func TestProvenProxySet_TracksPerAddress(t *testing.T) {
	s := &provenProxySet{proven: map[string]bool{}}

	if s.HasSucceeded("1.2.3.4:1080") {
		t.Fatal("expected unmarked address to report not succeeded")
	}

	s.MarkSucceeded("1.2.3.4:1080")
	if !s.HasSucceeded("1.2.3.4:1080") {
		t.Fatal("expected marked address to report succeeded")
	}
	if s.HasSucceeded("5.6.7.8:1080") {
		t.Fatal("expected a different address to be unaffected")
	}
}

// ---- slow retry state ----

func TestSlowRetryAdoptLegacy_SingleIdentityCarriesOverClock(t *testing.T) {
	s := newProxySlowRetryState()
	const addr = "1.2.3.4:1080"
	started := time.Now().Add(-5 * 24 * time.Hour)
	s.Proxies[addr] = &proxySlowRetryEntry{StartedAt: started}

	desired := []*connect.ProxySettings{authSettings(addr, "alice", "pw1")}
	adopted, split := s.adoptLegacy(desired)

	if adopted != 1 || split != 0 {
		t.Fatalf("adopted=%d split=%d, want 1,0", adopted, split)
	}
	if _, stillLegacy := s.Proxies[addr]; stillLegacy {
		t.Error("legacy slow-retry entry still present after adoption")
	}
	got, ok := s.Proxies[desired[0].Key()]
	if !ok || !got.StartedAt.Equal(started) {
		t.Fatalf("adopted entry lost its StartedAt (the 14-day drop clock): got=%+v ok=%v, want StartedAt=%v", got, ok, started)
	}
}

// TestSlowRetryAdoptLegacy_SplitLoserStartsFreshNotDropped is the
// production-safety property: the identity that does NOT win the legacy
// entry must simply have no slow-retry record at all, not inherit a
// near-expired drop clock or a stale DroppedAt.
func TestSlowRetryAdoptLegacy_SplitLoserStartsFreshNotDropped(t *testing.T) {
	const addr = "dc.decodo.com:10058"
	s := newProxySlowRetryState()
	started := time.Now().Add(-13 * 24 * time.Hour) // one day from being dropped
	s.Proxies[addr] = &proxySlowRetryEntry{StartedAt: started}

	userA := authSettings(addr, "sppmr4vcnj", "pw1")
	userB := authSettings(addr, "user-sppmr4vcnj-country-us-city-metro", "pw2")

	adopted, split := s.adoptLegacy([]*connect.ProxySettings{userA, userB})
	if adopted != 1 || split != 1 {
		t.Fatalf("adopted=%d split=%d, want 1,1", adopted, split)
	}
	if _, exists := s.Proxies[userB.Key()]; exists {
		t.Error("loser identity must have no slow-retry entry at all after a split, not an inherited near-expired clock")
	}
	if got, ok := s.Proxies[userA.Key()]; !ok || !got.StartedAt.Equal(started) {
		t.Errorf("winner lost its clock: got=%+v ok=%v", got, ok)
	}
}

func TestSlowRetryAdoptLegacy_NoAuthIsANoOp(t *testing.T) {
	s := newProxySlowRetryState()
	const addr = "1.2.3.4:1080"
	started := time.Now()
	s.Proxies[addr] = &proxySlowRetryEntry{StartedAt: started}

	adopted, split := s.adoptLegacy([]*connect.ProxySettings{noAuthSettings(addr)})

	if adopted != 0 || split != 0 {
		t.Fatalf("adopted=%d split=%d, want 0,0", adopted, split)
	}
	if got := s.Proxies[addr]; !got.StartedAt.Equal(started) {
		t.Errorf("no-auth entry was disturbed: %+v", got)
	}
}

// The slow-retry semaphore caps how many failing paid or file proxies can sit
// in the auth pipeline at once. A genuine give-up pins authFailures one BELOW
// the ceiling and zeroes slowRetryCycles, so the condition must also look at
// the genuine retry cycle counter or every ramp and daily attempt after the
// first give-up skips the semaphore.
func TestUsesSlowRetrySemaphore(t *testing.T) {
	const testMaxAuthFailures = 10
	cases := []struct {
		name                     string
		proxy, urlSourced        bool
		authFailures             int
		slowCycles, genuineCycle int
		want                     bool
	}{
		{"fresh proxy in its first ladder", true, false, 0, 0, 0, false},
		{"ladder exhausted", true, false, testMaxAuthFailures, 0, 0, true},
		{"in the slow retry ramp", true, false, testMaxAuthFailures - 1, 1, 0, true},
		{"after a genuine give-up (authFailures pinned below the ceiling)", true, false, testMaxAuthFailures - 1, 0, 1, true},
		{"later genuine cycle", true, false, testMaxAuthFailures - 1, 0, 7, true},
		{"URL-sourced proxies never use it", true, true, testMaxAuthFailures, 2, 3, false},
		{"the direct connection never queues", false, false, testMaxAuthFailures, 2, 3, false},
	}
	for _, tc := range cases {
		got := usesSlowRetrySemaphore(tc.proxy, tc.urlSourced, tc.authFailures, testMaxAuthFailures, tc.slowCycles, tc.genuineCycle)
		if got != tc.want {
			t.Errorf("%s: usesSlowRetrySemaphore = %v, want %v", tc.name, got, tc.want)
		}
	}
}
