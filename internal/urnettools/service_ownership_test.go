package urnettools

import (
	"errors"
	"os"
	"testing"
)

func parents(m map[int]int) func(int) (int, bool) {
	return func(pid int) (int, bool) { p, ok := m[pid]; return p, ok }
}

func TestProcessDescendsFrom(t *testing.T) {
	tree := parents(map[int]int{10: 1, 20: 10, 30: 20, 40: 99})
	cases := []struct {
		name          string
		pid, ancestor int
		want          bool
	}{
		{"itself", 10, 10, true},
		{"child", 20, 10, true},
		{"grandchild", 30, 10, true},
		{"sibling tree", 40, 10, false},
		{"unknown pid", 77, 10, false},
		{"no supervisor", 20, 0, false},
		{"no pid", 0, 10, false},
	}
	for _, c := range cases {
		if got := processDescendsFrom(c.pid, c.ancestor, tree); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	// reused ids can form a cycle; the walk must still end
	if processDescendsFrom(1, 5, parents(map[int]int{1: 2, 2: 1})) {
		t.Error("a parent cycle must not match")
	}
}

func TestServiceOwnsProvider(t *testing.T) {
	tree := parents(map[int]int{200: 100, 300: 1})
	const exe = `C:\Users\u\AppData\Local\urnetwork\urnetwork.exe`
	const state = `C:\Users\u\.urnetwork`
	cases := []struct {
		name string
		p    Provider
		sup  int
		want bool
	}{
		{"supervisor itself", Provider{PID: 100, Running: true, Binary: exe}, 100, true},
		{"supervised miner", Provider{PID: 200, Running: true, Binary: exe}, 100, true},
		// same executable, but not under the supervisor: the bare process
		{"bare provider, same exe", Provider{PID: 300, Running: true, Binary: exe}, 100, false},
		{"bare provider, no supervisor", Provider{PID: 300, Running: true, Binary: exe}, 0, false},
		{"stopped, same exe and state dir", Provider{Binary: `c:/users/u/appdata/local/urnetwork/URNETWORK.exe`, StateDir: `c:/USERS/u/.urnetwork/`}, 0, true},
		// a second install can run the same executable with its own state
		{"stopped, same exe, other state dir", Provider{Binary: exe, StateDir: `D:\second\.urnetwork`}, 0, false},
		{"stopped, same exe, no state dir", Provider{Binary: exe}, 0, false},
		{"stopped, other exe", Provider{Binary: `D:\other\urnetwork.exe`, StateDir: state}, 0, false},
		{"stopped, no binary", Provider{StateDir: state}, 0, false},
	}
	for _, c := range cases {
		if got := serviceOwnsProvider(c.p, exe, state, c.sup, tree); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func stubOwnership(t *testing.T, owned bool, stopBare func(Provider) error, removeFiles func()) {
	t.Helper()
	po, sb, rf := providerOwnedByServiceFn, stopBareProviderFn, removeProviderServiceFilesFn
	providerOwnedByServiceFn = func(Provider) bool { return owned }
	stopBareProviderFn, removeProviderServiceFilesFn = stopBare, removeFiles
	t.Cleanup(func() { providerOwnedByServiceFn, stopBareProviderFn, removeProviderServiceFilesFn = po, sb, rf })
}

// A provider the saved service does not own must not stop that service or
// delete its shortcut and config; it is stopped through its own path.
func TestUninstallLeavesForeignServiceAlone(t *testing.T) {
	p := uninstallFixture(t)
	stubCleanup(t)
	stubStopService(t, func(Provider) error { t.Error("foreign service must not be stopped"); return nil })
	bare, removed := 0, 0
	stubOwnership(t, false, func(Provider) error { bare++; return nil }, func() { removed++ })
	if err := uninstallProvider(p); err != nil {
		t.Fatalf("uninstallProvider: %v", err)
	}
	if bare != 1 {
		t.Errorf("bare provider stopped %d times, want 1", bare)
	}
	if removed != 0 {
		t.Error("foreign service files must not be removed")
	}
}

func TestUninstallRemovesOwnedServiceFiles(t *testing.T) {
	p := uninstallFixture(t)
	stubCleanup(t)
	stopped := 0
	stubStopService(t, func(Provider) error { stopped++; return nil })
	removed := 0
	stubOwnership(t, true, func(Provider) error { t.Error("owned provider is stopped by the service"); return nil }, func() { removed++ })
	if err := uninstallProvider(p); err != nil {
		t.Fatalf("uninstallProvider: %v", err)
	}
	if stopped != 1 || removed != 1 {
		t.Errorf("stopped=%d removed=%d, want 1 and 1", stopped, removed)
	}
}

func TestUninstallAbortsWhenBareProviderWillNotStop(t *testing.T) {
	p := uninstallFixture(t)
	stubCleanup(t)
	stubStopService(t, func(Provider) error { return nil })
	stubOwnership(t, false, func(Provider) error { return errors.New("access denied") }, func() {})
	if err := uninstallProvider(p); err == nil {
		t.Fatal("must fail when the bare provider cannot be stopped")
	}
	if _, err := os.Stat(p.Binary); err != nil {
		t.Errorf("binary must be untouched: %v", err)
	}
}
