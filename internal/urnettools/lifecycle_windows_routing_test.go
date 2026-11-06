package urnettools

import (
	"errors"
	"strings"
	"testing"
)

// Plain start, stop and restart on Windows must go through the per-user
// supervisor when it owns the provider: stopping only the miner by pid lets
// the supervisor start it again, and starting "provide" by hand runs a second
// provider beside the supervisor's. A provider the supervisor does not own
// keeps the direct process path. The routing is tested through seams, so it
// runs on every platform and never touches a real service.

type windowsLifecycleCalls struct {
	directStop, directStart, directRestart    int
	serviceStop, serviceStart, serviceRestart int
}

func stubWindowsLifecycle(t *testing.T, owned bool, serviceErr error) *windowsLifecycleCalls {
	t.Helper()
	c := &windowsLifecycleCalls{}
	po, ss, st, rs := providerOwnedByServiceFn, stopProviderServiceFn, startProviderServiceFn, restartProviderWindowsFn
	ds, dst, dr := cmdStopWindowsFn, cmdStartWindowsFn, cmdRestartWindowsFn
	t.Cleanup(func() {
		providerOwnedByServiceFn, stopProviderServiceFn, startProviderServiceFn, restartProviderWindowsFn = po, ss, st, rs
		cmdStopWindowsFn, cmdStartWindowsFn, cmdRestartWindowsFn = ds, dst, dr
	})
	providerOwnedByServiceFn = func(Provider) bool { return owned }
	stopProviderServiceFn = func(Provider) error { c.serviceStop++; return serviceErr }
	startProviderServiceFn = func(Provider) error { c.serviceStart++; return serviceErr }
	restartProviderWindowsFn = func(Provider) error { c.serviceRestart++; return serviceErr }
	cmdStopWindowsFn = func(Provider, bool, bool) error { c.directStop++; return nil }
	cmdStartWindowsFn = func(Provider, bool, bool) error { c.directStart++; return nil }
	cmdRestartWindowsFn = func(Provider, bool, bool) error { c.directRestart++; return nil }
	return c
}

func windowsRoutingFixture() Provider {
	return Provider{PID: 4242, Running: true, Binary: `C:\Users\op\urnetwork\urnetwork.exe`, StateDir: `C:\Users\op\.urnetwork`}
}

func TestWindowsStopOwnedProviderStopsTheService(t *testing.T) {
	c := stubWindowsLifecycle(t, true, nil)
	if err := stopWindowsProvider(windowsRoutingFixture(), false, false); err != nil {
		t.Fatalf("stopWindowsProvider: %v", err)
	}
	if c.serviceStop != 1 || c.directStop != 0 {
		t.Fatalf("service stops = %d, pid stops = %d; want 1 and 0 (a pid stop is undone by the supervisor)", c.serviceStop, c.directStop)
	}
}

func TestWindowsStopOwnedProviderReportsServiceFailure(t *testing.T) {
	c := stubWindowsLifecycle(t, true, errors.New("supervisor did not stop"))
	err := stopWindowsProvider(windowsRoutingFixture(), false, false)
	if err == nil || !strings.Contains(err.Error(), "could not stop provider") || !strings.Contains(err.Error(), "supervisor did not stop") {
		t.Fatalf("err = %v, want a could-not-stop error carrying the cause", err)
	}
	if c.directStop != 0 {
		t.Fatalf("pid stop ran %d times after a failed service stop, want 0", c.directStop)
	}
}

func TestWindowsStopUnownedProviderStopsTheProcess(t *testing.T) {
	c := stubWindowsLifecycle(t, false, nil)
	if err := stopWindowsProvider(windowsRoutingFixture(), true, false); err != nil {
		t.Fatalf("stopWindowsProvider: %v", err)
	}
	if c.directStop != 1 || c.serviceStop != 0 {
		t.Fatalf("pid stops = %d, service stops = %d; want 1 and 0 (another provider's supervisor must not be stopped)", c.directStop, c.serviceStop)
	}
}

func TestWindowsStartOwnedProviderStartsTheService(t *testing.T) {
	c := stubWindowsLifecycle(t, true, nil)
	p := windowsRoutingFixture()
	p.PID, p.Running = 0, false
	if err := startWindowsProvider(p, false, false); err != nil {
		t.Fatalf("startWindowsProvider: %v", err)
	}
	if c.serviceStart != 1 || c.directStart != 0 {
		t.Fatalf("service starts = %d, direct starts = %d; want 1 and 0 (a direct start runs a provider the supervisor does not manage)", c.serviceStart, c.directStart)
	}
}

func TestWindowsStartOwnedProviderReportsServiceFailure(t *testing.T) {
	stubWindowsLifecycle(t, true, errors.New("the provider service did not start"))
	err := startWindowsProvider(windowsRoutingFixture(), false, false)
	if err == nil || !strings.Contains(err.Error(), "could not start provider") {
		t.Fatalf("err = %v, want a could-not-start error", err)
	}
}

func TestWindowsStartUnownedProviderStartsDirectly(t *testing.T) {
	c := stubWindowsLifecycle(t, false, nil)
	if err := startWindowsProvider(windowsRoutingFixture(), false, false); err != nil {
		t.Fatalf("startWindowsProvider: %v", err)
	}
	if c.directStart != 1 || c.serviceStart != 0 {
		t.Fatalf("direct starts = %d, service starts = %d; want 1 and 0", c.directStart, c.serviceStart)
	}
}

func TestWindowsRestartOwnedProviderRestartsTheService(t *testing.T) {
	c := stubWindowsLifecycle(t, true, nil)
	if err := restartWindowsProvider(windowsRoutingFixture(), false, false); err != nil {
		t.Fatalf("restartWindowsProvider: %v", err)
	}
	if c.serviceRestart != 1 || c.directRestart != 0 || c.directStop != 0 {
		t.Fatalf("service restarts = %d, direct restarts = %d, pid stops = %d; want 1, 0, 0", c.serviceRestart, c.directRestart, c.directStop)
	}
}

func TestWindowsRestartUnownedProviderRestartsDirectly(t *testing.T) {
	c := stubWindowsLifecycle(t, false, nil)
	if err := restartWindowsProvider(windowsRoutingFixture(), true, false); err != nil {
		t.Fatalf("restartWindowsProvider: %v", err)
	}
	if c.directRestart != 1 || c.serviceRestart != 0 {
		t.Fatalf("direct restarts = %d, service restarts = %d; want 1 and 0", c.directRestart, c.serviceRestart)
	}
}
