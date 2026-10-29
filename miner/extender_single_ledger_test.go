package miner

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bbolt "go.etcd.io/bbolt"
)

func resetUncountedExtenderForTest(t *testing.T) {
	t.Helper()
	extenderUncountedLaunchUsed.Store(false)
	t.Cleanup(func() { extenderUncountedLaunchUsed.Store(false) })
}

func TestExtenderUnreadableLedgerWithholdsLaunch(t *testing.T) {
	testExtenderState(t)
	resetUncountedExtenderForTest(t)
	dir, err := providerStateDir()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := bbolt.Open(filepath.Join(dir, providerStoreFileName), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	log := &bytes.Buffer{}
	previous := proxyManagerLogWriter
	proxyManagerLogWriter = log
	t.Cleanup(func() { proxyManagerLogWriter = previous })
	first, second := newExtenderActivationGate(), newExtenderActivationGate()
	if first.decideAtStart() {
		t.Fatal("unavailable startup verdict was not withheld")
	}
	if first.allowLaunch() || second.allowLaunch() || extenderUncountedLaunchUsed.Load() {
		t.Fatal("an unavailable existing ledger allowed an uncounted launch")
	}
	if !strings.Contains(log.String(), "timeout") || !strings.Contains(log.String(), "unavailable state store") {
		t.Fatalf("misleading log: %s", log.String())
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	closeProviderStore()
	store := providerStoreInstance()
	if store == nil {
		t.Fatal("store did not recover")
	}
	var record extenderActivationRecord
	found, err := store.load(providerStoreBucketState, providerStoreKeyExtenderActivation, &record)
	if err != nil || found {
		t.Fatalf("uncounted launch was persisted: found=%v err=%v", found, err)
	}
	if !second.allowLaunch() {
		t.Fatal("recovered empty ledger should allow a counted launch")
	}
	found, err = store.load(providerStoreBucketState, providerStoreKeyExtenderActivation, &record)
	if err != nil || !found || len(record.Accounts[extenderActivationUnidentifiedAccount].Attempts) != hostActivationPosts() {
		t.Fatalf("recovered record=%+v found=%v err=%v", record, found, err)
	}
}

func TestUncountedLaunchCannotPassCommittedRefusalInLockedStore(t *testing.T) {
	testExtenderState(t)
	resetUncountedExtenderForTest(t)
	store := providerStoreInstance()
	if store == nil {
		t.Fatal("open store")
	}
	now := time.Now()
	record := extenderActivationRecord{Accounts: map[string]extenderActivationState{
		extenderTestAccountA: {Refusals: 1, LastRefusalUnix: now.Unix(), BackoffUntilUnix: now.Add(time.Hour).Unix()},
	}}
	if err := store.save(providerStoreBucketState, providerStoreKeyExtenderActivation, record); err != nil {
		t.Fatal(err)
	}
	closeProviderStore()
	dir, err := providerStateDir()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := bbolt.Open(filepath.Join(dir, providerStoreFileName), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	if newExtenderActivationGate().allowLaunch() {
		t.Fatal("uncounted launch bypassed a committed refusal in an unavailable store")
	}
}

func TestExtenderReadableLedgerRefusesAtCap(t *testing.T) {
	testExtenderState(t)
	resetUncountedExtenderForTest(t)
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
		t.Fatal("readable cap allowed launch")
	}
	if extenderUncountedLaunchUsed.Load() {
		t.Fatal("readable cap consumed uncounted allowance")
	}
}

func TestExtenderUncountedAllowanceIsAtomic(t *testing.T) {
	testExtenderState(t)
	resetUncountedExtenderForTest(t)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if allowUncountedExtenderLaunch(errors.New("unavailable")) {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("uncounted launches=%d, want 1", successes.Load())
	}
}

func TestExtenderReadableCapSurvivesWriteFailure(t *testing.T) {
	testExtenderState(t)
	resetUncountedExtenderForTest(t)
	now := time.Now().Unix()
	stubExtenderMutation(t, extenderActivationState{Attempts: []int64{now, now, now, now, now}}, 0, 100)
	if newExtenderActivationGate().allowLaunch() {
		t.Fatal("readable cap allowed after write failure")
	}
	if extenderUncountedLaunchUsed.Load() {
		t.Fatal("readable cap consumed uncounted allowance")
	}
}

func TestExtenderReleasedStoreNeverUsesUncountedLaunch(t *testing.T) {
	testExtenderState(t)
	resetUncountedExtenderForTest(t)
	globalProviderStoreMu.Lock()
	globalProviderStoreReleased = true
	globalProviderStoreMu.Unlock()
	if newExtenderActivationGate().allowLaunch() || extenderUncountedLaunchUsed.Load() {
		t.Fatal("released store allowed uncounted launch")
	}
}

func TestExtenderProbeEntryPointsBoundedOnLockedStore(t *testing.T) {
	testExtenderState(t)
	resetUncountedExtenderForTest(t)
	dir, err := providerStateDir()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := bbolt.Open(filepath.Join(dir, providerStoreFileName), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	gate := newExtenderActivationGate()
	start := time.Now()
	gate.decideAtStart()
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Errorf("startup probe took %s", elapsed)
	}
	start = time.Now()
	gate.allowLaunch()
	if elapsed := time.Since(start); elapsed >= 1500*time.Millisecond {
		t.Errorf("launch probe took %s", elapsed)
	}
}

func TestExtenderIgnoresObsoleteFallbackFile(t *testing.T) {
	testExtenderState(t)
	resetUncountedExtenderForTest(t)
	dir, err := providerStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir+".extender-activation-fallback.json", 0700); err != nil {
		t.Fatal(err)
	}
	if !newExtenderActivationGate().allowLaunch() {
		t.Fatal("obsolete fallback blocked readable store")
	}
	if extenderUncountedLaunchUsed.Load() {
		t.Fatal("obsolete file caused uncounted launch")
	}
}

// The barrier keeps a newly published refusal ahead of an uncounted decision.
func TestUncountedLaunchWaitsForRefusalBarrier(t *testing.T) {
	testExtenderState(t)
	resetUncountedExtenderForTest(t)
	release, err := lockExtenderRefusalBarrier()
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		close(started)
		done <- allowUncountedExtenderLaunch(errors.New("unavailable"))
	}()
	<-started
	extenderRefusalsHeld.mu.Lock()
	queueExtenderOpLocked(extenderPendingOp{kind: extenderOpRefusal, account: extenderTestAccountA, attempt: "scope.barrier", at: time.Now()})
	extenderRefusalsHeld.mu.Unlock()
	release()
	if <-done {
		t.Fatal("uncounted launch passed a refusal published under the barrier")
	}
}

func TestUncountedLaunchIsClosedByAHeldRefusal(t *testing.T) {
	testExtenderState(t)
	resetUncountedExtenderForTest(t)
	dir, err := providerStateDir()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := bbolt.Open(filepath.Join(dir, providerStoreFileName), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	failJournalRename(t)
	holdExtenderRefusal(extenderTestAccountA, "scope.2501")
	if newExtenderActivationGate().allowLaunch() {
		t.Fatal("the uncounted launch posted past a refusal this process holds")
	}
	if extenderUncountedLaunchUsed.Load() {
		t.Fatal("the held refusal spent the process's uncounted launch")
	}
}
