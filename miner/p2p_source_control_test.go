package miner

import (
	"path/filepath"
	"testing"

	connect "github.com/urnetwork/connect"
)

// Both keys are settings the control socket must accept, apply live, and
// report as live: the mode is read by connect on the next frame and the drop
// switch is a process flag, so neither needs a restart.
func TestP2PSourceControlKeys(t *testing.T) {
	for _, tc := range []struct {
		key  string
		good []string
		bad  string
	}{
		{"p2p_source_binding", []string{"off", "count", "drop"}, "on"},
		{"p2p_forged_source_drop", []string{"on", "off"}, "count"},
	} {
		if controlNeedsRestart(tc.key) {
			t.Errorf("%s must be live", tc.key)
		}
		for _, v := range tc.good {
			if err := validateControlValue(tc.key, v); err != nil {
				t.Errorf("%s=%s: %v", tc.key, v, err)
			}
		}
		if err := validateControlValue(tc.key, tc.bad); err == nil {
			t.Errorf("%s=%s accepted", tc.key, tc.bad)
		}
	}
}

// A persisted value is replayed before any provider builds a transport, and
// clearing the key restores the library default rather than a value written in
// this package.
func TestP2PSourceControlRestore(t *testing.T) {
	t.Cleanup(func() {
		_ = connect.SetP2PSourceBinding(connect.P2PSourceBindingDefault())
		connect.SetP2PForgedSourceDrop(true)
	})
	state := newControlState()
	if err := state.set("p2p_source_binding", "count"); err != nil {
		t.Fatal(err)
	}
	if err := state.set("p2p_forged_source_drop", "off"); err != nil {
		t.Fatal(err)
	}
	replayP2PSourcePolicyAtStartup(state)
	if got := connect.P2PSourceBinding(); got != "count" {
		t.Fatalf("mode=%s", got)
	}
	if connect.P2PForgedSourceDropEnabled() {
		t.Fatal("switch still on")
	}
	if err := applyControlLiveDefault("p2p_source_binding"); err != nil {
		t.Fatal(err)
	}
	if err := applyControlLiveDefault("p2p_forged_source_drop"); err != nil {
		t.Fatal(err)
	}
	if connect.P2PSourceBinding() != connect.P2PSourceBindingDefault() || !connect.P2PForgedSourceDropEnabled() {
		t.Fatal("defaults not restored")
	}
}

// A change queued while the provider was stopped is what an operator sees come
// back after a restart. Both kill switches must land as off rather than falling
// back to the live defaults.
func TestP2PSourceOffQueuedWhileStoppedSurvivesRestart(t *testing.T) {
	t.Cleanup(func() {
		_ = connect.SetP2PSourceBinding(connect.P2PSourceBindingDefault())
		connect.SetP2PForgedSourceDrop(true)
	})
	home := withTempHome(t)
	writeJSONFile(t, filepath.Join(home, "pending_overrides.json"), `[
		{"op":"set","key":"p2p_source_binding","value":"off"},
		{"op":"set","key":"p2p_forged_source_drop","value":"off"}
	]`)
	state := newControlState()
	mergePendingControlOverrides(state)
	replayP2PSourcePolicyAtStartup(state)
	if got := connect.P2PSourceBinding(); got != "off" {
		t.Fatalf("p2p_source_binding after restart = %q, want off", got)
	}
	if connect.P2PForgedSourceDropEnabled() {
		t.Fatal("p2p_forged_source_drop after restart = on, want off")
	}
}

// The default the library starts in is the default this package advertises, so
// a clear cannot restore something the library does not do.
func TestP2PSourceLiveDefaultMatchesLibrary(t *testing.T) {
	if got := controlLiveDefaults["p2p_source_binding"]; got != connect.P2PSourceBindingDefault() {
		t.Fatalf("miner default %q, library default %q", got, connect.P2PSourceBindingDefault())
	}
	if got := controlLiveDefaults["p2p_forged_source_drop"]; got != "on" {
		t.Fatalf("forged drop default %q, want on", got)
	}
}
