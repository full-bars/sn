package miner

// The status is where this host learns what the operator made of its activation
// posts, and it reports two different failures through the same field: an
// answer that came back activated: false, and an attempt that never got an
// answer at all. What the budget holds for each of them is pinned here, because
// holding a start for a post nothing refused costs the host the activation it
// would have got.

import (
	"testing"
	"time"

	"github.com/urnetwork/sdk"
)

// extenderListenerAttempt is one status the operator answered, at one attempt.
// The attempt time is what tells two failures apart, so the same error text at
// a later attempt is a new failure and not the same one re-read.
func extenderListenerAttempt(now time.Time, attempt int, refused bool) *sdk.ExtenderProvideStatus {
	return &sdk.ExtenderProvideStatus{
		Enabled:               true,
		LastActivationTime:    now.Unix() + int64(attempt),
		LastActivationError:   "the operator answered without activating the post",
		LastActivationRefused: refused,
	}
}

// extenderListenerState is the account's budget record as the status left it.
func extenderListenerState(t *testing.T, account string) extenderActivationState {
	t.Helper()
	record, err := readExtenderActivationRecord(account)
	if err != nil {
		t.Fatalf("the status wrote no readable record for %q: %s", account, err)
	}
	state, ok := record.Accounts[account]
	if !ok {
		t.Fatalf("the status wrote no record for %q", account)
	}
	return state
}

// An answer of activated: false is a refusal, and the wait it earns grows with
// the refusals behind it: the operator counts the post either way, so the next
// start must not spend another inside the window it closed.
func TestProviderExtenderStatusRecordsARefusalForAnOperatorRefusal(t *testing.T) {
	testExtenderState(t)
	now := time.Now()
	// v4 is the only family this host posts, so the refusal is one post.
	writeHostCapability(hostCapability{IPv6Usable: false, CheckedAtUnix: now.Unix()})

	listener := newProviderExtenderStatusListener(nil, 0, extenderTestAccountA)
	listener.ExtenderProvideStatusChanged(extenderListenerAttempt(now, 0, true))

	first := extenderListenerState(t, extenderTestAccountA)
	if first.Refusals != 1 {
		t.Fatalf("refusals = %d, want 1: the operator refused the post", first.Refusals)
	}
	if first.BackoffUntilUnix < now.Add(extenderRefusalBackoff).Unix() {
		t.Fatalf("backoff until %d, want at least %d", first.BackoffUntilUnix, now.Add(extenderRefusalBackoff).Unix())
	}

	// A second refusal is a second attempt, so it escalates the wait.
	listener.ExtenderProvideStatusChanged(extenderListenerAttempt(now, 1, true))
	second := extenderListenerState(t, extenderTestAccountA)
	if second.Refusals != 2 {
		t.Fatalf("refusals = %d, want 2: the second attempt was charged as the first", second.Refusals)
	}
	if second.BackoffUntilUnix <= first.BackoffUntilUnix {
		t.Fatalf("backoff until %d, want it past the first refusal's %d", second.BackoffUntilUnix, first.BackoffUntilUnix)
	}
}

// LastActivationError is set for every failed attempt, so a failure the
// operator never answered is not a refusal: recording a wait for it holds the
// next start for a post nothing refused, and the host loses the activation it
// would have got.
func TestProviderExtenderStatusDoesNotHoldForAFailureWithoutARefusal(t *testing.T) {
	testExtenderState(t)
	now := time.Now()
	writeHostCapability(hostCapability{IPv6Usable: false, CheckedAtUnix: now.Unix()})

	listener := newProviderExtenderStatusListener(nil, 0, extenderTestAccountA)
	// The post never reached the operator: an error stands, and nothing
	// answered activated: false.
	listener.ExtenderProvideStatusChanged(extenderListenerAttempt(now, 0, false))

	state := extenderListenerState(t, extenderTestAccountA)
	if state.Refusals != 0 {
		t.Fatalf("refusals = %d, want 0: a transient failure was recorded as a refusal", state.Refusals)
	}
	if state.BackoffUntilUnix != 0 {
		t.Fatalf("backoff until %d, want no wait: the operator refused nothing", state.BackoffUntilUnix)
	}
	if allow, wait, used := extenderActivationAllow(now, 1, extenderTestAccountA); !allow {
		t.Fatalf("the next attempt was held for %s with %d posts in the window, want it allowed", formatDuration(wait), used)
	}
}
