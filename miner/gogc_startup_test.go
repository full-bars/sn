package miner

import (
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
)

// gogc_startup_test.go pins the startup replay of a persisted
// `urnet-tools set gogc` and the GOGC half of the one runtime-tuning startup
// line.
//
// The bug this covers: the control socket applies gogc live and every profile
// tier skips its own GOGC default while persistedRuntimeTuningActive reports the
// key as set, but the miner had no startup replay. After a restart the runtime
// ran at its own default while the tiers kept skipping, so an operator's
// `urnet-tools set gogc 50` was silently gone and anything that assumed it was
// in force (the eco/turbo memory sizing, a capacity plan) was wrong.
//
// The value space is the socket's, not a new one: a non-negative percentage,
// "disabled" for collection off, and "off" for the clear value (the same word
// every tuning key uses). parseGoGCTarget is shared by validation, the live
// apply and the replay; these tests drive all three.

// currentGOGC reads the GOGC percentage in force, failing the test when the
// runtime does not expose it.
func currentGOGC(t *testing.T) int {
	t.Helper()
	percent, ok := readGOGCPercent()
	if !ok {
		t.Fatalf("the runtime does not expose GOGC; the test cannot read the value in force")
	}
	return int(percent)
}

// A persisted pin must be in force after the start, and the tier that would
// otherwise install its own default (eco 50) must skip for it.
func TestStartupAppliesPersistedGoGCAndTheTierSkips(t *testing.T) {
	preserveRuntimeTuning(t)
	state := withControlState(t)
	clearTuningEnv(t)
	t.Setenv("URNETWORK_PROFILE", profileEco)

	if err := state.set("gogc", "55"); err != nil {
		t.Fatalf("set gogc: %v", err)
	}
	// The runtime default; the eco tier's own default is 50, so a value of 55
	// can only come from the replay.
	debug.SetGCPercent(100)

	lines := recordStartupLines(t)
	applyStartupMemoryTuning(0, 4)

	if got := currentGOGC(t); got != 55 {
		t.Fatalf("GOGC after startup = %d, want the persisted 55", got)
	}
	line := goMemLimitSourceLine(t, *lines)
	if !strings.Contains(line, "GOGC 55") || !strings.Contains(line, "persisted urnet-tools set gogc") {
		t.Fatalf("startup line %q must name the pinned value and the persisted source", line)
	}
}

// "disabled" turns collection off and must survive a restart as -1, not as a
// percentage and not as "off" (which clears).
func TestStartupPersistedGoGCDisabled(t *testing.T) {
	for _, value := range []string{"disabled", "DISABLED"} {
		t.Run(value, func(t *testing.T) {
			preserveRuntimeTuning(t)
			state := withControlState(t)
			clearTuningEnv(t)
			t.Setenv("URNETWORK_PROFILE", profileEco)

			if err := state.set("gogc", value); err != nil {
				t.Fatalf("set gogc: %v", err)
			}
			debug.SetGCPercent(100)

			lines := recordStartupLines(t)
			applyStartupMemoryTuning(0, 4)

			if got := currentGOGC(t); got != -1 {
				t.Fatalf("GOGC after startup = %d, want -1 (collection off)", got)
			}
			line := goMemLimitSourceLine(t, *lines)
			if !strings.Contains(line, "GOGC off, collection disabled") || !strings.Contains(line, "persisted urnet-tools set gogc") {
				t.Fatalf("startup line %q must say collection is off and name the persisted source", line)
			}
		})
	}
}

// GOGC in the environment still outranks the persisted value: the runtime
// installed it before main, every tier skipped for it, and the replay must
// leave it alone rather than reverse the setting that wins.
func TestStartupEnvGoGCOutranksThePersistedValue(t *testing.T) {
	preserveRuntimeTuning(t)
	state := withControlState(t)
	clearTuningEnv(t)
	t.Setenv("URNETWORK_PROFILE", profileEco)

	// A test cannot re-run runtime init, so install what GOGC=37 installs at
	// boot.
	t.Setenv("GOGC", "37")
	debug.SetGCPercent(37)

	if err := state.set("gogc", "55"); err != nil {
		t.Fatalf("set gogc: %v", err)
	}

	lines := recordStartupLines(t)
	applyStartupMemoryTuning(0, 4)

	if got := currentGOGC(t); got != 37 {
		t.Fatalf("GOGC after startup = %d, want the environment's 37", got)
	}
	line := goMemLimitSourceLine(t, *lines)
	if !strings.Contains(line, "GOGC 37") || !strings.Contains(line, "env GOGC") {
		t.Fatalf("startup line %q must name the environment value and the env source", line)
	}
}

// With no pin at all the existing behaviour must be untouched: the tier's own
// default where a tier applies, the runtime default otherwise.
func TestStartupGoGCWithoutAPinKeepsTheTierOrRuntimeDefault(t *testing.T) {
	if detectEffectiveRAMLimitBytes() <= 0 {
		// The eco/turbo tiers also install a memory limit sized from RAM; a
		// zero reading would give the test process a zero-byte soft limit.
		t.Skip("effective RAM is unreadable; the tiers have no limit to install")
	}
	cases := []struct {
		name    string
		profile string
		want    int
		source  string
	}{
		{"eco tier", profileEco, 50, "profile tier " + profileEco},
		{"turbo tier", profileTurboV4, 200, "profile tier " + profileTurboV4},
		{"no tier", "", 100, goGCSourceDefault},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			preserveRuntimeTuning(t)
			withControlState(t)
			clearTuningEnv(t)
			t.Setenv("URNETWORK_PROFILE", c.profile)
			debug.SetGCPercent(100)

			lines := recordStartupLines(t)
			applyStartupMemoryTuning(0, 4)

			if got := currentGOGC(t); got != c.want {
				t.Fatalf("GOGC after startup = %d, want %d", got, c.want)
			}
			line := goMemLimitSourceLine(t, *lines)
			if !strings.Contains(line, "GOGC "+strconv.Itoa(c.want)) || !strings.Contains(line, c.source) {
				t.Fatalf("startup line %q must name GOGC %d and the source %q", line, c.want, c.source)
			}
		})
	}
}

// "off" is the clear value, not a pin: it must leave the tier's default in
// force, and it must not suppress the tier the way a real pin does - the
// uppercase form the socket's validation accepts included.
func TestStartupPersistedGoGCOffIsNotAPin(t *testing.T) {
	for _, value := range []string{"off", "OFF", "Off"} {
		t.Run(value, func(t *testing.T) {
			preserveRuntimeTuning(t)
			state := withControlState(t)
			clearTuningEnv(t)
			t.Setenv("URNETWORK_PROFILE", profileEco)

			if err := state.set("gogc", value); err != nil {
				t.Fatalf("set gogc: %v", err)
			}
			debug.SetGCPercent(100)

			lines := recordStartupLines(t)
			applyStartupMemoryTuning(0, 4)

			if got := currentGOGC(t); got != 50 {
				t.Fatalf("GOGC after startup = %d, want the eco tier's 50: %q is the clear value, not a pin", got, value)
			}
			line := goMemLimitSourceLine(t, *lines)
			if !strings.Contains(line, "GOGC 50") || !strings.Contains(line, "profile tier "+profileEco) {
				t.Fatalf("startup line %q must name the eco tier as the source", line)
			}
		})
	}
}

// A hand-edited, unparseable value (or a negative one the socket would have
// rejected) must not be applied and must not be reported as the pin in force:
// the setting that was there stands, and the line says so. Silence would leave
// an operator believing a pin is set.
func TestStartupUnparseablePersistedGoGCLeavesTheSettingInForce(t *testing.T) {
	for _, value := range []string{"half", "-50"} {
		t.Run(value, func(t *testing.T) {
			preserveRuntimeTuning(t)
			state := withControlState(t)
			clearTuningEnv(t)
			t.Setenv("URNETWORK_PROFILE", profileEco)

			if err := state.set("gogc", value); err != nil {
				t.Fatalf("set gogc: %v", err)
			}
			debug.SetGCPercent(100)

			lines := recordStartupLines(t)
			applyStartupMemoryTuning(0, 4)

			if got := currentGOGC(t); got != 100 {
				t.Fatalf("GOGC after startup = %d, want the 100 that was in force (a bad value must not be applied)", got)
			}
			var complained bool
			for _, line := range *lines {
				if strings.Contains(line, "is not a GC target") && strings.Contains(line, value) {
					complained = true
				}
			}
			if !complained {
				t.Fatalf("no line explains that the persisted value was ignored, got %q", *lines)
			}
			line := goMemLimitSourceLine(t, *lines)
			if strings.Contains(line, "persisted urnet-tools set gogc") {
				t.Fatalf("startup line %q claims a pin that was never applied", line)
			}
		})
	}
}

// The parse is the socket's value space, and nothing outside it: a
// non-negative percentage, "disabled" for collection off, "off" for the clear
// value.
func TestParseGoGCTarget(t *testing.T) {
	cases := []struct {
		value   string
		percent int
		clear   bool
		ok      bool
	}{
		{"50", 50, false, true},
		{"0", 0, false, true},
		{"+50", 50, false, true},
		{"disabled", -1, false, true},
		{"DISABLED", -1, false, true},
		{"off", 100, true, true},
		{"Off", 100, true, true},
		{"hundred", 0, false, false},
		{"-50", 0, false, false},
		{"-1", 0, false, false},
		{"", 0, false, false},
	}
	for _, c := range cases {
		target, err := parseGoGCTarget(c.value)
		if (err == nil) != c.ok {
			t.Errorf("parseGoGCTarget(%q) error = %v, want ok=%v", c.value, err, c.ok)
			continue
		}
		if !c.ok {
			continue
		}
		if target.Percent != c.percent || target.Clear != c.clear {
			t.Errorf("parseGoGCTarget(%q) = {Percent:%d Clear:%v}, want {Percent:%d Clear:%v}",
				c.value, target.Percent, target.Clear, c.percent, c.clear)
		}
	}
}

// The live apply and the startup replay must mean the same thing by the same
// value. If they diverge, a pin behaves one way while the operator is looking
// at it and another way after the next restart - the class of bug the replay
// exists to close. "off" is the exception by design (it clears, so the replay
// declines and the tier decides); TestStartupPersistedGoGCOffIsNotAPin covers
// that side.
func TestGoGCLiveApplyAndReplayAgree(t *testing.T) {
	for _, value := range []string{"55", "0", "disabled", "DISABLED"} {
		t.Run(value, func(t *testing.T) {
			preserveRuntimeTuning(t)
			state := withControlState(t)
			clearTuningEnv(t)

			debug.SetGCPercent(100)
			if err := applyControlLiveSideEffect("gogc", value); err != nil {
				t.Fatalf("live apply rejected %q: %v", value, err)
			}
			live := currentGOGC(t)

			if err := state.set("gogc", value); err != nil {
				t.Fatalf("set gogc: %v", err)
			}
			debug.SetGCPercent(100)
			if !replayPersistedGoGC() {
				t.Fatalf("the replay declined %q, which the live path applied", value)
			}
			if got := currentGOGC(t); got != live {
				t.Fatalf("replay of %q gives GOGC %d, live apply gives %d", value, got, live)
			}
		})
	}
}

// The line is the whole point of the change for the operator: one line has to
// say which value each Go runtime knob holds and who set it.
func TestGoRuntimeTuningLogLine(t *testing.T) {
	cases := []struct {
		name        string
		limit       int64
		limitSource string
		gcPercent   int
		gcOK        bool
		gcSource    string
		want        string
	}{
		{
			name: "both pinned", limit: 512 * testMiB, limitSource: "persisted urnet-tools set gomemlimit",
			gcPercent: 55, gcOK: true, gcSource: "persisted urnet-tools set gogc",
			want: "[proxy][resources] Go soft memory limit 512 MiB in force (source: persisted urnet-tools set gomemlimit); " +
				"GOGC 55 (source: persisted urnet-tools set gogc)",
		},
		{
			name: "tier defaults", limit: 1024 * testMiB, limitSource: "profile tier eco",
			gcPercent: 50, gcOK: true, gcSource: "profile tier eco",
			want: "[proxy][resources] Go soft memory limit 1024 MiB in force (source: profile tier eco); " +
				"GOGC 50 (source: profile tier eco)",
		},
		{
			name: "gc disabled", limit: 512 * testMiB, limitSource: goMemLimitSourcePlan,
			gcPercent: -1, gcOK: true, gcSource: "persisted urnet-tools set gogc",
			want: "[proxy][resources] Go soft memory limit 512 MiB in force (source: memory plan); " +
				"GOGC off, collection disabled (source: persisted urnet-tools set gogc)",
		},
		{
			name: "gc unknown", limit: 512 * testMiB, limitSource: goMemLimitSourcePlan,
			gcPercent: 0, gcOK: false, gcSource: goGCSourceDefault,
			want: "[proxy][resources] Go soft memory limit 512 MiB in force (source: memory plan); " +
				"GOGC unknown (source: runtime default)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line := goRuntimeTuningLogLine(c.limit, c.limitSource, c.gcPercent, c.gcOK, c.gcSource)
			if line != c.want {
				t.Errorf("goRuntimeTuningLogLine() = %q, want %q", line, c.want)
			}
			if !isImportantLogLine("0928 12:00:00 " + line) {
				t.Errorf("startup line %q is not classified important, so it misses events.log", line)
			}
		})
	}
}
