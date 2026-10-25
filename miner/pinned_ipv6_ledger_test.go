package miner

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestPinnedIpv6DecideAtStart pins the policy table: only a fresh yes starts the
// pinned v6 transport without asking, a fresh no holds it, and everything else
// probes in the background while IPv4 and the standby start immediately.
func TestPinnedIpv6DecideAtStart(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name         string
		state        pinnedIpv6State
		wantEnable   bool
		wantProbe    bool
		wantContains string
	}{
		{
			name:         "never probed probes and holds",
			state:        pinnedIpv6State{},
			wantProbe:    true,
			wantContains: "unknown",
		},
		{
			name:         "fresh yes enables without probing",
			state:        pinnedIpv6State{Verdict: pinnedIpv6Yes, CheckedAt: now.Add(-time.Hour), Class: pinnedIpv6ClassConnected},
			wantEnable:   true,
			wantContains: "proven",
		},
		{
			name:         "stale yes probes again",
			state:        pinnedIpv6State{Verdict: pinnedIpv6Yes, CheckedAt: now.Add(-7 * time.Hour), Class: pinnedIpv6ClassConnected},
			wantProbe:    true,
			wantContains: "stale",
		},
		{
			name:         "fresh no stays parked without probing",
			state:        pinnedIpv6State{Verdict: pinnedIpv6No, CheckedAt: now.Add(-5 * time.Minute), Class: pinnedIpv6ClassAddrType},
			wantContains: "parked",
		},
		{
			name:         "stale no re-probes",
			state:        pinnedIpv6State{Verdict: pinnedIpv6No, CheckedAt: now.Add(-20 * time.Minute), Class: pinnedIpv6ClassAddrType},
			wantProbe:    true,
			wantContains: "re-probing",
		},
		{
			name:         "yes with no timestamp is not trusted",
			state:        pinnedIpv6State{Verdict: pinnedIpv6Yes},
			wantProbe:    true,
			wantContains: "stale",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pinnedIpv6DecideAtStart(c.state, now)
			if got.EnablePinnedV6 != c.wantEnable || got.Probe != c.wantProbe {
				t.Fatalf("decision = enable %v probe %v, want enable %v probe %v (%s)",
					got.EnablePinnedV6, got.Probe, c.wantEnable, c.wantProbe, got.Reason)
			}
			if c.wantContains != "" && !strings.Contains(got.Reason, c.wantContains) {
				t.Fatalf("reason %q does not mention %q", got.Reason, c.wantContains)
			}
		})
	}
}

// TestPinnedIpv6FreshnessWindows pins the two windows exactly, including the
// boundary: a verdict exactly at the window is stale, so a six-hour-old yes
// re-proves rather than lingering.
func TestPinnedIpv6FreshnessWindows(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		state pinnedIpv6State
		want  bool
	}{
		{"yes just inside", pinnedIpv6State{Verdict: pinnedIpv6Yes, CheckedAt: now.Add(-pinnedIpv6YesFreshness + time.Second)}, true},
		{"yes exactly at the window", pinnedIpv6State{Verdict: pinnedIpv6Yes, CheckedAt: now.Add(-pinnedIpv6YesFreshness)}, false},
		{"no just inside", pinnedIpv6State{Verdict: pinnedIpv6No, CheckedAt: now.Add(-pinnedIpv6NoFreshness + time.Second)}, true},
		{"no exactly at the window", pinnedIpv6State{Verdict: pinnedIpv6No, CheckedAt: now.Add(-pinnedIpv6NoFreshness)}, false},
		{"zero timestamp", pinnedIpv6State{Verdict: pinnedIpv6Yes}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := pinnedIpv6Fresh(c.state, now); got != c.want {
				t.Fatalf("fresh = %v, want %v", got, c.want)
			}
		})
	}
}

// TestPinnedIpv6FailuresParkOnTheThird pins the strike rule and that a success
// outranks it: two failures leave the proxy alone, the third parks it, and one
// success afterwards clears both the verdict and the count.
func TestPinnedIpv6FailuresParkOnTheThird(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	state := pinnedIpv6State{Verdict: pinnedIpv6Unknown}

	state, parked := notePinnedIpv6Failure(state, now)
	if parked {
		t.Fatal("one failure parked the proxy; a single dial failure is noise")
	}
	state, parked = notePinnedIpv6Failure(state, now)
	if parked {
		t.Fatal("two failures parked the proxy; the 5s doubling backoff absorbs two")
	}
	state, parked = notePinnedIpv6Failure(state, now)
	if !parked {
		t.Fatalf("three failures did not park the proxy: %+v", state)
	}
	if state.Verdict != pinnedIpv6No || state.Class != pinnedIpv6ClassIsolatedFailed {
		t.Fatalf("parked state = verdict %q class %q, want no/isolated", state.Verdict, state.Class)
	}
	if state.ConsecutiveFailed != 0 {
		t.Fatalf("parked state kept a failure count of %d; the count belongs to the attempt streak, not the verdict", state.ConsecutiveFailed)
	}

	cleared := notePinnedIpv6Success(state, now.Add(time.Minute))
	if cleared.Verdict != pinnedIpv6Yes || cleared.Class != pinnedIpv6ClassConnected || cleared.ConsecutiveFailed != 0 {
		t.Fatalf("success did not clear the park: %+v", cleared)
	}

	// And the fresh verdict really does change the decision.
	if d := pinnedIpv6DecideAtStart(state, now.Add(time.Minute)); d.EnablePinnedV6 || d.Probe {
		t.Fatalf("a fresh park should hold v6 silently, got %+v", d)
	}
	if d := pinnedIpv6DecideAtStart(cleared, now.Add(time.Minute)); !d.EnablePinnedV6 {
		t.Fatalf("a fresh success should enable v6, got %+v", d)
	}
}

// TestClassifyPinnedIpv6Probe pins which failures are allowed to park IPv6.
// Only a definitive refusal about the family counts; ambiguity re-probes.
func TestClassifyPinnedIpv6Probe(t *testing.T) {
	for _, c := range []struct {
		name     string
		err      error
		sawEOF   bool
		wantVerb pinnedIpv6Verdict
		wantCls  string
	}{
		{"success", nil, false, pinnedIpv6Yes, pinnedIpv6ClassConnected},
		{"address type", errors.New("socks connect: address type not supported"), false, pinnedIpv6No, pinnedIpv6ClassAddrType},
		{"ruleset", errors.New("socks connect: connection not allowed by ruleset"), false, pinnedIpv6No, pinnedIpv6ClassRuleset},
		{"host unreachable", errors.New("socks connect: host unreachable"), false, pinnedIpv6No, pinnedIpv6ClassHostUnreach},
		{"proxy closed without reply", errors.New("read tcp: EOF"), true, pinnedIpv6No, pinnedIpv6ClassImmediateEOF},
		// Refused is the target, not the family: it must not park v6.
		{"refused", errors.New("dial tcp: connection refused"), false, pinnedIpv6Unknown, pinnedIpv6ClassRefused},
		{"timeout", timeoutError{}, false, pinnedIpv6Unknown, pinnedIpv6ClassTimeout},
		{"proxy unreachable", syscall.ENETUNREACH, false, pinnedIpv6Unknown, pinnedIpv6ClassProxyUnreach},
		{"something else", errors.New("tls: handshake failure"), false, pinnedIpv6Unknown, pinnedIpv6ClassUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			verdict, class := classifyPinnedIpv6Probe(c.err, c.sawEOF)
			if verdict != c.wantVerb || class != c.wantCls {
				t.Fatalf("classify = %q/%q, want %q/%q", verdict, class, c.wantVerb, c.wantCls)
			}
			// The invariant that matters: ambiguity never parks.
			if verdict == pinnedIpv6Unknown {
				state, parked := notePinnedIpv6Failure(pinnedIpv6State{Verdict: pinnedIpv6Unknown}, time.Now())
				_ = state
				if parked {
					t.Fatal("an unknown probe result parked the proxy")
				}
			}
		})
	}
}

// TestPinnedIpv6EntryRoundTrip pins the on-disk shape: the verdict survives
// through ProxyEntry, and "unknown" is written as absent rather than as a
// third state an older reader would reject.
func TestPinnedIpv6EntryRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	entry := ProxyEntry{}
	if got := entry.pinnedIpv6State(); got.Verdict != "" || got.ConsecutiveFailed != 0 {
		t.Fatalf("a fresh entry is not the empty state: %+v", got)
	}
	entry.setPinnedIpv6State(pinnedIpv6State{Verdict: pinnedIpv6No, CheckedAt: now, Class: pinnedIpv6ClassAddrType})
	if entry.Ipv6Verdict != "no" || entry.Ipv6Class != pinnedIpv6ClassAddrType || !entry.Ipv6CheckedAt.Equal(now) {
		t.Fatalf("entry = %+v", entry)
	}
	back := entry.pinnedIpv6State()
	if back.Verdict != pinnedIpv6No || back.Class != pinnedIpv6ClassAddrType || !back.CheckedAt.Equal(now) {
		t.Fatalf("round trip = %+v", back)
	}
	entry.setPinnedIpv6State(pinnedIpv6State{Verdict: pinnedIpv6Unknown})
	if entry.Ipv6Verdict != "" {
		t.Fatalf("unknown should be written as absent, got %q", entry.Ipv6Verdict)
	}
}

// timeoutError is a net.Error that reports Timeout true, so the classifier's
// timeout branch is exercised without sleeping or touching the network.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

var _ net.Error = timeoutError{}
