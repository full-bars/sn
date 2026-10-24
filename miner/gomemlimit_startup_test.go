package miner

import (
	"math"
	"runtime/debug"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
)

// gomemlimit_startup_test.go pins the startup replay of a persisted
// `urnet-tools set gomemlimit` and the one line that names the soft limit in
// force.
//
// The bug this covers: the control socket applies gomemlimit live and every
// profile tier skips its default while persistedRuntimeTuningActive reports the
// key as set, but the miner had no startup replay. After a restart the runtime
// ran under the memory plan's default while the tiers kept skipping, so the
// plan silently beat an explicit pin. The tests drive applyStartupMemoryTuning
// - the one function run.go calls for this - with the memory plan installed
// first, exactly as run.go does before the control state is loaded.

// recordStartupLines captures the important-startup lines for one start,
// including the soft-limit source line, and resets the once-per-start warning
// gate so the next test starts clean.
func recordStartupLines(t *testing.T) *[]string {
	t.Helper()
	lines := &[]string{}
	oldHook := importantLogHook
	importantLogHook = func(line string) { *lines = append(*lines, line) }
	t.Cleanup(func() {
		importantLogHook = oldHook
		resourceConfigWarnOnce = sync.Once{}
	})
	resourceConfigWarnOnce = sync.Once{}
	return lines
}

// goMemLimitSourceLine returns the startup line naming the soft limit in
// force. There must be exactly one: a second would mean two sites report the
// limit and could disagree.
func goMemLimitSourceLine(t *testing.T, lines []string) string {
	t.Helper()
	var found []string
	for _, line := range lines {
		if strings.Contains(line, "Go soft memory limit") {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d soft-limit lines %q, want exactly one (all lines: %q)", len(found), found, lines)
	}
	return found[0]
}

// installStartupMemoryPlan installs the provide path's memory plan the way
// run.go does before the control state is loaded, and restores the process
// budget the plan writes.
func installStartupMemoryPlan(t *testing.T, plan providerMemoryPlan) {
	t.Helper()
	previousBudget := connect.MemoryBudget()
	t.Cleanup(func() { connect.SetMemoryBudget(previousBudget) })
	applyProviderProcessMemory(plan)
}

// A persisted pin must be in force after the start, and the tier that would
// otherwise install its own default must skip for it. The eco tier is the
// sharpest case: without the replay it installs 75% of RAM over the plan,
// which would hide the bug the pin is here to prevent.
func TestStartupAppliesPersistedGoMemLimitAndTheTierSkips(t *testing.T) {
	preserveRuntimeTuning(t)
	state := withControlState(t)
	clearTuningEnv(t)
	t.Setenv("URNETWORK_PROFILE", profileEco)

	if err := state.set("gomemlimit", "512MiB"); err != nil {
		t.Fatalf("set gomemlimit: %v", err)
	}
	// run.go installs the plan first; its default must not survive the pin.
	installStartupMemoryPlan(t, newProviderMemoryPlan(0, 8*testGib, 4))
	if got := debug.SetMemoryLimit(-1); got == 512*testMiB {
		t.Fatalf("the plan already installed the pinned value %d; the test cannot tell the replay from the plan", got)
	}

	lines := recordStartupLines(t)
	applyStartupMemoryTuning(0, 4)

	if got := debug.SetMemoryLimit(-1); got != 512*testMiB {
		t.Fatalf("GOMEMLIMIT after startup = %d, want the persisted 512 MiB (%d)", got, 512*testMiB)
	}
	source := goMemLimitSourceLine(t, *lines)
	if !strings.Contains(source, "512 MiB") || !strings.Contains(source, "persisted urnet-tools set gomemlimit") {
		t.Fatalf("startup line %q must name the pinned value and the persisted source", source)
	}
	if !isImportantLogLine("0928 12:00:00 " + source) {
		t.Fatalf("startup line %q is not classified important, so it misses events.log", source)
	}
}

// GOMEMLIMIT in the environment still outranks the persisted value: the runtime
// installed it before main, the plan skipped for it, and the replay must leave
// it alone rather than downgrade the pin that wins.
func TestStartupEnvGoMemLimitOutranksThePersistedValue(t *testing.T) {
	preserveRuntimeTuning(t)
	state := withControlState(t)
	clearTuningEnv(t)
	t.Setenv("URNETWORK_PROFILE", profileEco)

	// A test cannot re-run runtime init, so install what GOMEMLIMIT=400MiB
	// installs at boot.
	t.Setenv("GOMEMLIMIT", "400MiB")
	debug.SetMemoryLimit(400 * testMiB)

	if err := state.set("gomemlimit", "512MiB"); err != nil {
		t.Fatalf("set gomemlimit: %v", err)
	}
	installStartupMemoryPlan(t, newProviderMemoryPlan(0, 8*testGib, 4))

	lines := recordStartupLines(t)
	applyStartupMemoryTuning(0, 4)

	if got := debug.SetMemoryLimit(-1); got != 400*testMiB {
		t.Fatalf("GOMEMLIMIT after startup = %d, want the environment's 400 MiB (%d)", got, 400*testMiB)
	}
	source := goMemLimitSourceLine(t, *lines)
	if !strings.Contains(source, "400 MiB") || !strings.Contains(source, "env GOMEMLIMIT") {
		t.Fatalf("startup line %q must name the environment value and the env source", source)
	}
}

// With no pin at all the existing behaviour must be untouched: the plan's
// default on a node with no tier, the tier's own default where a tier applies.
func TestStartupWithoutAPinKeepsThePlanAndTierDefaults(t *testing.T) {
	t.Run("plan default", func(t *testing.T) {
		preserveRuntimeTuning(t)
		withControlState(t)
		clearTuningEnv(t)

		plan := newProviderMemoryPlan(0, 8*testGib, 4)
		installStartupMemoryPlan(t, plan)

		lines := recordStartupLines(t)
		applyStartupMemoryTuning(0, 4)

		if got := debug.SetMemoryLimit(-1); got != int64(plan.SoftLimitByteCount) {
			t.Fatalf("GOMEMLIMIT after startup = %d, want the plan's %d", got, plan.SoftLimitByteCount)
		}
		source := goMemLimitSourceLine(t, *lines)
		if !strings.Contains(source, "memory plan") {
			t.Fatalf("startup line %q must name the memory plan as the source", source)
		}
	})

	t.Run("profile tier", func(t *testing.T) {
		preserveRuntimeTuning(t)
		withControlState(t)
		clearTuningEnv(t)
		t.Setenv("URNETWORK_PROFILE", profileEco)

		ram := detectEffectiveRAMLimitBytes()
		if ram <= 0 {
			t.Skip("effective RAM is unreadable; the eco tier has no limit to install")
		}
		installStartupMemoryPlan(t, newProviderMemoryPlan(0, 8*testGib, 4))

		lines := recordStartupLines(t)
		applyStartupMemoryTuning(0, 4)

		if got, want := debug.SetMemoryLimit(-1), ram*75/100; got != want {
			t.Fatalf("GOMEMLIMIT after startup = %d, want the eco tier's %d", got, want)
		}
		source := goMemLimitSourceLine(t, *lines)
		if !strings.Contains(source, "profile tier "+profileEco) {
			t.Fatalf("startup line %q must name the eco tier as the source", source)
		}
	})
}

// A cleared key and the value 0 are not the same thing: "off" is the clear
// value and must leave the plan in force, while 0 is the operator asking for
// no soft limit at all - and the generic fallback must not reverse it.
func TestStartupPersistedGoMemLimitOffAndZero(t *testing.T) {
	t.Run("off is not a pin", func(t *testing.T) {
		preserveRuntimeTuning(t)
		state := withControlState(t)
		clearTuningEnv(t)

		if err := state.set("gomemlimit", "off"); err != nil {
			t.Fatalf("set gomemlimit: %v", err)
		}
		plan := newProviderMemoryPlan(0, 8*testGib, 4)
		installStartupMemoryPlan(t, plan)

		lines := recordStartupLines(t)
		applyStartupMemoryTuning(0, 4)

		if got := debug.SetMemoryLimit(-1); got != int64(plan.SoftLimitByteCount) {
			t.Fatalf("GOMEMLIMIT after startup = %d, want the plan's %d", got, plan.SoftLimitByteCount)
		}
		if source := goMemLimitSourceLine(t, *lines); !strings.Contains(source, "memory plan") {
			t.Fatalf("startup line %q must name the memory plan as the source", source)
		}
	})

	t.Run("zero means no soft limit", func(t *testing.T) {
		preserveRuntimeTuning(t)
		state := withControlState(t)
		clearTuningEnv(t)

		if err := state.set("gomemlimit", "0"); err != nil {
			t.Fatalf("set gomemlimit: %v", err)
		}
		installStartupMemoryPlan(t, newProviderMemoryPlan(0, 8*testGib, 4))

		lines := recordStartupLines(t)
		applyStartupMemoryTuning(0, 4)

		if got := debug.SetMemoryLimit(-1); got != math.MaxInt64 {
			t.Fatalf("GOMEMLIMIT after startup = %d, want unlimited (%d)", got, int64(math.MaxInt64))
		}
		source := goMemLimitSourceLine(t, *lines)
		if !strings.Contains(source, "unlimited") || !strings.Contains(source, "persisted urnet-tools set gomemlimit") {
			t.Fatalf("startup line %q must say unlimited and name the persisted source", source)
		}
	})
}

// A hand-edited, unparseable value must not be applied and must not be
// reported as the pin in force: the plan's limit stands, and the line says so.
// Silence here would leave an operator believing a pin is set.
func TestStartupUnparseablePersistedGoMemLimitLeavesThePlanInForce(t *testing.T) {
	preserveRuntimeTuning(t)
	state := withControlState(t)
	clearTuningEnv(t)
	t.Setenv("URNETWORK_PROFILE", profileEco)

	if err := state.set("gomemlimit", "half-a-gib"); err != nil {
		t.Fatalf("set gomemlimit: %v", err)
	}
	plan := newProviderMemoryPlan(0, 8*testGib, 4)
	installStartupMemoryPlan(t, plan)

	lines := recordStartupLines(t)
	applyStartupMemoryTuning(0, 4)

	if got := debug.SetMemoryLimit(-1); got != int64(plan.SoftLimitByteCount) {
		t.Fatalf("GOMEMLIMIT after startup = %d, want the plan's %d (a bad value must not be applied)", got, plan.SoftLimitByteCount)
	}
	var complained bool
	for _, line := range *lines {
		if strings.Contains(line, "is not a byte count") && strings.Contains(line, "half-a-gib") {
			complained = true
		}
	}
	if !complained {
		t.Fatalf("no line explains that the persisted value was ignored, got %q", *lines)
	}
	if source := goMemLimitSourceLine(t, *lines); !strings.Contains(source, "memory plan") {
		t.Fatalf("startup line %q must name the memory plan, not a pin that was never applied", source)
	}
}

// The line is the whole point of the change for the operator: it has to be
// readable and unambiguous on its own.
func TestGoMemLimitLogLine(t *testing.T) {
	cases := []struct {
		limit  int64
		source string
		want   string
	}{
		{512 * testMiB, "persisted urnet-tools set gomemlimit",
			"[proxy][resources] Go soft memory limit 512 MiB in force (source: persisted urnet-tools set gomemlimit)"},
		{400 * testMiB, "env GOMEMLIMIT",
			"[proxy][resources] Go soft memory limit 400 MiB in force (source: env GOMEMLIMIT)"},
		{ // the plan default on a box whose RAM is unknown
			192 * testMiB, goMemLimitSourcePlan,
			"[proxy][resources] Go soft memory limit 192 MiB in force (source: memory plan)"},
		{1024 * testMiB, "profile tier eco",
			"[proxy][resources] Go soft memory limit 1024 MiB in force (source: profile tier eco)"},
		{math.MaxInt64, "env GOMEMLIMIT",
			"[proxy][resources] Go soft memory limit unlimited (source: env GOMEMLIMIT)"},
	}
	for _, c := range cases {
		if got := goMemLimitLogLine(c.limit, c.source); got != c.want {
			t.Errorf("goMemLimitLogLine(%d, %q) = %q, want %q", c.limit, c.source, got, c.want)
		}
	}
}
