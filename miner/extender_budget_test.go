package miner

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urfoundation/sn/clientauth"
	"github.com/urnetwork/sdk"
)

// postsPerDualStackPass is the charge for a host that can post on both
// families, which is what the budget tests exercise.
const postsPerDualStackPass = 2

// The account ids the budget tests charge. The record is keyed by the sign-in
// token's user_id, so a test name is enough to stand for one.
const (
	extenderTestAccountA = "account-a"
	extenderTestAccountB = "account-b"
)

// The client ids the identity tests write. A client is what a heal replaces;
// the budget no longer keys on it.
const (
	extenderTestClientA = "6f1c2ad0-2e3f-4b8a-9d1c-0a5b7c9e1234"
	extenderTestClientB = "0b7d3a1e-5c42-4f68-8a91-2d4e6f8a0b1c"
)

// testExtenderState gives each test its own state directory and its own store
// handle, and closes the handle afterwards so the next test starts clean.
func testExtenderState(t *testing.T) {
	t.Helper()
	t.Setenv("URNETWORK_STATE_DIR", shortStateDir(t))
	closeProviderStore()
	resetExtenderRefusals()
	t.Cleanup(func() {
		resetExtenderRefusals()
		closeProviderStore()
	})
}

// extenderTestAccount names the nth account of a test that churns sign-ins.
func extenderTestAccount(n int) string {
	return fmt.Sprintf("account-%02d", n)
}

// writeAccountToken writes the sign-in token the budget keys on, where the
// miner reads it (<state>/jwt). Its user_id is the account key.
func writeAccountToken(t *testing.T, userId string) {
	t.Helper()
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims{
		"user_id":    userId,
		"network_id": "8f14e45f-ceea-4f6b-9a3d-6c1f0d2b7e55",
	}).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign the test account token: %s", err)
	}
	path, err := providerStatePath("jwt")
	if err != nil {
		t.Fatalf("account token path: %s", err)
	}
	if err := clientauth.WriteToken(path, token); err != nil {
		t.Fatalf("write the test account token: %s", err)
	}
}

// writeClientCredential writes the direct slot's durable client credential,
// the identity a heal replaces. The budget no longer keys on it, so a test
// writes a new one to show the account's budget does not move.
func writeClientCredential(t *testing.T, clientId string) {
	t.Helper()
	claims := gojwt.MapClaims{"client_id": clientId, "device_id": "0b7d3a1e-5c42-4f68-8a91-2d4e6f8a0b1c"}
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, claims).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign the test client credential: %s", err)
	}
	path, err := providerClientJwtPath(providerSlotIdentities{}, nil)
	if err != nil {
		t.Fatalf("direct client credential path: %s", err)
	}
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write the test client credential: %s", err)
	}
}

// The budget follows the account, not the client identity: every client of one
// sign-in shares the operator's per-user window, so a heal to a fresh client
// must not hand the host a fresh budget.
func TestExtenderActivationBudgetIsPerAccount(t *testing.T) {
	testExtenderState(t)
	writeAccountToken(t, extenderTestAccountA)
	writeClientCredential(t, extenderTestClientA)

	gate := newExtenderActivationGate()
	if !gate.allowLaunch() {
		t.Fatal("the first launch under a fresh account must be allowed")
	}
	if !gate.allowLaunch() {
		t.Fatal("the second launch must be allowed: four posts are within the budget")
	}

	// The slot heals to a new client identity under the same account.
	writeClientCredential(t, extenderTestClientB)
	if gate.allowLaunch() {
		t.Fatal("a healed client under the same account must spend the same budget, not a fresh one")
	}
}

// extenderStoreAsSecondHost drops the process-wide handle without moving the
// state directory: the next call opens the same record, the way a second
// process on the same state directory would.
func extenderStoreAsSecondHost() {
	closeProviderStore()
}

// writeLegacyExtenderActivation seeds the pre-account shape, which is what a
// store written by an older build holds.
func writeLegacyExtenderActivation(t *testing.T, legacy extenderActivationLegacy) {
	t.Helper()
	if err := providerStoreInstance().save(providerStoreBucketState, providerStoreKeyExtenderActivation, legacy); err != nil {
		t.Fatalf("seed the legacy record: %s", err)
	}
}

// The operator allows six activation posts per account per hour. A start
// spends two, and the budget is persisted, so a restart cannot spend them
// again.
func TestExtenderActivationBudgetHoldsTheThirdStart(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)

	if allow, _, used := extenderActivationAllow(now, postsPerDualStackPass, extenderTestAccountA); !allow || used != 0 {
		t.Fatalf("first start: allow=%v used=%d, want allow=true used=0", allow, used)
	}
	if allow, _, used := extenderActivationAllow(now.Add(time.Minute), postsPerDualStackPass, extenderTestAccountA); !allow || used != 2 {
		t.Fatalf("second start: allow=%v used=%d, want allow=true used=2", allow, used)
	}
	// Four posts are counted; a third start would post six and hit the
	// operator's cap, so it is held until the first post ages out.
	allow, wait, used := extenderActivationAllow(now.Add(2*time.Minute), postsPerDualStackPass, extenderTestAccountA)
	if allow {
		t.Fatal("third start must be held")
	}
	if used != 4 {
		t.Fatalf("used=%d, want 4", used)
	}
	if want := extenderActivationWindow - 2*time.Minute; wait != want {
		t.Fatalf("wait=%s, want %s", wait, want)
	}
	// A held start records nothing, so a restart loop cannot spend the budget.
	if allow, _, _ := extenderActivationAllow(now.Add(3*time.Minute), postsPerDualStackPass, extenderTestAccountA); allow {
		t.Fatal("a held start must not open the budget")
	}
	if allow, _, used := extenderActivationAllow(now.Add(extenderActivationWindow+2*time.Minute), postsPerDualStackPass, extenderTestAccountA); !allow || used != 0 {
		t.Fatalf("after the window: allow=%v used=%d, want allow=true used=0", allow, used)
	}
}

// The budget survives a restart: a new process opens the same file and reads it.
func TestExtenderActivationBudgetSurvivesARestart(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	extenderActivationAllow(now, postsPerDualStackPass, extenderTestAccountA)
	extenderActivationAllow(now.Add(time.Minute), postsPerDualStackPass, extenderTestAccountA)

	// A restart drops the process-wide handle and opens the file again.
	extenderStoreAsSecondHost()
	allow, _, used := extenderActivationAllow(now.Add(2*time.Minute), postsPerDualStackPass, extenderTestAccountA)
	if allow || used != 4 {
		t.Fatalf("after a restart: allow=%v used=%d, want allow=false used=4", allow, used)
	}
}

// A different account is a different budget: the operator counts per user, so
// one account's spent window does not hold another's.
func TestExtenderActivationBudgetIsPerAccountSeparately(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)

	extenderActivationAllow(now, postsPerDualStackPass, extenderTestAccountA)
	extenderActivationAllow(now.Add(time.Minute), postsPerDualStackPass, extenderTestAccountA)
	if allow, _, used := extenderActivationAllow(now.Add(2*time.Minute), postsPerDualStackPass, extenderTestAccountA); allow {
		t.Fatalf("account A: third start allowed with used=%d, want held", used)
	}
	if allow, _, used := extenderActivationAllow(now.Add(2*time.Minute), postsPerDualStackPass, extenderTestAccountB); !allow || used != 0 {
		t.Fatalf("account B: allow=%v used=%d, want allow=true used=0", allow, used)
	}
}

// Two hosts that sign in to one account are one budget to the operator: the
// second host is held by what the first spent, so together they cannot spend
// more than the account's six.
func TestExtenderActivationBudgetIsSharedByOneAccountAcrossHosts(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	const shared = "shared-account"

	extenderActivationAllow(now, postsPerDualStackPass, shared)
	extenderActivationAllow(now.Add(time.Minute), postsPerDualStackPass, shared)

	// The second host opens the same record and starts under the same account.
	extenderStoreAsSecondHost()
	allow, _, used := extenderActivationAllow(now.Add(2*time.Minute), postsPerDualStackPass, shared)
	if allow || used != 4 {
		t.Fatalf("second host on the shared account: allow=%v used=%d, want allow=false used=4", allow, used)
	}
	// A sibling host signed in to a different account has its own budget.
	if allow, _, used := extenderActivationAllow(now.Add(2*time.Minute), postsPerDualStackPass, extenderTestAccountA); !allow || used != 0 {
		t.Fatalf("sibling host on another account: allow=%v used=%d, want allow=true used=0", allow, used)
	}
}

// A record written before the budget was keyed by any identity belongs to the
// account signed in now: the upgrade keeps the identity, so its attempts and
// its refusal streak are that account's, and they are rewritten in the new
// shape.
func TestExtenderActivationBudgetMigratesTheFlatRecord(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	writeLegacyExtenderActivation(t, extenderActivationLegacy{
		Attempts:        []int64{now.Add(-30 * time.Minute).Unix()},
		Refusals:        1,
		LastRefusalUnix: now.Add(-time.Hour).Unix(),
	})

	allow, _, used := extenderActivationAllow(now, postsPerDualStackPass, extenderTestAccountA)
	if !allow || used != 1 {
		t.Fatalf("first start after the upgrade: allow=%v used=%d, want allow=true used=1", allow, used)
	}
	record, err := readExtenderActivationRecord(extenderTestAccountA)
	if err != nil {
		t.Fatalf("record unreadable after the start: %s", err)
	}
	state, ok := record.Accounts[extenderTestAccountA]
	if !ok {
		t.Fatalf("record %+v has no entry for the account signed in now", record)
	}
	if len(state.Attempts) != 3 {
		t.Fatalf("attempts=%v, want the legacy post plus this start's two", state.Attempts)
	}
	if state.Refusals != 1 || state.LastRefusalUnix == 0 {
		t.Fatalf("refusal streak = %+v, want the legacy streak kept", state)
	}
	if record.extenderActivationLegacy.present() {
		t.Fatalf("legacy shape still present after the migration: %+v", record.extenderActivationLegacy)
	}
}

// The budget is charged to the account the direct slot signs in as: the
// user_id claim of the sign-in token at <state>/jwt, which is the key the
// operator's per-user rate limit counts.
func TestExtenderActivationUserIdReadsTheAccountToken(t *testing.T) {
	testExtenderState(t)
	if got := extenderActivationUserId(); got != "" {
		t.Fatalf("user id before sign-in = %q, want empty", got)
	}

	writeAccountToken(t, extenderTestAccountA)
	if got := extenderActivationUserId(); got != extenderTestAccountA {
		t.Fatalf("user id = %q, want the token's %q", got, extenderTestAccountA)
	}

	// A healed client identity does not change the account the budget charges.
	writeClientCredential(t, extenderTestClientB)
	if got := extenderActivationUserId(); got != extenderTestAccountA {
		t.Fatalf("user id after a client heal = %q, want %q", got, extenderTestAccountA)
	}

	// A malformed token names no account, so the start charges the
	// unidentified key rather than a guessed one.
	path, err := providerStatePath("jwt")
	if err != nil {
		t.Fatalf("account token path: %s", err)
	}
	if err := os.WriteFile(path, []byte("not-a-token"), 0o600); err != nil {
		t.Fatalf("write the malformed token: %s", err)
	}
	if got := extenderActivationUserId(); got != "" {
		t.Fatalf("user id from a malformed token = %q, want empty", got)
	}
}

// A store that cannot be opened must not hand out the role: an activation whose
// posts nothing records is the burst a supervisor restart loop repeats, so the
// start is held and the operator gets one line saying why, not one per attempt
// and not silence.
func TestExtenderActivationDeniesAndWarnsOnceWithoutAStore(t *testing.T) {
	// The state directory cannot be created when its parent is a regular file,
	// so the store stays unavailable for the whole test.
	parent := filepath.Join(t.TempDir(), "state-parent")
	if err := os.WriteFile(parent, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed the unusable state directory: %s", err)
	}
	t.Setenv("URNETWORK_STATE_DIR", filepath.Join(parent, "state"))
	closeProviderStore()

	previousWriter := proxyManagerLogWriter
	writer := &bytes.Buffer{}
	proxyManagerLogWriter = writer
	t.Cleanup(func() {
		proxyManagerLogWriter = previousWriter
		closeProviderStore()
	})

	now := time.Unix(1_800_000_000, 0)
	for i := 0; i < 3; i++ {
		allow, wait, used := extenderActivationAllow(now.Add(time.Duration(i)*time.Minute), postsPerDualStackPass, extenderTestAccountA)
		if allow || used != 0 {
			t.Fatalf("start %d with no store: allow=%v used=%d, want allow=false used=0", i, allow, used)
		}
		if wait != providerStoreRetryInterval {
			t.Fatalf("start %d with no store: wait=%s, want %s", i, wait, providerStoreRetryInterval)
		}
	}
	want := "activation budget unavailable for account " + extenderTestAccountA
	if lines := strings.Count(writer.String(), want); lines != 3 {
		t.Fatalf("unavailable-budget lines=%d, want 3 (log: %q)", lines, writer.String())
	}
}

// A refusal is persisted with its doubling wait for the account that was
// refused, so the next start honours it instead of posting into the same
// refusal.
func TestExtenderActivationRefusalPersistsAndDoubles(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)

	if wait := recordExtenderActivationRefused(now, extenderTestAccountA); wait != extenderRefusalBackoff {
		t.Fatalf("first refusal wait=%s, want %s", wait, extenderRefusalBackoff)
	}
	allow, wait, _ := extenderActivationAllow(now.Add(time.Minute), postsPerDualStackPass, extenderTestAccountA)
	if allow {
		t.Fatal("a start inside the refusal backoff must be held")
	}
	if want := extenderRefusalBackoff - time.Minute; wait != want {
		t.Fatalf("held wait=%s, want %s", wait, want)
	}
	// A different account is not inside that backoff.
	if allow, _, used := extenderActivationAllow(now.Add(time.Minute), postsPerDualStackPass, extenderTestAccountB); !allow || used != 0 {
		t.Fatalf("other account during the backoff: allow=%v used=%d, want allow=true used=0", allow, used)
	}
	// A second refusal doubles the hold, and the cap holds it at the maximum.
	now = now.Add(extenderRefusalBackoff)
	if wait := recordExtenderActivationRefused(now, extenderTestAccountA); wait != 2*extenderRefusalBackoff {
		t.Fatalf("second refusal wait=%s, want %s", wait, 2*extenderRefusalBackoff)
	}
	for i := 0; i < 20; i++ {
		now = now.Add(extenderRefusalBackoffMax)
		_ = recordExtenderActivationRefused(now, extenderTestAccountA)
	}
	record, _ := readExtenderActivationRecord(extenderTestAccountA)
	state := record.Accounts[extenderTestAccountA]
	if state.Refusals != 22 {
		t.Fatalf("refusals=%d, want 22", state.Refusals)
	}
	if got := time.Duration(state.BackoffUntilUnix-now.Unix()) * time.Second; extenderRefusalBackoffMax < got {
		t.Fatalf("backoff=%s exceeds the %s cap", got, extenderRefusalBackoffMax)
	}
}

// A signed activation clears the refusal that held the account, and the
// account's spending in the operator's window stays: the operator records every
// allowed attempt and only ages them out, so refunding them here would tell the
// next gate it has a budget the operator does not agree with. Another account's
// record is not touched at all.
func TestExtenderActivationSuccessClearsOnlyThatAccount(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	extenderActivationAllow(now, postsPerDualStackPass, extenderTestAccountA)
	recordExtenderActivationRefused(now, extenderTestAccountA)
	extenderActivationAllow(now, postsPerDualStackPass, extenderTestAccountB)

	recordExtenderActivationSucceeded(extenderTestAccountA)

	record, _ := readExtenderActivationRecord(extenderTestAccountA)
	state, ok := record.Accounts[extenderTestAccountA]
	if !ok {
		t.Fatalf("a signed activation dropped account A's record, want its attempts kept")
	}
	if state.Refusals != 0 || state.BackoffUntilUnix != 0 || state.LastRefusalUnix != 0 {
		t.Fatalf("account A kept refusals=%d backoff=%d last=%d after a signed activation, want them cleared",
			state.Refusals, state.BackoffUntilUnix, state.LastRefusalUnix)
	}
	if len(state.Attempts) == 0 {
		t.Fatalf("account A lost the attempts it spent; the window is the operator's, not ours to refund")
	}
	stateB, ok := record.Accounts[extenderTestAccountB]
	if !ok || len(stateB.Attempts) != postsPerDualStackPass {
		t.Fatalf("account B's record = %+v, want its own posts kept", stateB)
	}
}

// A start that charged attempts before the sign-in token was readable leaves
// them on the unidentified key, and the activation the operator signs after the
// token resolves accepts those posts. The success must drop them with the
// account's own state, or the next gate check adopts them back and holds a
// launch the success was meant to clear.
func TestExtenderActivationSuccessKeepsTheUnidentifiedPosts(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)

	// Two starts post while the token is unreadable, which spends four of the
	// five posts with no account to charge.
	for start := 0; start < 2; start++ {
		if allow, wait, _ := extenderActivationAllow(now, postsPerDualStackPass, ""); !allow {
			t.Fatalf("start %d before the token resolved was refused (wait=%s)", start+1, wait)
		}
	}

	// The token resolves and the operator signs the activation before the next
	// gate check.
	recordExtenderActivationSucceeded(extenderTestAccountA)

	allow, wait, used := extenderActivationAllow(now, postsPerDualStackPass, extenderTestAccountA)
	if allow || used != 4 {
		t.Fatalf("after a signed activation: allow=%v used=%d wait=%s, want allow=false used=4",
			allow, used, wait)
	}
	record, _ := readExtenderActivationRecord(extenderTestAccountA)
	if _, ok := record.Accounts[extenderActivationUnidentifiedAccount]; ok {
		t.Fatalf("unidentified entry was not adopted: %+v", record.Accounts)
	}
	if state := record.Accounts[extenderTestAccountA]; len(state.Attempts) != 4 {
		t.Fatalf("account attempts = %d, want 4 adopted posts", len(state.Attempts))
	}
}

// A refusal taken before the token resolved is adopted by the account the
// signed activation names. The success must clear that adopted hold too: it
// clears the account's refusal and backoff, and adopting after the clear would
// put the hold back, so the next gate check refuses a launch the activation
// just succeeded.
func TestExtenderActivationSuccessClearsAdoptedBackoff(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	recordExtenderActivationRefused(now, "")

	recordExtenderActivationSucceeded(extenderTestAccountA)

	record, _ := readExtenderActivationRecord(extenderTestAccountA)
	state := record.Accounts[extenderTestAccountA]
	if state.BackoffUntilUnix != 0 || state.Refusals != 0 {
		t.Fatalf("after a signed activation the adopted hold is still live: backoff=%d refusals=%d, want both cleared",
			state.BackoffUntilUnix, state.Refusals)
	}
}

// A record this build cannot decode is reported with the decode error itself.
// The warning is the only evidence the operator gets while the budget is held,
// so a line that says the store is unavailable would send them looking at the
// wrong thing.
func TestExtenderActivationWarningNamesTheStoreError(t *testing.T) {
	testExtenderState(t)
	logs := captureGateLogs(t)
	if err := providerStoreInstance().save(providerStoreBucketState, providerStoreKeyExtenderActivation, "not a record"); err != nil {
		t.Fatal(err)
	}

	_, err := readExtenderActivationRecord(extenderTestAccountA)
	if err == nil {
		t.Fatal("an undecodable record read without error")
	}
	if !strings.Contains(logs.String(), err.Error()) {
		t.Fatalf("warning does not name the store error %q:\n%s", err, logs.String())
	}
}

// The store keeps a record across a close and reopen, and a missing record
// reads as absent rather than as an error.
func TestProviderStoreRoundTripsARecord(t *testing.T) {
	testExtenderState(t)
	type record struct {
		Count int    `json:"count"`
		Note  string `json:"note"`
	}
	if err := providerStoreInstance().save(providerStoreBucketState, "probe", record{Count: 7, Note: "kept"}); err != nil {
		t.Fatalf("save: %s", err)
	}
	closeProviderStore()

	var got record
	found, err := providerStoreInstance().load(providerStoreBucketState, "probe", &got)
	if err != nil {
		t.Fatalf("load after a reopen: %s", err)
	}
	if !found {
		t.Fatal("record missing after a reopen")
	}
	if got.Count != 7 || got.Note != "kept" {
		t.Fatalf("record = %+v, want {7 kept}", got)
	}
	var missing record
	found, err = providerStoreInstance().load(providerStoreBucketState, "absent", &missing)
	if err != nil {
		t.Fatalf("load of an absent record: %s", err)
	}
	if found {
		t.Fatal("an absent record must report found=false")
	}
}

// A start before the sign-in token is readable charges the unidentified key.
// The first start that resolves the account inherits those posts: the operator
// counted them when they were made, so the account must not open a fresh
// window, and the unidentified entry is cleared so nothing is charged twice.
func TestExtenderActivationBudgetAdoptsTheUnidentifiedPosts(t *testing.T) {
	testExtenderState(t)
	gate := newExtenderActivationGate()

	if !gate.decideAtStart() {
		t.Fatal("the start before the token is readable must be allowed")
	}
	// The host signs in; the first launch spends the startup verdict, so the
	// next launch is the first Allow to resolve the account and see what the
	// earlier start already spent.
	writeAccountToken(t, extenderTestAccountA)
	if !gate.allowLaunch() {
		t.Fatal("the launch that spends the startup verdict must be allowed")
	}
	if !gate.allowLaunch() {
		t.Fatal("the first launch after sign-in must be allowed")
	}
	posts := hostActivationPosts()
	account := extenderActivationUserId()
	record, _ := readExtenderActivationRecord(account)
	state := record.Accounts[account]
	if len(state.Attempts) != 2*posts {
		t.Fatalf("account attempts=%d, want %d: the posts made before the token resolved were not adopted",
			len(state.Attempts), 2*posts)
	}
	if _, ok := record.Accounts[extenderActivationUnidentifiedAccount]; ok {
		t.Fatalf("the unidentified entry survived adoption: %+v", record.Accounts)
	}
}

// A refusal the host took while it was still unnamed is the same incident
// after it signs in: the account inherits the streak, so once that hold
// expires its next refusal keeps doubling instead of starting over at the
// first wait.
func TestExtenderActivationAdoptionKeepsTheRefusalStreak(t *testing.T) {
	testExtenderState(t)
	refusedAt := time.Unix(1_800_000_000, 0)
	if err := providerStoreInstance().save(providerStoreBucketState, providerStoreKeyExtenderActivation, extenderActivationRecord{
		Accounts: map[string]extenderActivationState{
			extenderActivationUnidentifiedAccount: {
				Refusals:         2,
				LastRefusalUnix:  refusedAt.Unix(),
				BackoffUntilUnix: refusedAt.Add(2 * extenderRefusalBackoff).Unix(),
			},
		},
	}); err != nil {
		t.Fatalf("seed the unidentified refusal: %s", err)
	}

	// The hold the second refusal set has expired, so the start is allowed
	// and this is where the account inherits the record.
	afterHold := refusedAt.Add(2*extenderRefusalBackoff + time.Minute)
	if allow, wait, _ := extenderActivationAllow(afterHold, postsPerDualStackPass, extenderTestAccountA); !allow {
		t.Fatalf("the start after the adopted hold expired was refused (wait=%s)", wait)
	}
	record, _ := readExtenderActivationRecord(extenderTestAccountA)
	if state := record.Accounts[extenderTestAccountA]; state.Refusals != 2 || state.LastRefusalUnix != refusedAt.Unix() {
		t.Fatalf("adopted state = %+v, want the streak of 2 from %d: the refusal count was dropped",
			state, refusedAt.Unix())
	}

	// The third refusal of the streak waits longer than the first would.
	if wait := recordExtenderActivationRefused(afterHold, extenderTestAccountA); wait <= 2*extenderRefusalBackoff {
		t.Fatalf("wait after adopting a streak of 2 = %s, want more than %s: the streak restarted",
			wait, 2*extenderRefusalBackoff)
	}
}

// A streak whose last refusal is older than the decay is over, so adoption must
// not revive it with a fresher timestamp taken from the other entry: the count
// and the time it was charged belong together. An account carrying five stale
// refusals that adopts a live streak of one refuses next at the second wait,
// not the sixth.
func TestExtenderActivationAdoptionDropsADecayedStreak(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	freshRefusal := now.Add(-time.Minute)
	if err := providerStoreInstance().save(providerStoreBucketState, providerStoreKeyExtenderActivation, extenderActivationRecord{
		Accounts: map[string]extenderActivationState{
			extenderTestAccountA: {
				Refusals:        5,
				LastRefusalUnix: now.Add(-extenderRefusalDecay - time.Minute).Unix(),
			},
			extenderActivationUnidentifiedAccount: {
				Refusals:        1,
				LastRefusalUnix: freshRefusal.Unix(),
			},
		},
	}); err != nil {
		t.Fatalf("seed the stale and the live streak: %s", err)
	}

	if allow, wait, _ := extenderActivationAllow(now, postsPerDualStackPass, extenderTestAccountA); !allow {
		t.Fatalf("the start after both holds expired was refused (wait=%s)", wait)
	}
	record, _ := readExtenderActivationRecord(extenderTestAccountA)
	state := record.Accounts[extenderTestAccountA]
	if state.Refusals != 1 || state.LastRefusalUnix != freshRefusal.Unix() {
		t.Fatalf("adopted state = %+v, want the live streak of 1 from %s: a stale count rode a fresh timestamp",
			state, freshRefusal)
	}

	wait := recordExtenderActivationRefused(now, extenderTestAccountA)
	if wait != 2*extenderRefusalBackoff {
		t.Fatalf("wait after adopting the streak = %s, want %s: the next refusal took the sixth wait",
			wait, 2*extenderRefusalBackoff)
	}
}

// An attempt older than the window has already left the operator's own limit,
// so adoption keeps only the posts still counted.
func TestExtenderActivationAdoptionDropsAttemptsOutsideTheWindow(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	if err := providerStoreInstance().save(providerStoreBucketState, providerStoreKeyExtenderActivation, extenderActivationRecord{
		Accounts: map[string]extenderActivationState{
			extenderActivationUnidentifiedAccount: {
				Attempts: []int64{
					now.Add(-extenderActivationWindow - time.Minute).Unix(),
					now.Add(-time.Minute).Unix(),
				},
			},
		},
	}); err != nil {
		t.Fatalf("seed the unidentified record: %s", err)
	}
	allow, _, used := extenderActivationAllow(now, postsPerDualStackPass, extenderTestAccountA)
	if !allow || used != 1 {
		t.Fatalf("adopted start: allow=%v used=%d, want allow=true used=1", allow, used)
	}
}

// A refusal recorded while the account was still unnamed holds that account
// once the token resolves: the hold was the operator answering this host, and
// it is still standing, so a fresh window cannot post into it.
func TestExtenderActivationRefusalBeforeSignInHoldsTheAccount(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)

	recordExtenderActivationRefused(now, "")
	if allow, wait, _ := extenderActivationAllow(now.Add(time.Minute), postsPerDualStackPass, extenderTestAccountA); allow {
		t.Fatalf("the account was allowed inside the refusal hold recorded before sign-in (wait=%s)", wait)
	}
}

// The record is one value, so two slots' listeners reporting at once must not
// lose one another's write: every account that reported is in the record.
func TestExtenderActivationBudgetKeepsEveryConcurrentReport(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	const accounts = 24

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < accounts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recordExtenderActivationRefused(now, extenderTestAccount(i))
		}(i)
	}
	close(start)
	wg.Wait()

	record, _ := readExtenderActivationRecord(extenderTestAccount(0))
	if len(record.Accounts) != accounts {
		t.Fatalf("accounts in the record = %d, want %d: a concurrent report was lost", len(record.Accounts), accounts)
	}
}

// The direct slot decides once per launch. The launch that follows the startup
// decision spends that verdict instead of charging the same posts twice, and a
// relaunch asks the record again, so a hold recorded since the process started
// is honoured and one it outlived is not.
func TestExtenderActivationGateRechecksOnRelaunch(t *testing.T) {
	testExtenderState(t)
	writeAccountToken(t, extenderTestAccountA)
	writeHostCapability(hostCapability{IPv6Usable: true, CheckedAtUnix: time.Now().Unix()})
	gate := newExtenderActivationGate()

	if !gate.decideAtStart() {
		t.Fatal("a start with a fresh budget must be allowed")
	}
	if !gate.allowLaunch() {
		t.Fatal("the launch that spends the startup verdict must be allowed")
	}
	accountId := extenderActivationUserId()
	if accountId != extenderTestAccountA {
		t.Fatalf("account id = %q, want %q: the test must charge a signed-in account, not the unidentified fallback",
			accountId, extenderTestAccountA)
	}
	record, _ := readExtenderActivationRecord(accountId)
	state := record.Accounts[extenderActivationAccountKey(accountId)]
	if len(state.Attempts) != postsPerDualStackPass {
		t.Fatalf("attempts=%d after one start, want %d: the startup posts were charged twice",
			len(state.Attempts), postsPerDualStackPass)
	}
	if _, ok := record.Accounts[extenderActivationUnidentifiedAccount]; ok {
		t.Fatalf("the start was charged to the unidentified key: %+v", record.Accounts)
	}

	// The operator refuses the activation, so the listener records a hold. A
	// relaunch inside it must not post again.
	recordExtenderActivationRefused(time.Now(), accountId)
	if gate.allowLaunch() {
		t.Fatal("a relaunch inside the recorded hold must not run the role")
	}
}

// A refusal the activation callback records must hold the account's next
// start, and the activation the operator signs must clear it: the listener and
// the gate have to key the record the same way.
func TestExtenderActivationListenerRefusalHoldsTheAccount(t *testing.T) {
	testExtenderState(t)
	writeAccountToken(t, extenderTestAccountA)
	account := extenderActivationUserId()

	listener := newProviderExtenderStatusListener(nil, 0, account)
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, Listening: true, LastActivationError: "refused", LastActivationRefused: true})
	now := time.Now()
	if allow, wait, _ := extenderActivationAllow(now, postsPerDualStackPass, account); allow {
		t.Fatalf("a start inside the refusal the listener recorded was allowed (wait=%s): the listener did not key the account", wait)
	}

	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, Listening: true, ActivatedV4: true})
	// The refused post was already posted, so the operator counted it. A signed
	// activation clears the hold; it does not refund the attempt.
	if allow, _, used := extenderActivationAllow(now, postsPerDualStackPass, account); !allow || used != 1 {
		t.Fatalf("after a signed activation: allow=%v used=%d, want allow=true used=1 (the refused post is not refunded)", allow, used)
	}
}

func TestExtenderActivationListenerPinsResolvedAccount(t *testing.T) {
	testExtenderState(t)
	writeAccountToken(t, extenderTestAccountA)
	listener := newProviderExtenderStatusListener(nil, 0, "")
	status := &sdk.ExtenderProvideStatus{Enabled: true, LastActivationError: "refused", LastActivationRefused: true}
	listener.ExtenderProvideStatusChanged(status)
	writeAccountToken(t, extenderTestAccountB)
	status.LastActivationTime++
	listener.ExtenderProvideStatusChanged(status)
	record, err := readExtenderActivationRecord(extenderTestAccountA)
	if err != nil || record.Accounts[extenderTestAccountA].Refusals != 2 {
		t.Fatalf("pinned account A record=%+v err=%v", record.Accounts, err)
	}
	record, err = readExtenderActivationRecord(extenderTestAccountB)
	if err != nil || record.Accounts[extenderTestAccountB].Refusals != 0 {
		t.Fatalf("changed token account B record=%+v err=%v", record.Accounts, err)
	}
}

// A refusal belongs to the activation attempt that produced it. The status
// carries the SDK's last activation time, so two attempts with the same
// booleans are still two refusals and the wait must escalate; a standing
// error beside another changed fact is the same attempt and is charged once.
func TestExtenderActivationRefusalsEscalateAcrossAttempts(t *testing.T) {
	testExtenderState(t)
	const account = extenderTestAccountA
	listener := newProviderExtenderStatusListener(nil, 0, account)
	first := &sdk.ExtenderProvideStatus{
		Enabled:               true,
		Listening:             true,
		LastActivationTime:    1_800_000_000_000,
		LastActivationError:   "refused",
		LastActivationRefused: true,
	}

	listener.ExtenderProvideStatusChanged(first)
	record, _ := readExtenderActivationRecord(account)
	state := record.Accounts[account]
	if state.Refusals != 1 {
		t.Fatalf("refusals after the first attempt = %d, want 1", state.Refusals)
	}
	firstHold := time.Duration(state.BackoffUntilUnix-time.Now().Unix()) * time.Second

	// The operator refuses the next attempt: every captured boolean is the
	// same, and only the attempt time moved.
	second := *first
	second.LastActivationTime += 1
	listener.ExtenderProvideStatusChanged(&second)
	record, _ = readExtenderActivationRecord(account)
	state = record.Accounts[account]
	if state.Refusals != 2 {
		t.Fatalf("refusals after a second attempt = %d, want 2: the attempt was deduplicated by its booleans", state.Refusals)
	}
	secondHold := time.Duration(state.BackoffUntilUnix-time.Now().Unix()) * time.Second
	if secondHold <= firstHold {
		t.Fatalf("hold after the second refusal = %s, want more than the first %s", secondHold, firstHold)
	}
}

// Re-delivering one attempt, even with a changed fact beside the standing
// error, must not charge the account a second refusal: the refusal is the
// attempt's, and this attempt is already recorded.
func TestExtenderActivationStandingRefusalIsChargedOnce(t *testing.T) {
	testExtenderState(t)
	const account = extenderTestAccountA
	listener := newProviderExtenderStatusListener(nil, 0, account)
	status := &sdk.ExtenderProvideStatus{
		Enabled:               true,
		Listening:             true,
		LastActivationTime:    1_800_000_000_000,
		LastActivationError:   "refused",
		LastActivationRefused: true,
	}

	listener.ExtenderProvideStatusChanged(status)
	listener.ExtenderProvideStatusChanged(status)
	redelivered := *status
	redelivered.Listening = false
	listener.ExtenderProvideStatusChanged(&redelivered)

	record, _ := readExtenderActivationRecord(account)
	state := record.Accounts[account]
	if state.Refusals != 1 {
		t.Fatalf("refusals = %d after re-delivering one attempt, want 1: a standing error was charged again", state.Refusals)
	}
	hold := time.Duration(state.BackoffUntilUnix-time.Now().Unix()) * time.Second
	if extenderRefusalBackoff < hold {
		t.Fatalf("hold = %s, want the first refusal's %s: the streak escalated without a new attempt", hold, extenderRefusalBackoff)
	}
}

// After the parent hands provider_state.db to a hotswap candidate it must not
// take the file back, and a write it still had queued must report the loss
// instead of returning nil.
func TestProviderStoreReleaseRefusesAReopenAndReportsLostWrites(t *testing.T) {
	testExtenderState(t)
	parent := providerStoreInstance()
	if parent == nil {
		t.Fatal("the parent's store did not open")
	}
	if err := parent.save(providerStoreBucketState, "probe", map[string]int{"count": 1}); err != nil {
		t.Fatalf("seed: %s", err)
	}

	releaseProviderStore()

	if err := parent.save(providerStoreBucketState, "probe", map[string]int{"count": 2}); !errors.Is(err, errProviderStoreReleased) {
		t.Fatalf("save after the release = %v, want %v: the lost write is silent", err, errProviderStoreReleased)
	}
	// The candidate owns the file now, so the parent's next lookup refuses
	// rather than reopening it and retaking the bbolt lock.
	if store, err := openProviderStoreForProcess(); store != nil || !errors.Is(err, errProviderStoreReleased) {
		t.Fatalf("open after the release = (%v, %v), want (nil, %v)", store, err, errProviderStoreReleased)
	}
	if store := providerStoreInstance(); store != nil {
		t.Fatal("the parent reopened the store the candidate now owns")
	}
}

// A refusal the store could not take must not be forgotten. The attempt is
// marked handled before the write, so when the write fails and the store comes
// back, the next observation of the same attempt has to charge it.
func TestExtenderActivationRefusalSurvivesATransientStoreFailure(t *testing.T) {
	testExtenderState(t)
	const account = extenderTestAccountA
	if providerStoreInstance() == nil {
		t.Fatal("the store did not open")
	}
	listener := newProviderExtenderStatusListener(nil, 0, account)
	status := &sdk.ExtenderProvideStatus{
		Enabled:               true,
		Listening:             true,
		LastActivationTime:    1_800_000_000_000,
		LastActivationError:   "refused",
		LastActivationRefused: true,
	}

	releaseProviderStore()
	listener.ExtenderProvideStatusChanged(status)
	if err := restoreProviderStoreAfterAbortedHandoff(); err != nil {
		t.Fatalf("the store did not come back: %s", err)
	}

	redelivered := *status
	redelivered.Listening = false
	listener.ExtenderProvideStatusChanged(&redelivered)

	record, err := readExtenderActivationRecord(account)
	if err != nil {
		t.Fatalf("read the record: %s", err)
	}
	if got := record.Accounts[account].Refusals; got != 1 {
		t.Fatalf("refusals = %d after the store recovered, want 1: the refusal was lost with the failed write", got)
	}
}

// A database that had to be moved aside may have held posts or a refusal that
// nobody can read any more. The fresh store must not open a full window for the
// next start: the operator still counts what the unreadable file recorded.
func TestExtenderActivationHoldsAfterTheStoreIsQuarantined(t *testing.T) {
	testExtenderState(t)
	closeProviderStore()
	t.Cleanup(closeProviderStore)
	path, err := providerStatePath(providerStoreFileName)
	if err != nil {
		t.Fatalf("store path: %s", err)
	}
	if err := os.WriteFile(path, []byte("this is not a bbolt database"), 0o600); err != nil {
		t.Fatalf("write the damaged file: %s", err)
	}

	now := time.Now()
	allow, wait, _ := extenderActivationAllow(now, postsPerDualStackPass, extenderTestAccountA)
	if allow {
		t.Fatal("the first start after a quarantine was allowed: the budget reset with the file")
	}
	if wait < extenderActivationWindow-time.Minute {
		t.Fatalf("hold = %s, want about one window (%s)", wait, extenderActivationWindow)
	}
	if allow, _, _ := extenderActivationAllow(now.Add(extenderActivationWindow+time.Minute), postsPerDualStackPass, extenderTestAccountA); !allow {
		t.Fatal("a start after the window was still held")
	}
}
