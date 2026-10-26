package miner

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testExtenderState gives each test its own state directory and its own store
// handle, and closes the handle afterwards so the next test starts clean.
func testExtenderState(t *testing.T) {
	t.Helper()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	closeProviderStore()
	t.Cleanup(closeProviderStore)
}

// The operator allows six activation posts per client per hour. A start spends
// two, and the budget is persisted, so a restart cannot spend them again.
func TestExtenderActivationBudgetHoldsTheThirdStart(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)

	if allow, _, used := extenderActivationAllow(now, extenderActivationPostsPerPass); !allow || used != 0 {
		t.Fatalf("first start: allow=%v used=%d, want allow=true used=0", allow, used)
	}
	if allow, _, used := extenderActivationAllow(now.Add(time.Minute), extenderActivationPostsPerPass); !allow || used != 2 {
		t.Fatalf("second start: allow=%v used=%d, want allow=true used=2", allow, used)
	}
	// Four posts are counted; a third start would post six and hit the
	// operator's cap, so it is held until the first post ages out.
	allow, wait, used := extenderActivationAllow(now.Add(2*time.Minute), extenderActivationPostsPerPass)
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
	if allow, _, _ := extenderActivationAllow(now.Add(3*time.Minute), extenderActivationPostsPerPass); allow {
		t.Fatal("a held start must not open the budget")
	}
	if allow, _, used := extenderActivationAllow(now.Add(extenderActivationWindow+2*time.Minute), extenderActivationPostsPerPass); !allow || used != 0 {
		t.Fatalf("after the window: allow=%v used=%d, want allow=true used=0", allow, used)
	}
}

// The budget survives a restart: a new process opens the same file and reads it.
func TestExtenderActivationBudgetSurvivesARestart(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	extenderActivationAllow(now, extenderActivationPostsPerPass)
	extenderActivationAllow(now.Add(time.Minute), extenderActivationPostsPerPass)

	// A restart drops the process-wide handle and opens the file again.
	closeProviderStore()
	allow, _, used := extenderActivationAllow(now.Add(2*time.Minute), extenderActivationPostsPerPass)
	if allow || used != 4 {
		t.Fatalf("after a restart: allow=%v used=%d, want allow=false used=4", allow, used)
	}
}

// A refusal is persisted with its doubling wait, so the next start honours it
// instead of posting into the same refusal.
func TestExtenderActivationRefusalPersistsAndDoubles(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)

	if wait := recordExtenderActivationRefused(now); wait != extenderRefusalBackoff {
		t.Fatalf("first refusal wait=%s, want %s", wait, extenderRefusalBackoff)
	}
	allow, wait, _ := extenderActivationAllow(now.Add(time.Minute), extenderActivationPostsPerPass)
	if allow {
		t.Fatal("a start inside the refusal backoff must be held")
	}
	if want := extenderRefusalBackoff - time.Minute; wait != want {
		t.Fatalf("held wait=%s, want %s", wait, want)
	}
	// A second refusal doubles the hold, and the cap holds it at the maximum.
	now = now.Add(extenderRefusalBackoff)
	if wait := recordExtenderActivationRefused(now); wait != 2*extenderRefusalBackoff {
		t.Fatalf("second refusal wait=%s, want %s", wait, 2*extenderRefusalBackoff)
	}
	for i := 0; i < 20; i++ {
		now = now.Add(extenderRefusalBackoffMax)
		_ = recordExtenderActivationRefused(now)
	}
	state := readExtenderActivationState()
	if state.Refusals != 22 {
		t.Fatalf("refusals=%d, want 22", state.Refusals)
	}
	if got := time.Duration(state.BackoffUntilUnix-now.Unix()) * time.Second; extenderRefusalBackoffMax < got {
		t.Fatalf("backoff=%s exceeds the %s cap", got, extenderRefusalBackoffMax)
	}
}

// A signed activation resets the budget: the operator's answer is what resets
// its own limit, not a clock.
func TestExtenderActivationSuccessClearsTheBudget(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	recordExtenderActivationRefused(now)
	recordExtenderActivationSucceeded()
	state := readExtenderActivationState()
	if len(state.Attempts) != 0 || state.BackoffUntilUnix != 0 || state.Refusals != 0 {
		t.Fatalf("state after a success = %+v, want empty", state)
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
	if !providerStoreInstance().load(providerStoreBucketState, "probe", &got) {
		t.Fatal("record missing after a reopen")
	}
	if got.Count != 7 || got.Note != "kept" {
		t.Fatalf("record = %+v, want {7 kept}", got)
	}
	var missing record
	if providerStoreInstance().load(providerStoreBucketState, "absent", &missing) {
		t.Fatal("an absent record must report false")
	}
}

// The first start after an update imports the standalone auth streak and keeps
// the old file, renamed, as the only other copy.
func TestProviderStoreMigratesTheAuthBackoffFile(t *testing.T) {
	testExtenderState(t)
	dir, err := providerStateDir()
	if err != nil {
		t.Fatalf("state dir: %s", err)
	}
	legacy := filepath.Join(dir, authBackoffFileName)
	body := `{"consecutive_auth_failures":3,"first_failure_unix":1800000000,"last_failure_unix":1800000060}`
	if err := os.WriteFile(legacy, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %s", err)
	}
	closeProviderStore()
	store := providerStoreInstance()
	if store == nil {
		t.Fatal("store did not open")
	}
	var got authBackoffState
	if !store.load(providerStoreBucketState, providerStoreKeyAuthBackoff, &got) {
		t.Fatal("the legacy streak was not imported")
	}
	if got.ConsecutiveFailures != 3 {
		t.Fatalf("imported failures=%d, want 3", got.ConsecutiveFailures)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("the legacy file should be renamed, stat err=%v", err)
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Fatalf("the migrated copy is missing: %v", err)
	}
}
