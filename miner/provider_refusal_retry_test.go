package miner

import (
	"testing"
	"time"

	"github.com/urnetwork/sdk"
)

func waitForRefusalCount(t *testing.T, account string, count int) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		record, err := readExtenderActivationRecord(account)
		if err == nil && record.Accounts[account].Refusals == count {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	record, err := readExtenderActivationRecord(account)
	t.Fatalf("refusals after quiet recovery = %d, want %d (error %v)", record.Accounts[account].Refusals, count, err)
}

func TestRefusalRetryWithoutAnotherStatus(t *testing.T) {
	testExtenderState(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	listener := newProviderExtenderStatusListener(nil, 0, extenderTestAccountA)
	releaseProviderStore()
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, LastActivationTime: 101, LastActivationError: "refused", LastActivationRefused: true})
	if err := restoreProviderStoreAfterAbortedHandoff(); err != nil {
		t.Fatal(err)
	}
	waitForRefusalCount(t, extenderTestAccountA, 1)
}

func TestRefusalRetryRetainsEarlierAttempt(t *testing.T) {
	testExtenderState(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	listener := newProviderExtenderStatusListener(nil, 0, extenderTestAccountA)
	releaseProviderStore()
	first := &sdk.ExtenderProvideStatus{Enabled: true, LastActivationTime: 201, LastActivationError: "refused", LastActivationRefused: true}
	second := *first
	second.LastActivationTime = 202
	listener.ExtenderProvideStatusChanged(first)
	listener.ExtenderProvideStatusChanged(&second)
	if err := restoreProviderStoreAfterAbortedHandoff(); err != nil {
		t.Fatal(err)
	}
	listener.ExtenderProvideStatusChanged(&second)
	waitForRefusalCount(t, extenderTestAccountA, 2)
	listener.ExtenderProvideStatusChanged(first)
	listener.ExtenderProvideStatusChanged(&second)
	record, err := readExtenderActivationRecord(extenderTestAccountA)
	if err != nil || record.Accounts[extenderTestAccountA].Refusals != 2 {
		t.Fatalf("redelivery double charged: %+v, %v", record, err)
	}
}

// A refusal the store did not take is still a verdict. Once the store is back,
// the first admission must charge it and stay closed, rather than pass on a
// record that has not heard of it while the retry is still waiting.
func TestPendingRefusalHoldsAdmissionUntilDurable(t *testing.T) {
	testExtenderState(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	listener := newProviderExtenderStatusListener(nil, 0, extenderTestAccountA)
	releaseProviderStore()
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, LastActivationTime: 301, LastActivationError: "refused", LastActivationRefused: true})
	if allow, _, _ := extenderActivationAllow(time.Now(), 1, extenderTestAccountA); allow {
		t.Fatal("admitted while the refusal could not be kept")
	}
	if err := restoreProviderStoreAfterAbortedHandoff(); err != nil {
		t.Fatal(err)
	}
	// The retry waits providerStoreRetryInterval; this admission comes first.
	if allow, wait, _ := extenderActivationAllow(time.Now(), 1, extenderTestAccountA); allow || wait <= 0 {
		t.Fatalf("admission after recovery = %t (wait %s), want held by the refusal", allow, wait)
	}
	record, err := readExtenderActivationRecord(extenderTestAccountA)
	if err != nil || record.Accounts[extenderTestAccountA].Refusals != 1 {
		t.Fatalf("refusal not kept by the admission that passed it: %+v, %v", record, err)
	}
	if held := extenderRefusalsHeldCount(); held != 0 {
		t.Fatalf("%d refusals still held after they were kept", held)
	}
}

// A process can end while its store is unavailable: a restart, or a hotswap
// parent that released the store to its candidate. The refusal it held must
// reach the next process's first admission.
func TestHeldRefusalSurvivesTheProcess(t *testing.T) {
	testExtenderState(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	listener := newProviderExtenderStatusListener(nil, 0, extenderTestAccountA)
	releaseProviderStore()
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, LastActivationTime: 401, LastActivationError: "refused", LastActivationRefused: true})

	// The process ends: its memory goes, and the next one opens the store.
	resetExtenderRefusals()
	closeProviderStore()

	if allow, wait, _ := extenderActivationAllow(time.Now(), 1, extenderTestAccountA); allow || wait <= 0 {
		t.Fatalf("admission in the next process = %t (wait %s), want held by the refusal", allow, wait)
	}
	record, err := readExtenderActivationRecord(extenderTestAccountA)
	if err != nil || record.Accounts[extenderTestAccountA].Refusals != 1 {
		t.Fatalf("the next process did not keep the refusal: %+v, %v", record, err)
	}
	if onDisk, err := readExtenderRefusalJournal(time.Now()); err != nil || len(onDisk) != 0 {
		t.Fatalf("held refusals left on disk after they were kept: %v, %v", onDisk, err)
	}
	// Charged once: a second process reading the same file again finds nothing.
	if _, _, _ = extenderActivationAllow(time.Now(), 1, extenderTestAccountA); readRefusals(t, extenderTestAccountA) != 1 {
		t.Fatal("the held refusal was charged twice")
	}
}

func readRefusals(t *testing.T, account string) int {
	t.Helper()
	record, err := readExtenderActivationRecord(account)
	if err != nil {
		t.Fatal(err)
	}
	return record.Accounts[account].Refusals
}
