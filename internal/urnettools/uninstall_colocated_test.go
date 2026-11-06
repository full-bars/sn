package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Uninstall removes the binary and the state dir. When another running
// provider uses either, removing them breaks it, so uninstall refuses, names
// that provider and changes nothing.

func stubRunningProviders(t *testing.T, running ...Provider) {
	t.Helper()
	prev := runningProviderProcessesFn
	t.Cleanup(func() { runningProviderProcessesFn = prev })
	runningProviderProcessesFn = func() []Provider { return running }
}

func TestUninstallRefusesWithAColocatedLiveProvider(t *testing.T) {
	cases := []struct {
		name  string
		owned serviceOwnership
		other func(p Provider) Provider
	}{
		// a bare provider beside the service, same executable, other state
		{"owned, shared binary", serviceOwned, func(p Provider) Provider {
			return Provider{PID: 7000, Running: true, Binary: strings.ToUpper(p.Binary), StateDir: filepath.Join(t.TempDir(), "other")}
		}},
		{"owned, shared state dir", serviceOwned, func(p Provider) Provider {
			return Provider{PID: 7000, Running: true, Binary: filepath.Join(t.TempDir(), "other.exe"), StateDir: p.StateDir}
		}},
		// a bare provider whose binary the service also runs
		{"bare, service shares the binary", serviceNotOwned, func(p Provider) Provider {
			return Provider{PID: 4000, Running: true, Binary: p.Binary, StateDir: p.StateDir}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := uninstallFixture(t)
			p.PID, p.Running = 4242, true
			stubCleanup(t)
			stubStopService(t, func(Provider) error { t.Error("the service must not be stopped"); return nil })
			stubOwnership(t, c.owned, func(Provider) error { t.Error("the provider must not be stopped"); return nil }, func() { t.Error("service files must not be removed") })
			other := c.other(p)
			stubRunningProviders(t, p, other)
			err := uninstallProvider(p)
			if err == nil || !strings.Contains(err.Error(), "pid 7000") && !strings.Contains(err.Error(), "pid 4000") || !strings.Contains(err.Error(), "Nothing was changed") {
				t.Fatalf("err = %v, want a refusal naming the other provider", err)
			}
			if _, err := os.Stat(p.Binary); err != nil {
				t.Errorf("binary must be untouched: %v", err)
			}
			if _, err := os.Stat(p.StateDir); err != nil {
				t.Errorf("state dir must be untouched: %v", err)
			}
		})
	}
}

// The service's own processes (its supervisor and miner) are not "another"
// provider: uninstall of the owned provider stops and removes them.
func TestUninstallIgnoresTheServicesOwnProcesses(t *testing.T) {
	p := uninstallFixture(t)
	p.PID, p.Running = 4243, true
	stubCleanup(t)
	stubStopService(t, func(Provider) error { return nil })
	stubOwnership(t, serviceOwned, func(Provider) error { t.Error("an owned provider is stopped through its service"); return nil }, func() {})
	owner := ownedWithMiner(4243)
	stubOwner(t, owner)
	stubServiceProcesses(t, &fakeServiceProcess{pid: 4243})
	stubRunningProviders(t,
		Provider{PID: owner.SupervisorPid, Running: true, Binary: p.Binary, StateDir: p.StateDir},
		p,
	)
	if err := uninstallProvider(p); err != nil {
		t.Fatalf("uninstallProvider: %v", err)
	}
	if _, err := os.Stat(p.Binary); !os.IsNotExist(err) {
		t.Errorf("binary should be removed, stat err = %v", err)
	}
}
