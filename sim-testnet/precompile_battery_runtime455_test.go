// Runtime custody failures must remain blocking even when other precompile
// families return a complete diagnostic battery.
package main

import (
	"math/big"
	"testing"
)

// Uses synthetic tuple values to isolate the existing acceptance boundary
// from any particular probe deployment or source of its custody mapping.
func TestPrecompileBatteryRejectsEveryFailedCustodyCheck(t *testing.T) {
	probeColdkey := [32]byte{2}
	mirrorKat := [32]byte{1}
	evidence := &PrecompileConformanceEvidence{SampleUID: 2, ProbeColdkey: hexBytesValue(probeColdkey[:])}
	evidence.Battery.MirrorKAT = hexBytesValue(mirrorKat[:])
	baseline := precompileBatteryTuple{
		BlakeOk: true, MirrorKat: mirrorKat, BlakeKatMatch: true, SelfColdkey: probeColdkey,
		EdOk: true, EdVerifyGood: true, EdVerifyBad: true, SrOk: true, SrVerifyGood: true, SrVerifyBad: true,
		MgOk: true, UidCount: 3, Uid0Hotkey: [32]byte{4}, Uid0Coldkey: [32]byte{5},
		NeuronOk: true, SampleExists: true, SampleUid: 2, AbsentRejected: true, StakeViewOk: true,
		SampleSelfStake: big.NewInt(6), NominatorMinimum: big.NewInt(7),
	}
	if !batteryTupleCompatible(evidence, &baseline, 7) {
		t.Fatal("complete custody and cryptographic controls were rejected")
	}
	for _, c := range []struct {
		name   string
		mutate func(*precompileBatteryTuple)
	}{
		{name: "mapping unavailable", mutate: func(tuple *precompileBatteryTuple) { tuple.BlakeOk = false }},
		{name: "mapping known answer failed", mutate: func(tuple *precompileBatteryTuple) { tuple.BlakeKatMatch = false }},
		{name: "mapping answer changed", mutate: func(tuple *precompileBatteryTuple) { tuple.MirrorKat = [32]byte{9} }},
		{name: "self mapping absent", mutate: func(tuple *precompileBatteryTuple) { tuple.SelfColdkey = [32]byte{} }},
		{name: "self mapping changed", mutate: func(tuple *precompileBatteryTuple) { tuple.SelfColdkey = [32]byte{9} }},
		{name: "custody stake unobserved", mutate: func(tuple *precompileBatteryTuple) { tuple.StakeViewOk = false }},
	} {
		tuple := baseline
		c.mutate(&tuple)
		if batteryTupleCompatible(evidence, &tuple, 7) {
			t.Errorf("%s passed the conformance gate", c.name)
		}
	}
}
