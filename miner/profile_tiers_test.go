package miner

import (
	"runtime/debug"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// preserveRuntimeTuning snapshots the process-wide Go runtime knobs the
// profile tiers write and restores them when the test ends, so one test's
// GOMEMLIMIT/GOGC cannot leak into the rest of the package's tests.
func preserveRuntimeTuning(t *testing.T) {
	t.Helper()
	previousLimit := debug.SetMemoryLimit(-1) // negative leaves the value unchanged
	t.Cleanup(func() {
		debug.SetMemoryLimit(previousLimit)
		debug.SetGCPercent(100)
	})
}

// withControlState installs a fresh in-memory control state for the duration
// of the test, so persistedRuntimeTuningActive sees exactly what the test
// sets and nothing else.
func withControlState(t *testing.T) *controlState {
	t.Helper()
	previous := globalControlState
	state := newControlState()
	globalControlState = state
	t.Cleanup(func() { globalControlState = previous })
	return state
}

// clearTuningEnv removes GOMEMLIMIT/GOGC/URNETWORK_PROFILE for the test.
func clearTuningEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GOMEMLIMIT", "")
	t.Setenv("GOGC", "")
	t.Setenv("URNETWORK_PROFILE", "")
}

func TestCanonicalProfileName(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"eco":       "eco",
		"lowmem":    "lowmem",
		"auto":      "auto",
		"v4":        "turbo-v4",
		"V4":        "turbo-v4",
		"v8":        "turbo-v8",
		"turbo-v4":  "turbo-v4",
		" turbo-v8": "turbo-v8",
		"Turbo-V8":  "turbo-v8",
	}
	for in, want := range cases {
		if got := canonicalProfileName(in); got != want {
			t.Errorf("canonicalProfileName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A raw env alias must resolve the same way the socket-set alias does: the
// socket stores turbo-v4, so URNETWORK_PROFILE=v4 has to select turbo-v4 too.
func TestEffectiveProfileCanonicalizesEnvAlias(t *testing.T) {
	t.Setenv("URNETWORK_PROFILE", "v4")
	if got := effectiveProfile(); got != profileTurboV4 {
		t.Fatalf("effectiveProfile() = %q, want %q", got, profileTurboV4)
	}
	t.Setenv("URNETWORK_PROFILE", "")
	if got := effectiveProfile(); got != "" {
		t.Fatalf("effectiveProfile() = %q, want empty", got)
	}
}

func TestProfileTierQueries(t *testing.T) {
	for _, profile := range []string{profileLowmem, profileEco} {
		if !profileWantsRamlogs(profile) {
			t.Errorf("profileWantsRamlogs(%q) = false, want true", profile)
		}
	}
	for _, profile := range []string{"", profileAuto, profileTurboV4, profileTurboV8} {
		if profileWantsRamlogs(profile) {
			t.Errorf("profileWantsRamlogs(%q) = true, want false", profile)
		}
	}
	if !profileBenchmarksDisk(profileAuto) {
		t.Error("only auto may benchmark the disk")
	}
	for _, profile := range []string{"", profileEco, profileLowmem, profileTurboV4, profileTurboV8} {
		if profileBenchmarksDisk(profile) {
			t.Errorf("profileBenchmarksDisk(%q) = true, want false", profile)
		}
	}
}

func TestApplyLowmodeSettings(t *testing.T) {
	clearTuningEnv(t)
	t.Setenv("URNETWORK_PROFILE", profileLowmem)

	settings := sdk.DefaultDeviceLocalSettings()
	applyLowmodeSettings(settings)

	if got := settings.ClientSettings.ContractManagerSettings.InitialContractTransferByteCount; got != 256*1024 {
		t.Errorf("initial contract = %d, want %d", got, 256*1024)
	}
	if got := settings.SequenceBufferSize; got != 16 {
		t.Errorf("SequenceBufferSize = %d, want 16", got)
	}

	// No other profile changes anything.
	other := sdk.DefaultDeviceLocalSettings()
	wantSequence := other.SequenceBufferSize
	t.Setenv("URNETWORK_PROFILE", profileEco)
	applyLowmodeSettings(other)
	if other.SequenceBufferSize != wantSequence {
		t.Errorf("eco changed SequenceBufferSize from %d to %d", wantSequence, other.SequenceBufferSize)
	}
}

func TestApplyTurboSettings(t *testing.T) {
	clearTuningEnv(t)
	preserveRuntimeTuning(t)

	for _, tc := range []struct {
		profile     string
		windowBytes connect.ByteCount
		queueBytes  connect.ByteCount
	}{
		{profileTurboV4, 4 * 1024 * 1024, 8 * 1024 * 1024},
		{profileTurboV8, 8 * 1024 * 1024, 16 * 1024 * 1024},
	} {
		t.Setenv("URNETWORK_PROFILE", tc.profile)
		settings := sdk.DefaultDeviceLocalSettings()
		applyTurboSettings(settings)

		if got := settings.ClientSettings.SendBufferSettings.ResendQueueMaxByteCount; got != tc.queueBytes {
			t.Errorf("%s: resend queue = %d, want %d", tc.profile, got, tc.queueBytes)
		}
		if got := settings.ClientSettings.ReceiveBufferSettings.ReceiveQueueMaxByteCount; got != tc.queueBytes {
			t.Errorf("%s: receive queue = %d, want %d", tc.profile, got, tc.queueBytes)
		}
		if got := settings.ClientSettings.WebRtcSettings.ReceiveBufferSize; got != tc.windowBytes*2 {
			t.Errorf("%s: webrtc receive buffer = %d, want %d", tc.profile, got, tc.windowBytes*2)
		}
		if got := settings.SequenceBufferSize; got != 512 {
			t.Errorf("%s: SequenceBufferSize = %d, want 512", tc.profile, got)
		}
	}

	// The per-provider constructor must not touch GOGC: the tier's GC default
	// is process-wide and installed once by applyProfileGCPercent, so a
	// constructor that wrote it would re-apply the tier value for every new
	// provider and after the startup line that reports the value in force.
	withControlState(t)
	t.Setenv("URNETWORK_PROFILE", profileTurboV4)
	debug.SetGCPercent(100)
	applyTurboSettings(sdk.DefaultDeviceLocalSettings())
	if got := debug.SetGCPercent(100); got != 100 {
		t.Errorf("applyTurboSettings changed GOGC to %d; the process-wide tier knob belongs to applyProfileGCPercent", got)
	}
}

// The tier's GOGC default is 200 (turbo) / 50 (eco) unless the operator pinned
// GOGC in the environment or a persisted gogc is in force.
func TestApplyProfileGCPercent(t *testing.T) {
	cases := []struct {
		profile string
		want    int
	}{
		{profileEco, 50},
		{profileTurboV4, 200},
		{profileTurboV8, 200},
		{profileLowmem, 100}, // no tier default: the runtime value stands
		{profileAuto, 100},
		{"", 100},
	}
	for _, c := range cases {
		t.Run("tier "+c.profile, func(t *testing.T) {
			clearTuningEnv(t)
			preserveRuntimeTuning(t)
			withControlState(t)
			t.Setenv("URNETWORK_PROFILE", c.profile)
			debug.SetGCPercent(100)

			installed := applyProfileGCPercent(c.profile)
			if got := debug.SetGCPercent(100); got != c.want {
				t.Errorf("%s: GOGC = %d, want %d", c.profile, got, c.want)
			}
			if wantInstalled := c.want != 100; installed != wantInstalled {
				t.Errorf("%s: applyProfileGCPercent reported %v, want %v", c.profile, installed, wantInstalled)
			}
		})
	}
}

func TestApplyProfileGCPercentDefersToOperatorGOGC(t *testing.T) {
	clearTuningEnv(t)
	preserveRuntimeTuning(t)
	withControlState(t)

	t.Setenv("URNETWORK_PROFILE", profileTurboV4)
	t.Setenv("GOGC", "37")
	debug.SetGCPercent(100)
	if applyProfileGCPercent(profileTurboV4) {
		t.Errorf("applyProfileGCPercent installed a tier default over GOGC from the environment")
	}
	if got := debug.SetGCPercent(100); got != 100 {
		t.Errorf("GOGC env must win: got %d, want 100", got)
	}

	t.Setenv("GOGC", "")
	globalControlState.set("gogc", "55")
	debug.SetGCPercent(100)
	if applyProfileGCPercent(profileTurboV4) {
		t.Errorf("applyProfileGCPercent installed a tier default over a persisted gogc")
	}
	if got := debug.SetGCPercent(100); got != 100 {
		t.Errorf("persisted gogc must win: got %d, want 100", got)
	}
}

func TestApplyEcoSettings(t *testing.T) {
	clearTuningEnv(t)
	preserveRuntimeTuning(t)
	withControlState(t)

	// Without a persisted value the tier sets a 75% RAM limit. Its GOGC 50
	// default is process-wide and covered by TestApplyProfileGCPercent.
	t.Setenv("URNETWORK_PROFILE", profileEco)
	debug.SetMemoryLimit(999999999999)
	applyEcoSettings(0)
	if got := debug.SetMemoryLimit(-1); got == 999999999999 {
		t.Errorf("eco must set a memory limit")
	}

	// A persisted gomemlimit wins over the eco default.
	globalControlState.set("gomemlimit", "512MiB")
	debug.SetMemoryLimit(999999999999)
	applyEcoSettings(0)
	if got := debug.SetMemoryLimit(-1); got != 999999999999 {
		t.Errorf("persisted gomemlimit must win: got %d", got)
	}

	// --max-memory wins too.
	globalControlState.clear("gomemlimit")
	debug.SetMemoryLimit(999999999999)
	applyEcoSettings(64 * 1024 * 1024)
	if got := debug.SetMemoryLimit(-1); got != 999999999999 {
		t.Errorf("--max-memory must win: got %d", got)
	}
}

func TestApplyTurboMemoryLimit(t *testing.T) {
	clearTuningEnv(t)
	preserveRuntimeTuning(t)
	withControlState(t)

	debug.SetMemoryLimit(999999999999)
	applyTurboMemoryLimit(profileTurboV4, 0)
	if got := debug.SetMemoryLimit(-1); got == 999999999999 {
		t.Error("turbo must set a memory limit")
	}

	globalControlState.set("gomemlimit", "512MiB")
	debug.SetMemoryLimit(999999999999)
	applyTurboMemoryLimit(profileTurboV8, 0)
	if got := debug.SetMemoryLimit(-1); got != 999999999999 {
		t.Errorf("persisted gomemlimit must win: got %d", got)
	}

	globalControlState.clear("gomemlimit")
	debug.SetMemoryLimit(999999999999)
	applyTurboMemoryLimit(profileTurboV4, 64*1024*1024)
	if got := debug.SetMemoryLimit(-1); got != 999999999999 {
		t.Errorf("--max-memory must win: got %d", got)
	}

	// The limit only applies to the turbo tiers.
	debug.SetMemoryLimit(999999999999)
	applyTurboMemoryLimit(profileEco, 0)
	if got := debug.SetMemoryLimit(-1); got != 999999999999 {
		t.Errorf("eco must not take the turbo limit: got %d", got)
	}
}

// ensureMemoryLimit only acts when nothing else set a finite limit; the miner
// normally runs under the provider memory plan, so the common path is a no-op.
func TestEnsureMemoryLimitLeavesAnExistingFiniteLimit(t *testing.T) {
	clearTuningEnv(t)
	preserveRuntimeTuning(t)

	debug.SetMemoryLimit(123456789)
	ensureMemoryLimit(0)
	if got := debug.SetMemoryLimit(-1); got != 123456789 {
		t.Errorf("existing finite limit must stay: got %d", got)
	}
}

func TestApplyProfileDeviceSettingsSelectsTier(t *testing.T) {
	clearTuningEnv(t)
	preserveRuntimeTuning(t)

	t.Setenv("URNETWORK_PROFILE", profileLowmem)
	lowmem := sdk.DefaultDeviceLocalSettings()
	applyProfileDeviceSettings(lowmem)
	if lowmem.SequenceBufferSize != 16 {
		t.Errorf("lowmem SequenceBufferSize = %d, want 16", lowmem.SequenceBufferSize)
	}

	t.Setenv("URNETWORK_PROFILE", profileTurboV8)
	turbo := sdk.DefaultDeviceLocalSettings()
	applyProfileDeviceSettings(turbo)
	if turbo.ClientSettings.SendBufferSettings.ResendQueueMaxByteCount != 16*1024*1024 {
		t.Errorf("turbo-v8 resend queue = %d", turbo.ClientSettings.SendBufferSettings.ResendQueueMaxByteCount)
	}
}
