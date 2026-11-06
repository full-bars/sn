package urnettools

import (
	"strings"
	"testing"
)

// Start and restart of an owned provider say "started" or "restarted" only
// once the provider is up, within a bounded wait.

func stubProviderUp(t *testing.T, states ...providerUpState) *int {
	t.Helper()
	calls := 0
	providerServiceUpFn = func(providerServiceOwnership) providerUpState {
		state := states[min(calls, len(states)-1)]
		calls++
		return state
	}
	return &calls
}

var (
	upNotYet       = providerUpState{ServiceRunning: true}
	upNow          = providerUpState{Up: true, ServiceRunning: true}
	upServiceEnded = providerUpState{}
)

func TestWindowsStartOwnedWaitsForTheProvider(t *testing.T) {
	stubWindowsLifecycle(t, serviceOwned, nil)
	calls := stubProviderUp(t, upNotYet, upNotYet, upNow)
	var err error
	out := captureStdout(t, func() { err = startWindowsProvider(windowsRoutingFixture(), false, false) })
	if err != nil {
		t.Fatalf("startWindowsProvider: %v", err)
	}
	if *calls != 3 || strings.Count(out, "started ") != 1 {
		t.Fatalf("up checks = %d, output %q; want 3 checks and one \"started\"", *calls, out)
	}
}

func TestWindowsStartOwnedReportsAProviderThatDoesNotComeUp(t *testing.T) {
	stubWindowsLifecycle(t, serviceOwned, nil)
	stubProviderUp(t, upNotYet)
	var err error
	out := captureStdout(t, func() { err = startWindowsProvider(windowsRoutingFixture(), false, false) })
	if err == nil || !strings.Contains(err.Error(), "did not come up") || !strings.Contains(err.Error(), `.urnetwork\service.log`) {
		t.Fatalf("err = %v, want a did-not-come-up error with the service log", err)
	}
	if strings.Contains(out, "started ") {
		t.Fatalf("output %q must not claim started", out)
	}
}

func TestWindowsStartOwnedReportsAServiceThatEndsBeforeTheProviderIsUp(t *testing.T) {
	stubWindowsLifecycle(t, serviceOwned, nil)
	stubProviderUp(t, upNotYet, upServiceEnded)
	err := startWindowsProvider(windowsRoutingFixture(), false, false)
	if err == nil || !strings.Contains(err.Error(), "stopped before") || !strings.Contains(err.Error(), `service.log`) {
		t.Fatalf("err = %v, want a stopped-before-up error with the service log", err)
	}
}

func TestWindowsRestartOwnedReportsAProviderThatDoesNotComeUp(t *testing.T) {
	stubWindowsLifecycle(t, serviceOwned, nil)
	stubProviderUp(t, upNotYet)
	var err error
	out := captureStdout(t, func() { err = restartWindowsProvider(windowsRoutingFixture(), false, false) })
	if err == nil || !strings.Contains(err.Error(), "could not restart provider") || !strings.Contains(err.Error(), "did not come up") || !strings.Contains(err.Error(), "service.log") {
		t.Fatalf("err = %v, want a could-not-restart error that says the provider did not come up", err)
	}
	if strings.Contains(out, "restarted ") {
		t.Fatalf("output %q must not claim restarted", out)
	}
}
