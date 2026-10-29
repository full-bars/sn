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
