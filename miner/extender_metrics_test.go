package miner

import (
	"strings"
	"testing"

	"github.com/urnetwork/sdk"
)

// withExtenderSeam installs the extender seam hook for the duration of a test
// and restores the previous hook on cleanup.
func withExtenderSeam(t *testing.T, hook func() *providerExtenderObservation) {
	t.Helper()
	prev := metricsSeamExtender
	metricsSeamExtender = hook
	t.Cleanup(func() { metricsSeamExtender = prev })
}

var allExtenderMetricFamilies = []string{
	"urnet_extender_enabled",
	"urnet_extender_listening",
	"urnet_extender_listen_failed",
	"urnet_extender_activated",
	"urnet_extender_activation_failed",
	"urnet_extender_activation_refused",
	"urnet_extender_revoked",
	"urnet_extender_last_activation_attempt_seconds",
}

// When the extender seam hook is nil, all extender families must be completely
// absent from the scrape (never fabricated zeroes).
func TestExtenderMetricsAbsentWhenHookNil(t *testing.T) {
	withExtenderSeam(t, nil)
	out := minerExtraMetrics()
	for _, family := range allExtenderMetricFamilies {
		if strings.Contains(out, family) {
			t.Fatalf("family %q must be absent when hook is nil:\n%s", family, out)
		}
	}
}

// When the extender seam hook returns nil, all extender families must be absent.
func TestExtenderMetricsAbsentWhenHookReturnsNil(t *testing.T) {
	withExtenderSeam(t, func() *providerExtenderObservation {
		return nil
	})
	out := minerExtraMetrics()
	for _, family := range allExtenderMetricFamilies {
		if strings.Contains(out, family) {
			t.Fatalf("family %q must be absent when hook returns nil:\n%s", family, out)
		}
	}
}

// When the extender seam hook returns an unknown observation (Known == false),
// all extender families must be absent (not zero).
func TestExtenderMetricsAbsentWhenUnknown(t *testing.T) {
	withExtenderSeam(t, func() *providerExtenderObservation {
		return &providerExtenderObservation{
			Known:             false,
			Enabled:           true,
			Listening:         true,
			ListenFailed:      true,
			ActivatedV4:       true,
			ActivatedV6:       true,
			ActivationFailed:  true,
			ActivationRefused: true,
			Revoked:           true,
			ActivationTime:    1800000000000,
		}
	})
	out := minerExtraMetrics()
	for _, family := range allExtenderMetricFamilies {
		if strings.Contains(out, family) {
			t.Fatalf("family %q must be absent when observation is unknown:\n%s", family, out)
		}
	}
}

// When the hook returns a known observation, all families appear with the
// correct types, help strings, labels, and 0/1 boolean values.
func TestExtenderMetricsKnownValues(t *testing.T) {
	withExtenderSeam(t, func() *providerExtenderObservation {
		return &providerExtenderObservation{
			Known:             true,
			Enabled:           true,
			Listening:         true,
			ListenFailed:      false,
			ActivatedV4:       true,
			ActivatedV6:       false,
			ActivationFailed:  false,
			ActivationRefused: false,
			Revoked:           false,
			ActivationTime:    1800000000000,
		}
	})
	out := minerExtraMetrics()
	for _, want := range []string{
		"# HELP urnet_extender_enabled",
		"# TYPE urnet_extender_enabled gauge",
		"urnet_extender_enabled 1",
		"# HELP urnet_extender_listening",
		"# TYPE urnet_extender_listening gauge",
		"urnet_extender_listening 1",
		"# HELP urnet_extender_listen_failed",
		"# TYPE urnet_extender_listen_failed gauge",
		"urnet_extender_listen_failed 0",
		"# HELP urnet_extender_activated",
		"# TYPE urnet_extender_activated gauge",
		"urnet_extender_activated{family=\"ipv4\"} 1",
		"urnet_extender_activated{family=\"ipv6\"} 0",
		"# HELP urnet_extender_activation_failed",
		"# TYPE urnet_extender_activation_failed gauge",
		"urnet_extender_activation_failed 0",
		"# HELP urnet_extender_activation_refused",
		"# TYPE urnet_extender_activation_refused gauge",
		"urnet_extender_activation_refused 0",
		"# HELP urnet_extender_revoked",
		"# TYPE urnet_extender_revoked gauge",
		"urnet_extender_revoked 0",
		"# HELP urnet_extender_last_activation_attempt_seconds",
		"# TYPE urnet_extender_last_activation_attempt_seconds gauge",
		"urnet_extender_last_activation_attempt_seconds 1800000000",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("scrape missing expected line %q:\n%s", want, out)
		}
	}

	for _, err := range Lint(out) {
		t.Errorf("lint error: %v", err)
	}
}

// When ActivationTime <= 0, urnet_extender_last_activation_attempt_seconds must
// be omitted from the scrape, while the other families still appear.
func TestExtenderMetricsActivationTimeZeroOmitted(t *testing.T) {
	withExtenderSeam(t, func() *providerExtenderObservation {
		return &providerExtenderObservation{
			Known:          true,
			Enabled:        true,
			Listening:      true,
			ActivationTime: 0,
		}
	})
	out := minerExtraMetrics()
	if strings.Contains(out, "urnet_extender_last_activation_attempt_seconds") {
		t.Fatalf("urnet_extender_last_activation_attempt_seconds must be omitted when ActivationTime is 0:\n%s", out)
	}
	if !strings.Contains(out, "urnet_extender_enabled 1") {
		t.Fatalf("urnet_extender_enabled must be present:\n%s", out)
	}
	for _, err := range Lint(out) {
		t.Errorf("lint error: %v", err)
	}
}

// Test alternate booleans and timestamps to verify all conversions.
func TestExtenderMetricsInvertedValues(t *testing.T) {
	withExtenderSeam(t, func() *providerExtenderObservation {
		return &providerExtenderObservation{
			Known:             true,
			Enabled:           false,
			Listening:         false,
			ListenFailed:      true,
			ActivatedV4:       false,
			ActivatedV6:       true,
			ActivationFailed:  true,
			ActivationRefused: true,
			Revoked:           true,
			ActivationTime:    1700000000500,
		}
	})
	out := minerExtraMetrics()
	for _, want := range []string{
		"urnet_extender_enabled 0",
		"urnet_extender_listening 0",
		"urnet_extender_listen_failed 1",
		"urnet_extender_activated{family=\"ipv4\"} 0",
		"urnet_extender_activated{family=\"ipv6\"} 1",
		"urnet_extender_activation_failed 1",
		"urnet_extender_activation_refused 1",
		"urnet_extender_revoked 1",
		"urnet_extender_last_activation_attempt_seconds 1700000000",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("scrape missing expected line %q:\n%s", want, out)
		}
	}
	for _, err := range Lint(out) {
		t.Errorf("lint error: %v", err)
	}
}

// Test that providerExtenderStatusListener's observation() method returns a
// copy of the latest observation under stateLock.
func TestExtenderStatusListenerObservationMethod(t *testing.T) {
	testExtenderState(t)
	listener := newProviderExtenderStatusListener(nil, 0, "")

	// Before any observation is recorded
	initial := listener.observation()
	if initial.Known {
		t.Fatalf("initial observation must be unknown: %+v", initial)
	}

	status := &sdk.ExtenderProvideStatus{
		Enabled:               true,
		Listening:             true,
		ListenError:           "listen err",
		ActivatedV4:           true,
		ActivatedV6:           false,
		LastActivationError:   "refused",
		LastActivationRefused: true,
		LastActivationTime:    1800000000000,
		RevokedTime:           1800000001000,
	}
	listener.ExtenderProvideStatusChanged(status)
	// The listener publishes process-wide, so this test's extender must not be
	// readable by the next scrape in the package.
	t.Cleanup(resetProviderExtenderObservation)

	obs := listener.observation()
	if !obs.Known {
		t.Fatal("expected Known to be true")
	}
	if !obs.Enabled || !obs.Listening || !obs.ListenFailed || !obs.ActivatedV4 || obs.ActivatedV6 || !obs.ActivationFailed || !obs.ActivationRefused || !obs.Revoked || obs.ActivationTime != 1800000000000 {
		t.Fatalf("observation mismatch: %+v", obs)
	}

	// Verify Observation() alias returns identical result
	if listener.Observation() != obs {
		t.Fatalf("Observation() did not match observation()")
	}
}

// A posted role that reports off, or a device that is torn down, must stop being
// published. Nothing else replaces an enabled reading, so the scrape would
// otherwise keep claiming an extender that has stopped. A listener that is not
// the publisher clears nothing.
func TestExtenderObservationClearedByItsOwnListener(t *testing.T) {
	resetProviderExtenderObservation()
	t.Cleanup(resetProviderExtenderObservation)

	direct := newProviderExtenderStatusListener(nil, 0, "")
	proxy := newProviderExtenderStatusListener(nil, 1, "")

	direct.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, Listening: true, ActivatedV4: true})
	if providerExtenderObservationLatest() == nil {
		t.Fatal("an enabled reading must be published")
	}

	proxy.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: false})
	if providerExtenderObservationLatest() == nil {
		t.Fatal("a proxy slot's disabled reading erased the direct slot's reading")
	}

	direct.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: false})
	if latest := providerExtenderObservationLatest(); latest != nil {
		t.Fatalf("a disabled reading from the publisher must clear it, got %+v", *latest)
	}
}

// The teardown path clears what its own listener published, and only that.
func TestExtenderObservationClearedOnTeardown(t *testing.T) {
	resetProviderExtenderObservation()
	t.Cleanup(resetProviderExtenderObservation)

	direct := newProviderExtenderStatusListener(nil, 0, "")
	other := newProviderExtenderStatusListener(nil, 2, "")
	direct.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, Listening: true})

	clearProviderExtenderObservation(other)
	if providerExtenderObservationLatest() == nil {
		t.Fatal("another listener's teardown cleared the direct reading")
	}

	clearProviderExtenderObservation(direct)
	if latest := providerExtenderObservationLatest(); latest != nil {
		t.Fatalf("the publisher's teardown must clear it, got %+v", *latest)
	}
}

// Retiring a listener drops its reading and stops late callbacks from publishing.
func TestExtenderObservationRetiredListener(t *testing.T) {
	resetProviderExtenderObservation()
	t.Cleanup(resetProviderExtenderObservation)

	direct := newProviderExtenderStatusListener(nil, 0, "")
	other := newProviderExtenderStatusListener(nil, 2, "")
	direct.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, Listening: true})

	retireProviderExtenderObservation(other)
	if providerExtenderObservationLatest() == nil {
		t.Fatal("another listener's retirement cleared the direct reading")
	}

	retireProviderExtenderObservation(direct)
	if latest := providerExtenderObservationLatest(); latest != nil {
		t.Fatalf("the publisher's retirement must clear it, got %+v", *latest)
	}

	// A late callback from the retired listener must not publish.
	direct.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{Enabled: true, Listening: true})
	if latest := providerExtenderObservationLatest(); latest != nil {
		t.Fatalf("a retired listener's late callback must not publish, got %+v", *latest)
	}
}
