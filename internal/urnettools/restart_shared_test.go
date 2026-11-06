package urnettools

import (
	"errors"
	"strings"
	"testing"
)

// An unowned provider's restart stops it through provider.sock in the state
// dir. With another provider process running, the socket may be that one's:
// the restart would shut the wrong provider down. It must refuse before
// anything is stopped, and also when the process list cannot be read.

func stubRunningProvidersErr(t *testing.T, err error) {
	t.Helper()
	prev := runningProviderProcessesFn
	t.Cleanup(func() { runningProviderProcessesFn = prev })
	runningProviderProcessesFn = func() ([]Provider, error) { return nil, err }
}

func TestWindowsRestartUnownedRefusesWhileAnotherProviderRuns(t *testing.T) {
	c := stubWindowsLifecycle(t, serviceNotOwned, nil)
	p := windowsRoutingFixture()
	stubRunningProviders(t, p, Provider{PID: 7000, Running: true, Binary: p.Binary, StateDir: p.StateDir})
	err := restartWindowsProvider(p, false, false)
	if err == nil || !strings.Contains(err.Error(), "pid 7000") || !strings.Contains(err.Error(), "nothing was stopped") {
		t.Fatalf("err = %v, want a refusal naming pid 7000", err)
	}
	if c.directRestart != 0 {
		t.Fatalf("restart ran %d times, want 0", c.directRestart)
	}
}

func TestWindowsRestartUnownedRefusesWhenTheProcessListCannotBeRead(t *testing.T) {
	c := stubWindowsLifecycle(t, serviceNotOwned, nil)
	stubRunningProvidersErr(t, errors.New("snapshot denied"))
	err := restartWindowsProvider(windowsRoutingFixture(), false, false)
	if err == nil || !strings.Contains(err.Error(), "snapshot denied") || !strings.Contains(err.Error(), "nothing was stopped") {
		t.Fatalf("err = %v, want a fail-closed refusal carrying the cause", err)
	}
	if c.directRestart != 0 {
		t.Fatalf("restart ran %d times, want 0", c.directRestart)
	}
}

func TestWindowsRestartUnownedRunsWhenItIsTheOnlyProvider(t *testing.T) {
	c := stubWindowsLifecycle(t, serviceNotOwned, nil)
	p := windowsRoutingFixture()
	stubRunningProviders(t, p)
	if err := restartWindowsProvider(p, false, false); err != nil {
		t.Fatalf("restartWindowsProvider: %v", err)
	}
	if c.directRestart != 1 {
		t.Fatalf("restart ran %d times, want 1", c.directRestart)
	}
}

// The colocated guard cannot prove safety from a list it could not read.
func TestUninstallRefusesWhenTheProcessListCannotBeRead(t *testing.T) {
	p := uninstallFixture(t)
	p.PID, p.Running = 4242, true
	stubCleanup(t)
	stubStopService(t, func(Provider) error { t.Error("the service must not be stopped"); return nil })
	stubOwnership(t, serviceNotOwned, func(Provider) error { t.Error("the provider must not be stopped"); return nil }, func() { t.Error("service files must not be removed") })
	stubRunningProvidersErr(t, errors.New("snapshot denied"))
	err := uninstallProvider(p)
	if err == nil || !strings.Contains(err.Error(), "snapshot denied") || !strings.Contains(err.Error(), "Nothing was changed") {
		t.Fatalf("err = %v, want a fail-closed refusal carrying the cause", err)
	}
}

// A socket that answers proves a provider answers, not that it is the one
// the service started: a bare provider on the same state dir answers too.
// Up needs a provider process under the supervisor.
func TestServiceUpNeedsAMinerUnderTheSupervisor(t *testing.T) {
	owner := providerServiceOwnership{Executable: `C:\u\urnetwork.exe`, StateDir: `C:\u\.urnetwork`}
	bare := map[int]serviceProcessEntry{
		100: {Parent: 1, Name: "urnetwork.exe"}, // supervisor, no miner yet
		300: {Parent: 1, Name: "urnetwork.exe"}, // bare provider answering the socket
	}
	if got := observeProviderServiceUp(owner, 100, bare, 0); got.Up || !got.ServiceRunning {
		t.Fatalf("state %+v with only a bare provider, want not up and service running", got)
	}
	withMiner := map[int]serviceProcessEntry{
		100: {Parent: 1, Name: "urnetwork.exe"},
		200: {Parent: 100, Name: "urnetwork.exe"},
	}
	if got := observeProviderServiceUp(owner, 100, withMiner, 200); !got.Up {
		t.Fatalf("state %+v with the recorded miner under the supervisor, want up", got)
	}
}
