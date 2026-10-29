package miner

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"go.etcd.io/bbolt"
)

func fallbackTestSetup(t *testing.T) (*bytes.Buffer, string) {
	t.Helper()
	closeProviderStore()
	t.Cleanup(closeProviderStore)
	t.Setenv("URNETWORK_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	previous := proxyManagerLogWriter
	writer := &bytes.Buffer{}
	proxyManagerLogWriter = writer
	t.Cleanup(func() { proxyManagerLogWriter = previous })
	extenderUncountedMu.Lock()
	extenderUncountedLaunches = 0
	extenderUncountedMu.Unlock()
	t.Cleanup(func() {
		extenderUncountedMu.Lock()
		extenderUncountedLaunches = 0
		extenderUncountedMu.Unlock()
	})
	path, err := extenderFallbackPath()
	if err != nil {
		t.Fatal(err)
	}
	return writer, path
}

func fallbackAttempts(t *testing.T, path, account string) int {
	t.Helper()
	return len(readFallbackTest(t, path).Accounts[account].Attempts)
}

func realAttempts(t *testing.T, account string) (int, extenderActivationState) {
	t.Helper()
	store := providerStoreInstance()
	if store == nil {
		t.Fatal("real store unavailable")
	}
	var record extenderActivationRecord
	if _, err := store.load(providerStoreBucketState, providerStoreKeyExtenderActivation, &record); err != nil {
		t.Fatal(err)
	}
	state := record.Accounts[account]
	return len(state.Attempts), state
}

// lockedStore holds provider_state.db the way a second process would.
func lockedStore(t *testing.T) *bbolt.DB {
	t.Helper()
	dir, _ := providerStateDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := bbolt.Open(filepath.Join(dir, providerStoreFileName), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func readFallbackTest(t *testing.T, path string) extenderFallbackRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record extenderFallbackRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestExtenderFallbackStartupLocked(t *testing.T) {
	log, path := fallbackTestSetup(t)
	locked := lockedStore(t)
	gate := newExtenderActivationGate()
	if gate.decideAtStart() {
		t.Fatal("startup should defer until first launch probe")
	}
	if !gate.allowLaunch() {
		t.Fatal("locked store must allow via fallback")
	}
	if got := fallbackAttempts(t, path, extenderActivationUnidentifiedAccount); got != hostActivationPosts() {
		t.Fatalf("fallback attempts=%d", got)
	}
	if !strings.Contains(log.String(), "launch counted against fallback") {
		t.Fatalf("log: %s", log)
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	if !gate.allowLaunch() {
		t.Fatal("recovered store should allow the second launch")
	}
	if got, _ := realAttempts(t, extenderActivationUnidentifiedAccount); got != 2*hostActivationPosts() {
		t.Fatalf("real record holds %d posts, want the fallback launch and this one (%d)", got, 2*hostActivationPosts())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("merged fallback still present: %v", err)
	}
}

// Fallback spending is reconciled into the real record: a recovered store must
// not hand back a fresh allowance.
func TestExtenderLedgerFallbackToReal(t *testing.T) {
	_, path := fallbackTestSetup(t)
	locked := lockedStore(t)
	gate := newExtenderActivationGate()
	for i := 0; i < 2; i++ {
		if !gate.allowLaunch() {
			t.Fatalf("fallback launch %d refused", i+1)
		}
	}
	if got := fallbackAttempts(t, path, extenderActivationUnidentifiedAccount); got != 4 {
		t.Fatalf("fallback posts=%d want 4", got)
	}
	_ = locked.Close()
	if gate.allowLaunch() {
		t.Fatal("recovered store authorised a sixth post: fallback spending was not reconciled")
	}
	if got, _ := realAttempts(t, extenderActivationUnidentifiedAccount); got != 4 {
		t.Fatalf("real posts=%d want the 4 reconciled from the fallback", got)
	}
}

// Real spending survives an outage: the fallback adds to it on recovery.
func TestExtenderLedgerRealToFallback(t *testing.T) {
	_, path := fallbackTestSetup(t)
	gate := newExtenderActivationGate()
	if !gate.allowLaunch() {
		t.Fatal("first real launch refused")
	}
	closeProviderStore()
	locked := lockedStore(t)
	for i := 0; i < 2; i++ {
		if !gate.allowLaunch() {
			t.Fatalf("outage launch %d refused", i+1)
		}
	}
	if got := fallbackAttempts(t, path, extenderActivationUnidentifiedAccount); got != 2*hostActivationPosts() {
		t.Fatalf("fallback posts=%d", got)
	}
	_ = locked.Close()
	closeProviderStore()
	if gate.allowLaunch() {
		t.Fatal("recovered record forgot the launch made before the outage")
	}
	if got, _ := realAttempts(t, extenderActivationUnidentifiedAccount); got != 3*hostActivationPosts() {
		t.Fatalf("real posts=%d want %d", got, 3*hostActivationPosts())
	}
}

// Alternating between the ledgers never loses a post.
func TestExtenderLedgerAlternates(t *testing.T) {
	fallbackTestSetup(t)
	now := time.Now()
	account := extenderActivationUnidentifiedAccount
	spent := 0
	for i := 0; i < 2; i++ {
		if allow, _, _, err := extenderActivationDecision(now, 1, ""); err != nil || !allow {
			t.Fatalf("real %d: allow=%v err=%v", i, allow, err)
		}
		spent++
		closeProviderStore()
		locked := lockedStore(t)
		if allow, _, _, err := decideExtenderFallback(now, 1, ""); err != nil || !allow {
			t.Fatalf("fallback %d: allow=%v err=%v", i, allow, err)
		}
		spent++
		_ = locked.Close()
		closeProviderStore()
	}
	if _, _, _, err := extenderActivationDecision(now, 0, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := realAttempts(t, account); got != spent {
		t.Fatalf("real record holds %d posts, spent %d", got, spent)
	}
}

// A refusal hold taken while the store was out reaches the real record.
func TestExtenderLedgerHoldTravels(t *testing.T) {
	fallbackTestSetup(t)
	locked := lockedStore(t)
	now := time.Now()
	if wait, persisted := recordExtenderActivationRefusedPersisted(now, extenderTestAccountA); persisted || wait != extenderRefusalBackoff {
		t.Fatalf("outage refusal = (%s, %v)", wait, persisted)
	}
	if allow, _, _, err := decideExtenderFallback(now.Add(time.Minute), 1, extenderTestAccountA); err != nil || allow {
		t.Fatalf("fallback ignored the hold: allow=%v err=%v", allow, err)
	}
	_ = locked.Close()
	closeProviderStore()
	allow, wait, _, err := extenderActivationDecision(now.Add(time.Minute), 1, extenderTestAccountA)
	if err != nil || allow || wait <= 0 {
		t.Fatalf("recovered record dropped the hold: allow=%v wait=%s err=%v", allow, wait, err)
	}
}

// A merge whose fallback file could not be removed must not apply twice.
func TestExtenderLedgerMergeIsIdempotent(t *testing.T) {
	_, path := fallbackTestSetup(t)
	locked := lockedStore(t)
	now := time.Now()
	if allow, _, _, err := decideExtenderFallback(now, 2, ""); err != nil || !allow {
		t.Fatalf("fallback: allow=%v err=%v", allow, err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = locked.Close()
	closeProviderStore()
	for i := 0; i < 3; i++ {
		if _, _, _, err := extenderActivationDecision(now, 0, ""); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, saved, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := realAttempts(t, extenderActivationUnidentifiedAccount); got != 2 {
		t.Fatalf("real posts=%d want 2: the same fallback was merged repeatedly", got)
	}
}

// Both ledgers are decided under one cross-process lock.
func TestExtenderLedgerLockCoversBothPaths(t *testing.T) {
	_, path := fallbackTestSetup(t)
	held := flock.New(path + ".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := held.Lock(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, _, _, err := extenderActivationDecision(now, 1, ""); err == nil {
		t.Fatal("real decision ran without the ledger lock")
	}
	if _, _, _, err := decideExtenderFallback(now, 1, ""); err == nil {
		t.Fatal("fallback decision ran without the ledger lock")
	}
	_ = held.Unlock()
	if allow, _, _, err := extenderActivationDecision(now, 1, ""); err != nil || !allow {
		t.Fatalf("after release: allow=%v err=%v", allow, err)
	}
	if got, _ := realAttempts(t, extenderActivationUnidentifiedAccount); got != 1 {
		t.Fatalf("real posts=%d want 1", got)
	}
}

// Without a usable fallback, a process lets only two launches through
// uncounted, however often it restarts the role.
func TestExtenderFallbackUncountedBound(t *testing.T) {
	log, _ := fallbackTestSetup(t)
	stubExtenderMutation(t, extenderActivationState{}, 100, 0)
	path, err := extenderFallbackPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	gate := newExtenderActivationGate()
	for i := 1; i <= 5; i++ {
		allow := gate.allowLaunch()
		if allow != (i <= extenderUncountedLaunchMax) {
			t.Fatalf("launch %d allowed=%v", i, allow)
		}
	}
	text := log.String()
	if !strings.Contains(text, "uncounted launch 2 of 2") || !strings.Contains(text, "extender role off: activation budget could not be recorded") || !strings.Contains(text, "already launched 2 uncounted") {
		t.Fatalf("log: %s", text)
	}
}

// The bound does not depend on the fallback file: a held ledger lock is the
// same fault.
func TestExtenderFallbackUncountedBoundOnLockedLedger(t *testing.T) {
	_, path := fallbackTestSetup(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	held := flock.New(path + ".lock")
	if err := held.Lock(); err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()
	gate := newExtenderActivationGate()
	for i := 1; i <= 3; i++ {
		if allow := gate.allowLaunch(); allow != (i <= extenderUncountedLaunchMax) {
			t.Fatalf("launch %d allowed=%v", i, allow)
		}
	}
}

func TestExtenderFallbackHoldRefuses(t *testing.T) {
	log, _ := fallbackTestSetup(t)
	now := time.Now()
	for i := 0; i < 2; i++ {
		if allow, _, _, err := decideExtenderFallback(now, 2, ""); err != nil || !allow {
			t.Fatalf("launch %d: allow=%v err=%v", i, allow, err)
		}
	}
	if allowExtenderFallback(now, 2, "", errors.New("store out")) {
		t.Fatal("exhausted fallback allowed a launch")
	}
	if !strings.Contains(log.String(), "fallback activation budget held") {
		t.Fatalf("log: %s", log)
	}
}

func TestExtenderFallbackRealExhaustedUntouched(t *testing.T) {
	log, path := fallbackTestSetup(t)
	store := providerStoreInstance()
	if store == nil {
		t.Fatal("open store")
	}
	now := time.Now().Unix()
	record := extenderActivationRecord{Accounts: map[string]extenderActivationState{extenderActivationUnidentifiedAccount: {Attempts: []int64{now, now, now, now, now}}}}
	if err := store.save(providerStoreBucketState, providerStoreKeyExtenderActivation, record); err != nil {
		t.Fatal(err)
	}
	if newExtenderActivationGate().allowLaunch() {
		t.Fatal("readable exhausted store must refuse")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fallback touched: %v", err)
	}
	if !strings.Contains(log.String(), "activation held:") {
		t.Fatalf("log: %s", log)
	}
}

func TestExtenderFallbackReadableCapWriteFailure(t *testing.T) {
	_, path := fallbackTestSetup(t)
	now := time.Now().Unix()
	stubExtenderMutation(t, extenderActivationState{Attempts: []int64{now, now, now, now, now}}, 0, 100)
	if newExtenderActivationGate().allowLaunch() {
		t.Fatal("readable exhausted cap must refuse even if write fails")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fallback touched: %v", err)
	}
}
