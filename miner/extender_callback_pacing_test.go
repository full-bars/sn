package miner

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sdk"
)

// Status callbacks arrive at whatever rate the device sends them. A burst of
// changed successes and distinct refusals must cost one store transaction and
// one journal file, keep every refusal, and leave admission closed to all of
// them before they are written.
func TestCallbackBurstIsCoalescedAndAdmissionStaysClosed(t *testing.T) {
	testExtenderState(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	extenderPersistPace.Store(int64(time.Hour))
	listener := newProviderExtenderStatusListener(nil, 0, extenderTestAccountA)
	writes, journals := providerStoreWrites.Load(), extenderRefusalJournalWrites.Load()

	for i := 0; i < 5; i++ {
		// Changed successes: each is a new observation of an activation.
		listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, Listening: i%2 == 0, ActivatedV4: true, LastActivationTime: int64(1000 + i)})
	}
	const refusals = 10
	for i := 0; i < refusals; i++ {
		listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, LastActivationTime: int64(2000 + i), LastActivationError: "refused", LastActivationRefused: true})
	}
	if got := providerStoreWrites.Load() - writes; got != 1 {
		t.Fatalf("a burst of %d callbacks ran %d store transactions, want 1", 5+refusals, got)
	}
	if got := extenderRefusalJournalWrites.Load() - journals; got != 1 {
		t.Fatalf("a burst of %d refusals wrote %d journal files, want 1", refusals, got)
	}
	// Whatever is not written yet is still held, and admission is closed to it.
	if held := extenderRefusalsHeldCount(); held != refusals {
		t.Fatalf("held %d refusals after the burst, want %d: a coalesced refusal was dropped", held, refusals)
	}
	if allow, wait, _ := extenderActivationAllow(time.Now(), 1, extenderTestAccountA); allow || wait <= 0 {
		t.Fatalf("admission after the burst = %t (wait %s), want held", allow, wait)
	}
	if got := readRefusals(t, extenderTestAccountA); got != refusals {
		t.Fatalf("the admission charged %d refusals, want all %d", got, refusals)
	}
	if held := extenderRefusalsHeldCount(); held != 0 {
		t.Fatalf("%d refusals still held after they were kept", held)
	}
}

// Failed I/O must not turn the pace into a retry per callback: a journal that
// cannot be written is tried once in the interval, and the refusals stay held.
func TestCallbackBurstAgainstFailingJournalWritesOnce(t *testing.T) {
	testExtenderState(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	extenderPersistPace.Store(int64(time.Hour))
	original := providerStoreDurableRename
	t.Cleanup(func() { providerStoreDurableRename = original })
	providerStoreDurableRename = func(string, string) error { return errors.New("rename failed") }
	releaseProviderStore()
	listener := newProviderExtenderStatusListener(nil, 0, extenderTestAccountA)
	journals := extenderRefusalJournalWrites.Load()
	for i := 0; i < 10; i++ {
		listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, LastActivationTime: int64(3000 + i), LastActivationError: "refused", LastActivationRefused: true})
	}
	if got := extenderRefusalJournalWrites.Load() - journals; got != 1 {
		t.Fatalf("10 refusals against a failing journal ran %d journal writes, want 1", got)
	}
	if held := extenderRefusalsHeldCount(); held != 10 {
		t.Fatalf("held %d refusals, want all 10", held)
	}
}
