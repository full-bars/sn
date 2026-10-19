package miner

import (
	"testing"

	"github.com/urnetwork/connect"
)

// h3_mode_test.go ports the v33 provider's h3_mode_test.go plus the mode half
// of h3_direct_test.go. The gate half of the v33 test does not apply: the
// pinned full-bars/connect in this tree carries no process-wide H3 switch, so
// the mode is applied at transport build time and a change relaunches the
// identities it affects (see h3_mode_apply.go).

func TestParseH3Mode(t *testing.T) {
	for value, want := range map[string]h3Setting{
		"": {Mode: h3ModeDirect}, "direct": {Mode: h3ModeDirect}, "DIRECT": {Mode: h3ModeDirect},
		"off": {Mode: h3ModeOff}, "0": {Mode: h3ModeOff},
		"on": {Mode: h3ModeOn}, " On ": {Mode: h3ModeOn},
		"all": {Mode: h3ModeAll}, "16": {Mode: h3ModeTop, Cap: 16}, " 64 ": {Mode: h3ModeTop, Cap: 64},
	} {
		got, err := parseH3Mode(value)
		if err != nil || got != want {
			t.Fatalf("parseH3Mode(%q) = %v, %v; want %v", value, got, err, want)
		}
	}
	for _, bad := range []string{"maybe", "true", "-1", "+4", "00", "04", "1.5", "all proxies"} {
		if _, err := parseH3Mode(bad); err == nil {
			t.Fatalf("parseH3Mode(%q) must be rejected", bad)
		}
	}
	for _, value := range []string{"off", "direct", "on", "all", "16"} {
		got, _ := parseH3Mode(value)
		if got.String() != value {
			t.Fatalf("%q does not round trip, got %q", value, got.String())
		}
	}
}

func TestH3EnabledParsesTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false}, {"off", false}, {"0", false}, {"false", false}, {"no", false}, {"garbage", false},
		{"on", true}, {"ON", true}, {" 1 ", true}, {"true", true}, {"Yes", true},
	} {
		t.Setenv("URNETWORK_H3", tc.value)
		if got := h3Enabled(); got != tc.want {
			t.Fatalf("URNETWORK_H3=%q: h3Enabled=%v, want %v", tc.value, got, tc.want)
		}
	}
}

// The default: the direct identity runs every upstream carrier as an automatic
// fallback behind H1, and no identity is the beside-H1 auxiliary (which the
// pinned connect expresses as H3 at H1's own tier, see h3ModePreferences).
func TestDirectIdentityDefaultsToAutomaticFallbacks(t *testing.T) {
	t.Setenv("URNETWORK_H3", "")
	preferences := h3ModePreferences(h3Setting{Mode: h3ModeDirect}, true, false)
	want := connect.DefaultTransportModePreferences()
	if len(preferences) != len(want) {
		t.Fatalf("direct modes = %v, want upstream's %v", preferences, want)
	}
	for mode, priority := range want {
		if preferences[mode] != priority {
			t.Fatalf("mode %s priority %d, want %d", mode, preferences[mode], priority)
		}
	}
	if !(preferences[connect.TransportModeH1] < preferences[connect.TransportModeH3]) {
		t.Fatal("H1 must be the primary: H3 is a fallback unless the auxiliary is on")
	}
}

func TestH3ModePreferencesPerMode(t *testing.T) {
	for _, mode := range []h3Mode{h3ModeOff, h3ModeDirect, h3ModeOn, h3ModeTop, h3ModeAll} {
		got := h3ModePreferences(h3Setting{Mode: mode, Cap: 4}, false, false)
		if len(got) != 1 || got[connect.TransportModeH1] != 1 {
			t.Fatalf("mode %v: an ineligible proxy must be H1 only, got %v", mode, got)
		}
	}
	for _, mode := range []h3Mode{h3ModeOff} {
		if got := h3ModePreferences(h3Setting{Mode: mode}, true, true); len(got) != 1 || got[connect.TransportModeH1] != 1 {
			t.Fatalf("off: every identity must be H1 only, got %v", got)
		}
	}
	// the fallbacks, for the direct identity in every mode but off, and for an
	// eligible proxy
	want := connect.DefaultTransportModePreferences()
	for _, c := range []struct {
		name          string
		setting       h3Setting
		direct, proxy bool
	}{
		{"direct", h3Setting{Mode: h3ModeDirect}, true, false},
		{"top direct", h3Setting{Mode: h3ModeTop, Cap: 2}, true, false},
		{"top proxy", h3Setting{Mode: h3ModeTop, Cap: 2}, false, true},
		{"all proxy", h3Setting{Mode: h3ModeAll}, false, true},
	} {
		got := h3ModePreferences(c.setting, c.direct, c.proxy)
		for mode, priority := range want {
			if got[mode] != priority {
				t.Fatalf("%s: mode %s priority %d, want %d", c.name, mode, got[mode], priority)
			}
		}
	}
	on := h3ModePreferences(h3Setting{Mode: h3ModeOn}, true, false)
	if on[connect.TransportModeH3] != on[connect.TransportModeH1] {
		t.Fatalf("on: H3 must share H1's tier (auxiliary), got %v", on)
	}
	for _, mode := range []connect.TransportMode{connect.TransportModeH3Dns, connect.TransportModeH3DnsPump} {
		if on[mode] <= on[connect.TransportModeH1] {
			t.Fatalf("on: %s must stay a fallback, got %v", mode, on)
		}
	}
	// a proxy is never the auxiliary
	if got := h3ModePreferences(h3Setting{Mode: h3ModeAll}, false, true); got[connect.TransportModeH3] <= got[connect.TransportModeH1] {
		t.Fatalf("a proxy must not run H3 beside H1, got %v", got)
	}
}

func TestResolveH3Mode(t *testing.T) {
	t.Setenv("URNETWORK_H3", "")
	if got := resolveH3Mode(nil, proxyResolutionNoSource); got.Mode != h3ModeDirect {
		t.Fatalf("unset on a pooled node = %v, want direct", got)
	}
	if got := resolveH3Mode(nil, proxyResolutionZeroValid); got.Mode != h3ModeOn {
		t.Fatalf("unset on a settled direct-only node = %v, want on (the existing default)", got)
	}
	t.Setenv("URNETWORK_H3", "on")
	if got := resolveH3Mode(nil, proxyResolutionNoSource); got.Mode != h3ModeOn {
		t.Fatalf("URNETWORK_H3=on = %v, want on", got)
	}
	state := newControlState()
	for _, c := range []struct {
		value string
		want  h3Mode
	}{{"off", h3ModeOff}, {"direct", h3ModeDirect}, {"on", h3ModeOn}, {"all", h3ModeAll}, {"8", h3ModeTop}} {
		if err := state.set("h3", c.value); err != nil {
			t.Fatal(err)
		}
		if got := resolveH3Mode(state, proxyResolutionZeroValid); got.Mode != c.want {
			t.Fatalf("explicit %q = %v, want %v", c.value, got, c.want)
		}
	}
}

// A change of setting reports a relaunch of the direct identity when its own
// policy changed; the startup replay does not.
func TestApplyH3ModeRelaunchesOnlyOnAChange(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	t.Setenv("URNETWORK_H3", "")
	h3AppliedSetting.Store(nil)
	t.Cleanup(func() { h3AppliedSetting.Store(nil) })
	direct := h3Setting{Mode: h3ModeDirect}
	if _, relaunch := applyH3Mode(direct); relaunch {
		t.Fatal("the first application is the startup replay and must not relaunch")
	}
	if _, relaunch := applyH3Mode(direct); relaunch {
		t.Fatal("an unchanged mode must not relaunch")
	}
	if previous, relaunch := applyH3Mode(h3Setting{Mode: h3ModeOn}); !relaunch || previous != direct {
		t.Fatalf("direct -> on: previous=%v relaunch=%v", previous, relaunch)
	}
	if _, relaunch := applyH3Mode(h3Setting{Mode: h3ModeOff}); !relaunch {
		t.Fatal("on -> off must relaunch the direct identity")
	}
	// widening to proxies leaves the direct identity's policy alone
	applyH3Mode(direct)
	if _, relaunch := applyH3Mode(h3Setting{Mode: h3ModeAll}); relaunch {
		t.Fatal("direct -> all must not relaunch the direct identity")
	}
	if _, relaunch := applyH3Mode(h3Setting{Mode: h3ModeTop, Cap: 4}); relaunch {
		t.Fatal("all -> 4 must not relaunch the direct identity")
	}
}

// The h3 key reaches the running miner: it is a live key, the value grammar is
// the reference one, a set applies at once, and a clear hands the decision
// back to the startup default.
func TestH3ControlKeyDrivesTheLiveMode(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	t.Setenv("URNETWORK_H3", "")
	h3AppliedSetting.Store(nil)
	t.Cleanup(func() { h3AppliedSetting.Store(nil) })

	if controlNeedsRestart("h3") {
		t.Fatal("h3 is live: controlNeedsRestart must be false")
	}
	for _, bad := range []string{"maybe", "", "-1", "true"} {
		if err := validateControlValue("h3", bad); err == nil {
			t.Fatalf("h3=%q must be rejected", bad)
		}
	}
	for _, ok := range []string{"on", "off", "ON", "direct", "all", "16"} {
		if err := validateControlValue("h3", ok); err != nil {
			t.Fatalf("h3=%q: %v", ok, err)
		}
	}

	if err := applyControlLiveSideEffect("h3", "on"); err != nil {
		t.Fatalf("set on: %v", err)
	}
	if got := h3AppliedSetting.Load(); got == nil || got.Mode != h3ModeOn {
		t.Fatalf("set on applied %+v, want on", got)
	}
	if err := applyControlLiveSideEffect("h3", "off"); err != nil {
		t.Fatalf("set off: %v", err)
	}
	if got := h3AppliedSetting.Load(); got == nil || got.Mode != h3ModeOff {
		t.Fatalf("set off applied %+v, want off", got)
	}
	// clear returns to the startup default, not to a hardcoded value
	t.Setenv("URNETWORK_H3", "on")
	if err := applyControlLiveDefault("h3"); err != nil {
		t.Fatalf("clear with env on: %v", err)
	}
	if got := h3AppliedSetting.Load(); got == nil || got.Mode != h3ModeOn {
		t.Fatalf("clear with env on applied %+v, want on", got)
	}
	t.Setenv("URNETWORK_H3", "")
	if err := applyControlLiveDefault("h3"); err != nil {
		t.Fatalf("clear with env off: %v", err)
	}
	if got := h3AppliedSetting.Load(); got == nil || got.Mode != h3ModeDirect {
		t.Fatalf("clear with env off applied %+v, want direct", got)
	}
}

// The datagram keys are accepted for client compatibility but drive nothing:
// the pinned upstream connect offers and sends QUIC DATAGRAM by default and
// carries no runtime switch, so the miner reports the always-on upstream
// default instead of pretending to toggle it.
func TestDatagramKeysReportTheUpstreamDefault(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	for _, key := range []string{"h3_datagram", "h3_datagram_send"} {
		if controlNeedsRestart(key) {
			t.Fatalf("%s is accepted and has no restart-gated effect: controlNeedsRestart must be false", key)
		}
		if err := applyControlLiveSideEffect(key, "off"); err != nil {
			t.Fatalf("%s=off: %v", key, err)
		}
		if err := applyControlLiveDefault(key); err != nil {
			t.Fatalf("clear %s: %v", key, err)
		}
	}
	if settings := connect.DefaultPlatformTransportSettings(); !settings.EnableH3Datagrams {
		t.Fatal("the pinned connect default must offer QUIC datagrams; the miner keeps the upstream default")
	}
}
