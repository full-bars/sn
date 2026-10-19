package miner

import "testing"

// h3_direct_default_test.go ports the v33 provider's h3_direct_default_test.go.
// The datagram half of the v33 test does not apply here: the h3 port is
// limit-only and the pinned upstream connect has datagrams always on, so the
// relative keys are accepted but never toggled (see control_socket.go).

// TestDirectOnlyH3Defaults pins which proxy-resolution states auto-enable the
// H3 transport family. Only a SETTLED direct-only node qualifies: a node still
// loading proxies, one whose pool resolved empty, and one with direct switched
// off (nothing served at all) must all keep the off default.
func TestDirectOnlyH3Defaults(t *testing.T) {
	cases := []struct {
		name       string
		resolution int32
		want       bool
	}{
		{"direct-only, settled", proxyResolutionZeroValid, true},
		{"direct off and no source", proxyResolutionNoSource, false},
		{"proxies resolved", proxyResolutionOK, false},
		{"source yielded nothing", proxyResolutionEmpty, false},
		{"source failed", proxyResolutionFailed, false},
		{"still pending", proxyResolutionPending, false},
	}
	for _, tc := range cases {
		if got := directOnlyH3Defaults(tc.resolution); got != tc.want {
			t.Errorf("directOnlyH3Defaults(%s) = %v, want %v",
				tc.name, got, tc.want)
		}
	}
}

// TestDefaultH3Setting checks the startup fallback: URNETWORK_H3 still asks for
// H3 on a pooled node, while a settled direct-only node gets it for free.
func TestDefaultH3Setting(t *testing.T) {
	t.Setenv("URNETWORK_H3", "")

	if !defaultH3Setting(nil, proxyResolutionZeroValid) {
		t.Error("a settled direct-only node must default H3 on with no env set")
	}
	if defaultH3Setting(nil, proxyResolutionOK) {
		t.Error("a pooled node must keep the H3 off default with no env set")
	}

	t.Setenv("URNETWORK_H3", "on")
	if !defaultH3Setting(nil, proxyResolutionOK) {
		t.Error("URNETWORK_H3=on must still enable H3 on a pooled node")
	}
}

// TestH3DefaultsFollowResolutionTransitions drives the settled-state hook: a
// transition into direct-only applies the default, a transition out of it
// applies the startup default again, and an explicit operator key is never
// overridden in either direction.
func TestH3DefaultsFollowResolutionTransitions(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	oldState, oldStatus := globalControlState, currentProxyResolution()
	oldApplied := h3AppliedSetting.Load()
	t.Cleanup(func() {
		globalControlState = oldState
		proxyResolutionStatus.Store(oldStatus)
		h3AppliedSetting.Store(oldApplied)
	})

	for _, tc := range []struct {
		name, env, override string
		wantEntering        h3Mode
		wantLeaving         h3Mode
	}{
		// no key, no env: direct-only turns h3 on; a pooled resolution returns
		// to the plain automatic-fallback default
		{"unset", "", "", h3ModeOn, h3ModeDirect},
		// URNETWORK_H3=on survives both directions
		{"environment fallback", "on", "", h3ModeOn, h3ModeOn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("URNETWORK_H3", tc.env)
			state := newControlState()
			if tc.override != "" {
				if err := state.set("h3", tc.override); err != nil {
					t.Fatal(err)
				}
			}
			globalControlState = state
			h3AppliedSetting.Store(nil)
			proxyResolutionStatus.Store(proxyResolutionPending)

			setProxyResolutionStatus(proxyResolutionZeroValid, "")
			if got := h3AppliedSetting.Load(); got == nil || got.Mode != tc.wantEntering {
				t.Fatalf("entering direct-only applied %+v, want %v", got, tc.wantEntering)
			}
			setProxyResolutionStatus(proxyResolutionOK, "")
			if got := h3AppliedSetting.Load(); got == nil || got.Mode != tc.wantLeaving {
				t.Fatalf("leaving direct-only applied %+v, want %v", got, tc.wantLeaving)
			}
		})
	}

	// an explicit key (either value) is an operator choice: the default must
	// leave it alone on both transitions
	sentinel := h3Setting{Mode: h3ModeTop, Cap: 7}
	for _, key := range []string{"off", "direct", "on", "all"} {
		state := newControlState()
		if err := state.set("h3", key); err != nil {
			t.Fatal(err)
		}
		globalControlState = state
		h3AppliedSetting.Store(&sentinel)
		proxyResolutionStatus.Store(proxyResolutionPending)
		setProxyResolutionStatus(proxyResolutionZeroValid, "")
		if got := h3AppliedSetting.Load(); got == nil || *got != sentinel {
			t.Fatalf("key %q: entering direct-only changed the applied setting to %+v", key, got)
		}
		setProxyResolutionStatus(proxyResolutionOK, "")
		if got := h3AppliedSetting.Load(); got == nil || *got != sentinel {
			t.Fatalf("key %q: leaving direct-only changed the applied setting to %+v", key, got)
		}
	}
}
