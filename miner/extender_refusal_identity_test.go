package miner

import (
	"testing"

	"github.com/urnetwork/sdk"
)

// Two refusals that finish in one millisecond with the same family flags are
// still two refusals when they say different things, and the record must
// charge both.
func TestSameMillisecondRefusalsWithDifferentTextAreCountedSeparately(t *testing.T) {
	testExtenderState(t)
	listener := newProviderExtenderStatusListener(nil, 0, extenderTestAccountA)
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, LastActivationTime: 7001, LastActivationError: "v4: over budget", LastActivationRefused: true})
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, LastActivationTime: 7001, LastActivationError: "v6: over budget", LastActivationRefused: true})
	if got := readRefusals(t, extenderTestAccountA); got != 2 {
		t.Fatalf("refusals = %d, want the two distinct refusals charged", got)
	}
}

// A status delivered again, or re-read beside a family flag that changed while
// the same refusal stands, is one refusal: the key follows the refusal's own
// payload, not the flags that drift around it.
func TestRedeliveredRefusalIsCountedOnceWhateverTheFamilyFlags(t *testing.T) {
	testExtenderState(t)
	listener := newProviderExtenderStatusListener(nil, 0, extenderTestAccountA)
	refused := sdk.ExtenderProvideStatus{Enabled: true, LastActivationTime: 7101, LastActivationError: "refused", LastActivationRefused: true}
	again := refused
	listener.ExtenderProvideStatusChanged(&refused)
	listener.ExtenderProvideStatusChanged(&again)
	drift := refused
	drift.ActivatedV4 = true
	listener.ExtenderProvideStatusChanged(&drift)
	drift.ActivatedV6 = true
	listener.ExtenderProvideStatusChanged(&drift)
	if got := readRefusals(t, extenderTestAccountA); got != 1 {
		t.Fatalf("refusals = %d, want one: a redelivery or a drifting flag charged the standing refusal again", got)
	}
}

// The status's time is the latest attempt of either family, apart from the
// error it still reports. A v6 refusal that stands while v4 then activates at a
// newer millisecond is re-delivered with v4's time, and it is still the one
// refusal.
func TestStandingRefusalIsNotChargedAgainAtTheOtherFamilysNewerTime(t *testing.T) {
	testExtenderState(t)
	listener := newProviderExtenderStatusListener(nil, 0, extenderTestAccountA)
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, LastActivationTime: 7201, LastActivationError: "v6 refused", LastActivationRefused: true})
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, ActivatedV4: true, LastActivationTime: 7202, LastActivationError: "v6 refused", LastActivationRefused: true})
	if got := readRefusals(t, extenderTestAccountA); got != 1 {
		t.Fatalf("refusals = %d, want one: the other family's newer time charged the standing refusal again", got)
	}
}

// With no family activated, a newer time can only be a newer failed attempt,
// so the same text refused again is a second refusal.
func TestRepeatedRefusalWithNoFamilyActivatedIsChargedAgain(t *testing.T) {
	testExtenderState(t)
	listener := newProviderExtenderStatusListener(nil, 0, extenderTestAccountA)
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, LastActivationTime: 7301, LastActivationError: "refused", LastActivationRefused: true})
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, LastActivationTime: 7302, LastActivationError: "refused", LastActivationRefused: true})
	if got := readRefusals(t, extenderTestAccountA); got != 2 {
		t.Fatalf("refusals = %d, want two attempts refused one after the other", got)
	}
}
