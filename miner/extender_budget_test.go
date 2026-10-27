package miner

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
)

// postsPerDualStackPass is the charge for a host that can post on both
// families, which is what the budget tests exercise.
const postsPerDualStackPass = 2

// The client ids the budget tests charge. The record is keyed by whatever the
// direct slot provides under, so a test name is enough to stand for one.
const (
	extenderTestClientA = "client-a"
	extenderTestClientB = "client-b"
)

// testExtenderState gives each test its own state directory and its own store
// handle, and closes the handle afterwards so the next test starts clean.
func testExtenderState(t *testing.T) {
	t.Helper()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	closeProviderStore()
	t.Cleanup(closeProviderStore)
}

// extenderTestClient names the nth client of a test that churns identities.
func extenderTestClient(n int) string {
	return fmt.Sprintf("client-%02d", n)
}

// extenderStoreAsSecondHost drops the process-wide handle without moving the
// state directory: the next call opens the same record, the way a second
// process on the same state directory would.
func extenderStoreAsSecondHost() {
	closeProviderStore()
}

// writeLegacyExtenderActivation seeds the pre-client shape, which is what a
// store written by an older build holds.
func writeLegacyExtenderActivation(t *testing.T, legacy extenderActivationLegacy) {
	t.Helper()
	if err := providerStoreInstance().save(providerStoreBucketState, providerStoreKeyExtenderActivation, legacy); err != nil {
		t.Fatalf("seed the legacy record: %s", err)
	}
}

// The operator allows six activation posts per client per hour. A start spends
// two, and the budget is persisted, so a restart cannot spend them again.
func TestExtenderActivationBudgetHoldsTheThirdStart(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)

	if allow, _, used := extenderActivationAllow(now, postsPerDualStackPass, extenderTestClientA); !allow || used != 0 {
		t.Fatalf("first start: allow=%v used=%d, want allow=true used=0", allow, used)
	}
	if allow, _, used := extenderActivationAllow(now.Add(time.Minute), postsPerDualStackPass, extenderTestClientA); !allow || used != 2 {
		t.Fatalf("second start: allow=%v used=%d, want allow=true used=2", allow, used)
	}
	// Four posts are counted; a third start would post six and hit the
	// operator's cap, so it is held until the first post ages out.
	allow, wait, used := extenderActivationAllow(now.Add(2*time.Minute), postsPerDualStackPass, extenderTestClientA)
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
	if allow, _, _ := extenderActivationAllow(now.Add(3*time.Minute), postsPerDualStackPass, extenderTestClientA); allow {
		t.Fatal("a held start must not open the budget")
	}
	if allow, _, used := extenderActivationAllow(now.Add(extenderActivationWindow+2*time.Minute), postsPerDualStackPass, extenderTestClientA); !allow || used != 0 {
		t.Fatalf("after the window: allow=%v used=%d, want allow=true used=0", allow, used)
	}
}

// The budget survives a restart: a new process opens the same file and reads it.
func TestExtenderActivationBudgetSurvivesARestart(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	extenderActivationAllow(now, postsPerDualStackPass, extenderTestClientA)
	extenderActivationAllow(now.Add(time.Minute), postsPerDualStackPass, extenderTestClientA)

	// A restart drops the process-wide handle and opens the file again.
	extenderStoreAsSecondHost()
	allow, _, used := extenderActivationAllow(now.Add(2*time.Minute), postsPerDualStackPass, extenderTestClientA)
	if allow || used != 4 {
		t.Fatalf("after a restart: allow=%v used=%d, want allow=false used=4", allow, used)
	}
}

// A client that has spent its window must not block a different client. The
// identity a healed slot replaced is not the identity it provides under now,
// and the server charges the new client separately.
func TestExtenderActivationBudgetIsPerClient(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)

	extenderActivationAllow(now, postsPerDualStackPass, extenderTestClientA)
	extenderActivationAllow(now.Add(time.Minute), postsPerDualStackPass, extenderTestClientA)
	if allow, _, used := extenderActivationAllow(now.Add(2*time.Minute), postsPerDualStackPass, extenderTestClientA); allow {
		t.Fatalf("client A: third start allowed with used=%d, want held", used)
	}
	if allow, _, used := extenderActivationAllow(now.Add(2*time.Minute), postsPerDualStackPass, extenderTestClientB); !allow || used != 0 {
		t.Fatalf("client B: allow=%v used=%d, want allow=true used=0", allow, used)
	}
}

// Two hosts that provide under one client id are one client to the operator:
// the second host is held by what the first spent, so together they cannot
// spend more than the client's six.
func TestExtenderActivationBudgetIsSharedByOneClientAcrossHosts(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	const shared = "shared-client"

	extenderActivationAllow(now, postsPerDualStackPass, shared)
	extenderActivationAllow(now.Add(time.Minute), postsPerDualStackPass, shared)

	// The second host opens the same record and starts under the same client.
	extenderStoreAsSecondHost()
	allow, _, used := extenderActivationAllow(now.Add(2*time.Minute), postsPerDualStackPass, shared)
	if allow || used != 4 {
		t.Fatalf("second host on the shared client: allow=%v used=%d, want allow=false used=4", allow, used)
	}
	// A sibling host that healed to a new client is a different client at the
	// server, so the shared client's spending does not hold it.
	if allow, _, used := extenderActivationAllow(now.Add(2*time.Minute), postsPerDualStackPass, extenderTestClientA); !allow || used != 0 {
		t.Fatalf("sibling host on a new client: allow=%v used=%d, want allow=true used=0", allow, used)
	}
}

// A record written before the budget was keyed by client belongs to the client
// providing now: the upgrade keeps the identity, so its attempts and its
// refusal streak are that client's, and they are rewritten in the new shape.
func TestExtenderActivationBudgetMigratesTheFlatRecord(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	writeLegacyExtenderActivation(t, extenderActivationLegacy{
		Attempts:        []int64{now.Add(-30 * time.Minute).Unix()},
		Refusals:        1,
		LastRefusalUnix: now.Add(-time.Hour).Unix(),
	})

	allow, _, used := extenderActivationAllow(now, postsPerDualStackPass, extenderTestClientA)
	if !allow || used != 1 {
		t.Fatalf("first start after the upgrade: allow=%v used=%d, want allow=true used=1", allow, used)
	}
	record, found := readExtenderActivationRecord(extenderTestClientA)
	if !found {
		t.Fatal("record missing after the start")
	}
	state, ok := record.Clients[extenderTestClientA]
	if !ok {
		t.Fatalf("record %+v has no entry for the client providing now", record)
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

// The record keeps the most recent 32 clients and drops the least recently
// used, so a host that churns identities cannot grow it without limit.
func TestExtenderActivationBudgetClientMapIsBounded(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	first := extenderTestClient(0)
	for i := 0; i < extenderActivationClientBound+1; i++ {
		if allow, _, _ := extenderActivationAllow(now.Add(time.Duration(i)*time.Second), postsPerDualStackPass, extenderTestClient(i)); !allow {
			t.Fatalf("client %d: a fresh client was held", i)
		}
	}

	record, _ := readExtenderActivationRecord(first)
	if len(record.Clients) != extenderActivationClientBound {
		t.Fatalf("clients=%d, want %d", len(record.Clients), extenderActivationClientBound)
	}
	if _, ok := record.Clients[first]; ok {
		t.Fatal("the least recently used client survived the bound")
	}
	last := extenderTestClient(extenderActivationClientBound)
	if _, ok := record.Clients[last]; !ok {
		t.Fatalf("the most recently used client %s was evicted", last)
	}
}

// The budget is charged to the client the direct slot provides under, which is
// the client_id of its durable credential: the identity provideClientJWT
// reuses for that slot and rewrites when the identity is replaced.
func TestExtenderActivationClientIdReadsTheDirectSlotCredential(t *testing.T) {
	testExtenderState(t)
	if got := extenderActivationClientId(); got != "" {
		t.Fatalf("client id before registration = %q, want empty", got)
	}

	clientId := "6f1c2ad0-2e3f-4b8a-9d1c-0a5b7c9e1234"
	claims := gojwt.MapClaims{"client_id": clientId, "device_id": "0b7d3a1e-5c42-4f68-8a91-2d4e6f8a0b1c"}
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, claims).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign the test credential: %s", err)
	}
	path, err := providerClientJwtPath(providerSlotIdentities{}, nil)
	if err != nil {
		t.Fatalf("direct credential path: %s", err)
	}
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write the test credential: %s", err)
	}

	if got := extenderActivationClientId(); got != clientId {
		t.Fatalf("client id = %q, want the credential's %q", got, clientId)
	}
}

// A store that cannot be opened must not stop the role: the start is allowed,
// and the operator gets one line saying the budget went unenforced, not one per
// attempt and not silence.
func TestExtenderActivationBudgetUnenforcedIsLoggedOnce(t *testing.T) {
	// The state directory cannot be created when its parent is a regular file,
	// so the store stays unavailable for the whole test.
	parent := filepath.Join(t.TempDir(), "state-parent")
	if err := os.WriteFile(parent, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed the unusable state directory: %s", err)
	}
	t.Setenv("URNETWORK_STATE_DIR", filepath.Join(parent, "state"))
	closeProviderStore()

	previousOnce := extenderBudgetWarnOnce
	extenderBudgetWarnOnce = new(sync.Once)
	previousWriter := proxyManagerLogWriter
	writer := &bytes.Buffer{}
	proxyManagerLogWriter = writer
	t.Cleanup(func() {
		extenderBudgetWarnOnce = previousOnce
		proxyManagerLogWriter = previousWriter
		closeProviderStore()
	})

	now := time.Unix(1_800_000_000, 0)
	for i := 0; i < 3; i++ {
		allow, _, used := extenderActivationAllow(now.Add(time.Duration(i)*time.Minute), postsPerDualStackPass, extenderTestClientA)
		if !allow || used != 0 {
			t.Fatalf("start %d with no store: allow=%v used=%d, want allow=true used=0", i, allow, used)
		}
	}
	want := "activation budget unenforced for client " + extenderTestClientA
	if lines := strings.Count(writer.String(), want); lines != 1 {
		t.Fatalf("unenforced-budget lines=%d, want exactly 1 (log: %q)", lines, writer.String())
	}
}

// A refusal is persisted with its doubling wait for the client that was
// refused, so the next start honours it instead of posting into the same
// refusal.
func TestExtenderActivationRefusalPersistsAndDoubles(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)

	if wait := recordExtenderActivationRefused(now, extenderTestClientA); wait != extenderRefusalBackoff {
		t.Fatalf("first refusal wait=%s, want %s", wait, extenderRefusalBackoff)
	}
	allow, wait, _ := extenderActivationAllow(now.Add(time.Minute), postsPerDualStackPass, extenderTestClientA)
	if allow {
		t.Fatal("a start inside the refusal backoff must be held")
	}
	if want := extenderRefusalBackoff - time.Minute; wait != want {
		t.Fatalf("held wait=%s, want %s", wait, want)
	}
	// A different client is not inside that backoff.
	if allow, _, used := extenderActivationAllow(now.Add(time.Minute), postsPerDualStackPass, extenderTestClientB); !allow || used != 0 {
		t.Fatalf("other client during the backoff: allow=%v used=%d, want allow=true used=0", allow, used)
	}
	// A second refusal doubles the hold, and the cap holds it at the maximum.
	now = now.Add(extenderRefusalBackoff)
	if wait := recordExtenderActivationRefused(now, extenderTestClientA); wait != 2*extenderRefusalBackoff {
		t.Fatalf("second refusal wait=%s, want %s", wait, 2*extenderRefusalBackoff)
	}
	for i := 0; i < 20; i++ {
		now = now.Add(extenderRefusalBackoffMax)
		_ = recordExtenderActivationRefused(now, extenderTestClientA)
	}
	record, _ := readExtenderActivationRecord(extenderTestClientA)
	state := record.Clients[extenderTestClientA]
	if state.Refusals != 22 {
		t.Fatalf("refusals=%d, want 22", state.Refusals)
	}
	if got := time.Duration(state.BackoffUntilUnix-now.Unix()) * time.Second; extenderRefusalBackoffMax < got {
		t.Fatalf("backoff=%s exceeds the %s cap", got, extenderRefusalBackoffMax)
	}
}

// A signed activation resets the budget of the client that activated: the
// operator's answer is what resets its own limit, not a clock. Another client's
// spending is not the operator's answer about this one.
func TestExtenderActivationSuccessClearsOnlyThatClient(t *testing.T) {
	testExtenderState(t)
	now := time.Unix(1_800_000_000, 0)
	extenderActivationAllow(now, postsPerDualStackPass, extenderTestClientA)
	recordExtenderActivationRefused(now, extenderTestClientA)
	extenderActivationAllow(now, postsPerDualStackPass, extenderTestClientB)

	recordExtenderActivationSucceeded(extenderTestClientA)

	record, _ := readExtenderActivationRecord(extenderTestClientA)
	if state, ok := record.Clients[extenderTestClientA]; ok {
		t.Fatalf("client A kept %+v after a signed activation, want cleared", state)
	}
	state, ok := record.Clients[extenderTestClientB]
	if !ok || len(state.Attempts) != postsPerDualStackPass {
		t.Fatalf("client B's record = %+v, want its own posts kept", state)
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
