package miner

import (
	"testing"

	"github.com/urnetwork/sdk"
)

func TestCallbackRefusalRetriesUnchangedObservation(t *testing.T) {
	testExtenderState(t)
	const account = extenderTestAccountA
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	listener := newProviderExtenderStatusListener(nil, 0, account)
	status := &sdk.ExtenderProvideStatus{Enabled: true, Listening: true, LastActivationTime: 1800000000000, LastActivationError: "refused", LastActivationRefused: true}
	releaseProviderStore()
	listener.ExtenderProvideStatusChanged(status)
	if err := restoreProviderStoreAfterAbortedHandoff(); err != nil {
		t.Fatal(err)
	}
	listener.ExtenderProvideStatusChanged(status)
	record, err := readExtenderActivationRecord(account)
	if err != nil {
		t.Fatal(err)
	}
	if got := record.Accounts[account].Refusals; got != 1 {
		t.Fatalf("refusals after unchanged redelivery = %d, want 1", got)
	}
	listener.ExtenderProvideStatusChanged(status)
	changed := *status
	changed.Listening = false
	listener.ExtenderProvideStatusChanged(&changed)
	record, err = readExtenderActivationRecord(account)
	if err != nil || record.Accounts[account].Refusals != 1 {
		t.Fatalf("refusals after persistence and redelivery = %d, err %v; want 1", record.Accounts[account].Refusals, err)
	}
}
