package urnettools

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// testOwnership is a lookup result in the given state; an unknown one carries
// the lookup error that made it unknown.
func testOwnership(state serviceOwnership) providerServiceOwnership {
	o := providerServiceOwnership{State: state, Installed: true, Executable: `C:\Users\op\urnetwork\urnetwork.exe`, StateDir: `C:\Users\op\.urnetwork`, LogPath: `C:\Users\op\.urnetwork\service.log`}
	if state == serviceOwnershipUnknown {
		o.Err = errors.New("read the provider service config: urnetwork-provider.json is damaged")
	}
	return o
}

// Only "no service is installed" may read as not owned. Every other failed
// lookup leaves ownership unknown, or a plain stop would kill a miner the
// supervisor brings back and a plain start would run a second provider.
func TestClassifyServiceOwnership(t *testing.T) {
	const exe, state = `C:\u\urnetwork.exe`, `C:\u\.urnetwork`
	running := Provider{PID: 200, Running: true, Binary: exe, StateDir: state}
	stopped := Provider{Binary: exe, StateDir: state}
	tree := map[int]serviceProcessEntry{200: {Parent: 100, Name: "urnetwork.exe"}, 100: {Parent: 1, Name: "urnetwork.exe"}}
	saved := func(l serviceLookup) serviceLookup {
		l.Executable, l.StateDir = exe, state
		return l
	}
	cases := []struct {
		name      string
		p         Provider
		l         serviceLookup
		want      serviceOwnership
		installed bool
		errHas    string
	}{
		{"no service installed", running, serviceLookup{NotInstalled: true}, serviceNotOwned, false, ""},
		{"config unreadable", running, serviceLookup{LoadErr: errors.New("access is denied")}, serviceOwnershipUnknown, true, "access is denied"},
		{"config unreadable, stopped", stopped, serviceLookup{LoadErr: errors.New("is damaged")}, serviceOwnershipUnknown, true, "is damaged"},
		{"supervisor pid lookup failed", running, saved(serviceLookup{SupervisorErr: errors.New("its process id is unknown")}), serviceOwnershipUnknown, true, "process id is unknown"},
		{"supervisor pid lookup failed, stopped", stopped, saved(serviceLookup{SupervisorErr: errors.New("lock probe failed")}), serviceOwnershipUnknown, true, "lock probe failed"},
		{"process list failed", running, saved(serviceLookup{SupervisorPid: 100, ProcessesErr: errors.New("snapshot failed")}), serviceOwnershipUnknown, true, "snapshot failed"},
		{"process list failed, no selected pid", stopped, saved(serviceLookup{SupervisorPid: 100, ProcessesErr: errors.New("snapshot failed")}), serviceOwnershipUnknown, true, "snapshot failed"},
		{"miner under the supervisor", running, saved(serviceLookup{SupervisorPid: 100, Processes: tree}), serviceOwned, true, ""},
		{"running, no supervisor", running, saved(serviceLookup{}), serviceNotOwned, true, ""},
		{"stopped, matching config", stopped, saved(serviceLookup{}), serviceOwned, true, ""},
	}
	for _, c := range cases {
		got := classifyServiceOwnership(c.p, c.l)
		if got.State != c.want || got.Installed != c.installed {
			t.Errorf("%s: state %d installed %v, want %d and %v", c.name, got.State, got.Installed, c.want, c.installed)
			continue
		}
		if c.errHas != "" && (got.Err == nil || !strings.Contains(got.Err.Error(), c.errHas)) {
			t.Errorf("%s: err = %v, want it to name %q", c.name, got.Err, c.errHas)
		}
	}
}

// Unknown ownership refuses start, stop and restart, and never falls back to
// the direct process path.
func TestWindowsLifecycleRefusesWhenOwnershipUnknown(t *testing.T) {
	for _, action := range []string{"stop", "start", "restart"} {
		t.Run(action, func(t *testing.T) {
			c := stubWindowsLifecycle(t, serviceOwnershipUnknown, nil)
			var err error
			switch action {
			case "stop":
				err = stopWindowsProvider(windowsRoutingFixture(), false, false)
			case "start":
				err = startWindowsProvider(windowsRoutingFixture(), false, false)
			case "restart":
				err = restartWindowsProvider(windowsRoutingFixture(), true, false)
			}
			if err == nil || !strings.Contains(err.Error(), "could not tell whether") || !strings.Contains(err.Error(), "is damaged") || !strings.Contains(err.Error(), "urnet-tools "+action) {
				t.Fatalf("err = %v, want a refusal naming the lookup problem and the next step", err)
			}
			if *c != (windowsLifecycleCalls{}) {
				t.Fatalf("calls = %+v, want none: unknown ownership must change nothing", *c)
			}
		})
	}
}

// Uninstall with unknown ownership stops nothing and removes nothing.
func TestUninstallRefusesWhenOwnershipUnknown(t *testing.T) {
	p := uninstallFixture(t)
	stubCleanup(t)
	stubStopService(t, func(Provider) error { t.Error("the service must not be stopped"); return nil })
	stubOwnership(t, serviceOwnershipUnknown, func(Provider) error { t.Error("the provider must not be stopped by pid"); return nil }, func() { t.Error("service files must not be removed") })
	err := uninstallProvider(p)
	if err == nil || !strings.Contains(err.Error(), "could not tell whether") || !strings.Contains(err.Error(), "is damaged") {
		t.Fatalf("err = %v, want a refusal naming the lookup problem", err)
	}
	if _, err := os.Stat(p.Binary); err != nil {
		t.Errorf("binary must be untouched: %v", err)
	}
	if _, err := os.Stat(p.StateDir); err != nil {
		t.Errorf("state dir must be untouched: %v", err)
	}
}
