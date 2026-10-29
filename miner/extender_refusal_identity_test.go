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
