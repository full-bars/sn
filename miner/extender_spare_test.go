package miner

// The window is five posts against the operator's six. A pass spends one post
// per family, so a dual-stack host spends four on two passes and cannot afford
// a third, and the fifth post is stranded. These pin what may spend it: one
// retry of a family that failed, never a family that activated, never past the
// ceiling.

import (
	"testing"
	"time"

	"github.com/urnetwork/sdk"
)

// The per-family outcomes the tests feed in.
var (
	extenderTestFamilyActivated = extenderActivationFamilyResult{Tried: true, Activated: true}
	extenderTestFamilyFailed    = extenderActivationFamilyResult{Tried: true}
	// extenderTestFamilyUntried is a family the pass never posted.
	extenderTestFamilyUntried = extenderActivationFamilyResult{}
)

// extenderWindowPosts is how many posts the account's window holds at now.
func extenderWindowPosts(t *testing.T, account string, now time.Time) int {
	t.Helper()
	record, found := readExtenderActivationRecord(account)
	if !found {
		return 0
	}
	state := record.Accounts[extenderActivationAccountKey(account)]
	return pruneExtenderAttempts(&state, now, extenderActivationWindow)
}

// spendTwoPasses is a host that has spent the four posts two dual-stack passes
// cost, which is the state the stranded post exists in.
func spendTwoPasses(t *testing.T, now time.Time, account string) {
	t.Helper()
	for i := 0; i < 2; i++ {
		if allow, _, _ := extenderActivationAllow(now.Add(time.Duration(i)*time.Minute), postsPerDualStackPass, account); !allow {
			t.Fatalf("pass %d was held with the window empty", i+1)
		}
	}
}

// The post a dual-stack host cannot spend on a pass goes to the family that
// failed, and the window ends at the budget rather than one short of it.
func TestExtenderActivationSpareFundsTheFailedFamily(t *testing.T) {
	cases := []struct {
		name string
		pass extenderActivationPass
		want extenderActivationFamily
	}{
		{
			name: "v4 failed",
			pass: extenderActivationPass{V4: extenderTestFamilyFailed, V6: extenderTestFamilyActivated},
			want: extenderActivationFamilyV4,
		},
		{
			name: "v6 failed",
			pass: extenderActivationPass{V4: extenderTestFamilyActivated, V6: extenderTestFamilyFailed},
			want: extenderActivationFamilyV6,
		},
	}
	for i, c := range cases {
		testExtenderState(t)
		now := time.Unix(1_800_000_000, 0)
		account := extenderTestAccount(i)
		spendTwoPasses(t, now, account)
		at := now.Add(2 * time.Minute)
		// The third pass cannot be afforded, which is what strands the post.
		if allow, _, used := extenderActivationAllow(at, postsPerDualStackPass, account); allow || used != 4 {
			t.Fatalf("%s: third pass allow=%v used=%d, want held with 4 counted", c.name, allow, used)
		}
		family, spent := extenderActivationSpendSpare(at, account, c.pass)
		if !spent || family != c.want {
			t.Fatalf("%s: family=%q spent=%v, want %q spent", c.name, family, spent, c.want)
		}
		if got := extenderWindowPosts(t, account, at); got != extenderActivationBudget {
			t.Fatalf("%s: window holds %d posts, want the budget %d", c.name, got, extenderActivationBudget)
		}
	}
}

// A pass that activated both families has nothing to retry, so the post stays
// where it is: the operator records every post and a duplicate buys nothing.
func TestExtenderActivationSpareStaysUnspentWhenBothFamiliesActivate(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	spendTwoPasses(t, now, extenderTestAccountA)
	at := now.Add(2 * time.Minute)

	bothActivated := extenderActivationPass{V4: extenderTestFamilyActivated, V6: extenderTestFamilyActivated}
	if family, spent := extenderActivationSpendSpare(at, extenderTestAccountA, bothActivated); spent {
		t.Fatalf("spent the spare on %q after both families activated", family)
	}
	// A family the pass never posted is not a target either.
	untried := extenderActivationPass{V4: extenderTestFamilyActivated, V6: extenderTestFamilyUntried}
	if family, spent := extenderActivationSpendSpare(at, extenderTestAccountA, untried); spent {
		t.Fatalf("spent the spare on %q, which the pass never tried", family)
	}
	if got := extenderWindowPosts(t, extenderTestAccountA, at); got != 4 {
		t.Fatalf("window holds %d posts, want the 4 the passes spent", got)
	}
}

// IPv4 is the path that still carries traffic when v6 does not work, so a pass
// that failed on both families is retried on v4.
func TestExtenderActivationSparePrefersIPv4WhenBothFamiliesFail(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	spendTwoPasses(t, now, extenderTestAccountA)

	family, spent := extenderActivationSpendSpare(now.Add(2*time.Minute), extenderTestAccountA, extenderActivationPass{
		V4: extenderTestFamilyFailed,
		V6: extenderTestFamilyFailed,
	})
	if !spent || family != extenderActivationFamilyV4 {
		t.Fatalf("family=%q spent=%v, want ipv4 spent", family, spent)
	}
}

// A retry is not charged while a refusal wait still stands: it cannot run
// before the wait ends, and the post would be spent on nothing.
func TestExtenderActivationSpareWaitsOutTheRefusalBackoff(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	failed := extenderActivationPass{V4: extenderTestFamilyFailed}

	recordExtenderActivationRefused(now, extenderTestAccountA)
	if family, spent := extenderActivationSpendSpare(now.Add(time.Minute), extenderTestAccountA, failed); spent {
		t.Fatalf("spent the spare on %q inside the refusal backoff", family)
	}
	if got := extenderWindowPosts(t, extenderTestAccountA, now.Add(time.Minute)); got != 0 {
		t.Fatalf("window holds %d posts, want none", got)
	}
	family, spent := extenderActivationSpendSpare(now.Add(extenderRefusalBackoff), extenderTestAccountA, failed)
	if !spent || family != extenderActivationFamilyV4 {
		t.Fatalf("after the wait: family=%q spent=%v, want ipv4 spent", family, spent)
	}
}

// The spare buys one retry, not a retry loop: once a window has funded one, a
// later failure in the same window cannot fund another, and the window never
// passes the budget.
func TestExtenderActivationSpareIsOneRetryPerWindow(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	failed := extenderActivationPass{V4: extenderTestFamilyFailed}
	spendTwoPasses(t, now, extenderTestAccountA)
	at := now.Add(2 * time.Minute)

	if _, spent := extenderActivationSpendSpare(at, extenderTestAccountA, failed); !spent {
		t.Fatal("the first failure in the window did not fund a retry")
	}
	if family, spent := extenderActivationSpendSpare(at.Add(time.Minute), extenderTestAccountA, failed); spent {
		t.Fatalf("a second failure in the window funded %q", family)
	}
	if got := extenderWindowPosts(t, extenderTestAccountA, at.Add(time.Minute)); got != extenderActivationBudget {
		t.Fatalf("window holds %d posts, want the budget %d", got, extenderActivationBudget)
	}

	// Once the window that funded it has passed, a failure may fund one again.
	later := at.Add(extenderActivationWindow + 2*time.Minute)
	if family, spent := extenderActivationSpendSpare(later, extenderTestAccountA, failed); !spent || family != extenderActivationFamilyV4 {
		t.Fatalf("a window later: family=%q spent=%v, want ipv4 spent", family, spent)
	}
}

// Nothing may put the window past the budget, whether a pass asks or the spare
// does, and the operator's own six stays out of reach.
func TestExtenderActivationSpareNeverExceedsTheBudget(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	// A window already at the budget with no retry funded: only the ceiling
	// stands between it and a sixth post.
	attempts := make([]int64, 0, extenderActivationBudget)
	for i := 0; i < extenderActivationBudget; i++ {
		attempts = append(attempts, now.Add(-time.Duration(i)*time.Minute).Unix())
	}
	record := extenderActivationRecordForAccount(extenderTestAccountA)
	record.Accounts[extenderTestAccountA] = extenderActivationState{Attempts: attempts}
	writeExtenderActivationRecord(record, extenderTestAccountA)

	failed := extenderActivationPass{V4: extenderTestFamilyFailed}
	if family, spent := extenderActivationSpendSpare(now, extenderTestAccountA, failed); spent {
		t.Fatalf("spent a post on %q with the window full", family)
	}
	if allow, _, _ := extenderActivationAllow(now, 1, extenderTestAccountA); allow {
		t.Fatal("a full window allowed another post")
	}
	if got := extenderWindowPosts(t, extenderTestAccountA, now); got != extenderActivationBudget {
		t.Fatalf("window holds %d posts, want the budget %d", got, extenderActivationBudget)
	}
}

// A v6 post is only charged to a host whose own measurement says v6 is usable,
// so a v4-only host never funds a v6 retry it cannot use.
func TestExtenderActivationPassFollowsTheHostCapability(t *testing.T) {
	testExtenderState(t)
	writeHostCapability(hostCapability{IPv6Usable: false, CheckedAtUnix: 1_800_000_000})

	observation := providerExtenderObservation{Known: true, Enabled: true, ActivationFailed: true}
	pass := observation.activationPass()
	if !pass.V4.Tried || pass.V4.Activated {
		t.Fatalf("v4 result = %+v, want tried and not activated", pass.V4)
	}
	if pass.V6.Tried {
		t.Fatalf("v6 result = %+v on a host measured without v6, want it never tried", pass.V6)
	}

	writeHostCapability(hostCapability{IPv6Usable: true, CheckedAtUnix: 1_800_000_000})
	observation.ActivatedV4 = true
	pass = observation.activationPass()
	if !pass.V4.Activated {
		t.Fatalf("v4 result = %+v, want the status's activation", pass.V4)
	}
	if !pass.V6.Tried || pass.V6.Activated {
		t.Fatalf("v6 result = %+v, want tried and not activated", pass.V6)
	}
}

// A pass can activate one family and fail the other, and the status carries
// both facts at once. That is a failure: reading it as a success records no
// refusal, so the next start posts into the same wall, and it strands the
// leftover post that is meant to fund the retry.
func TestProviderExtenderStatusRecordsARefusalForAPartlyActivatedPass(t *testing.T) {
	testExtenderState(t)
	now := time.Now()
	// Two dual-stack passes spend four of the five posts, which is the state
	// the spare exists in.
	spendTwoPasses(t, now, extenderTestAccountA)
	// This host's own measurement says v6 is usable, so the pass posted v6 and
	// the failure is v6's.
	writeHostCapability(hostCapability{IPv6Usable: true, CheckedAtUnix: now.Unix()})

	listener := newProviderExtenderStatusListener(nil, 0, extenderTestAccountA)
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{
		Enabled:             true,
		ActivatedV4:         true,
		LastActivationTime:  now.Unix(),
		LastActivationError: "the operator refused the v6 post",
	})
	if !listener.last.ActivatedV4 || listener.last.ActivatedV6 || !listener.last.ActivationFailed {
		t.Fatalf("observation = %+v, want the mixed pass the status described", listener.last)
	}

	record, found := readExtenderActivationRecord(listener.account)
	if !found {
		t.Fatal("the mixed pass wrote no record")
	}
	state := record.Accounts[listener.account]
	if state.Refusals != 1 {
		t.Fatalf("refusals = %d, want 1: the failed family was recorded as a success", state.Refusals)
	}
	// The wait is stamped from the wall clock, so the assertion is the wait it
	// implies rather than the second it landed on.
	if state.BackoffUntilUnix < now.Add(extenderRefusalBackoff).Unix() {
		t.Fatalf("backoff until %d, want at least %d", state.BackoffUntilUnix, now.Add(extenderRefusalBackoff).Unix())
	}
	if state.SpareFamily != string(extenderActivationFamilyV6) || state.SpareAtUnix == 0 {
		t.Fatalf("spare family = %q at %d, want the v6 retry funded", state.SpareFamily, state.SpareAtUnix)
	}
	if got := extenderWindowPosts(t, listener.account, time.Now()); got != extenderActivationBudget {
		t.Fatalf("window holds %d posts, want the budget %d", got, extenderActivationBudget)
	}
}
