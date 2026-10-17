package provider

import "testing"

// /metrics is on unless an operator turned it off: an unset key follows the
// default, an explicit off is honored, an explicit on stays on.
func TestMetricsEnabledByStateDefaultsOn(t *testing.T) {
	withTempHome(t)
	state := newControlState()
	if !metricsEnabledByState(state) {
		t.Fatal("an unset metrics key must default to on")
	}
	if err := state.set("metrics", "off"); err != nil {
		t.Fatal(err)
	}
	if metricsEnabledByState(state) {
		t.Fatal("a persisted off must win over the default")
	}
	if err := state.set("metrics", "on"); err != nil {
		t.Fatal(err)
	}
	if !metricsEnabledByState(state) {
		t.Fatal("a persisted on must stay on")
	}
}

// The startup default starts the listener for a node that never set the key,
// stays out of the way of an explicit address, a persisted off and a HotSwap
// candidate, and the persisted-tuning replay is unchanged for a fresh state.
func TestStartDefaultMetrics(t *testing.T) {
	withTempHome(t)
	t.Setenv("URNETWORK_METRICS", "")
	stubMetricsAuto(t, noTailscale)
	t.Cleanup(func() {
		_ = applyMetricsLive("off")
		ClearCoordinatorClosers()
	})

	off := newControlState()
	if err := off.set("metrics", "off"); err != nil {
		t.Fatal(err)
	}
	startDefaultMetrics(off, false)
	if metricsServer != nil {
		t.Fatal("a persisted off must not start the listener")
	}

	startDefaultMetrics(newControlState(), true)
	if metricsServer != nil {
		t.Fatal("a HotSwap candidate must not bind: the parent still holds the address")
	}

	t.Setenv("URNETWORK_METRICS", "127.0.0.1:1")
	startDefaultMetrics(newControlState(), false)
	if metricsServer != nil {
		t.Fatal("an explicit URNETWORK_METRICS address is bound at startup, not here")
	}
	t.Setenv("URNETWORK_METRICS", "")

	startDefaultMetrics(newControlState(), false)
	if metricsServer == nil {
		t.Fatal("a node that never set the key must start /metrics at startup")
	}
	// idempotent
	startDefaultMetrics(newControlState(), false)

	// replaying persisted tuning on a fresh state does not start a listener
	_ = applyMetricsLive("off")
	applyPersistedRuntimeTuning(newControlState())
	if metricsServer != nil {
		t.Fatal("the persisted-tuning replay only acts on an explicit persisted on")
	}
}
