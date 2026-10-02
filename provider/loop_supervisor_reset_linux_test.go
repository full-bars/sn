//go:build linux

package provider

import "testing"

// A dead pressure monitor must not leave the last (possibly emergency) score,
// tightened GOGC or shrunk memory budget driving the pool controller.
func TestResetPressureActuatorsFailsNeutral(t *testing.T) {
	setPressure(0.95)
	gcTightening.Store(true)
	var setTo []int
	st := &gcGovernorState{baselineGOGC: 100, currentGOGC: 25, level: 2}
	resetPressureActuators(st, func(n int) int { setTo = append(setTo, n); return 25 })

	if currentPressure() != 0 {
		t.Fatalf("pressure = %v, want 0", currentPressure())
	}
	if gcTightening.Load() {
		t.Fatal("gcTightening still set")
	}
	if len(setTo) != 1 || setTo[0] != 100 || st.currentGOGC != 100 || st.level != 0 {
		t.Fatalf("GOGC reset: set=%v currentGOGC=%d level=%d, want baseline 100 and level 0", setTo, st.currentGOGC, st.level)
	}
}

// When the governor never tightened, the reset must not touch GOGC at all.
func TestResetPressureActuatorsLeavesUntouchedGOGCAlone(t *testing.T) {
	st := &gcGovernorState{baselineGOGC: 100, currentGOGC: 100}
	called := false
	resetPressureActuators(st, func(n int) int { called = true; return 100 })
	if called {
		t.Fatal("SetGCPercent called although GOGC was already at baseline")
	}
}
