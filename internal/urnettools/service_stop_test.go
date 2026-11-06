package urnettools

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeServiceProcess stands in for a process held open across a stop.
type fakeServiceProcess struct {
	pid             int
	alive           bool
	endsOnTerminate bool
	terminated      int
	closed          bool
	waits           []time.Duration
}

func (f *fakeServiceProcess) Pid() int { return f.pid }
func (f *fakeServiceProcess) Exited(wait time.Duration) bool {
	f.waits = append(f.waits, wait)
	return !f.alive
}
func (f *fakeServiceProcess) Terminate() error {
	f.terminated++
	if !f.endsOnTerminate {
		return errors.New("access is denied")
	}
	f.alive = false
	return nil
}
func (f *fakeServiceProcess) Close() { f.closed = true }

// stubServiceProcesses serves the given fakes by pid; any other pid is gone.
func stubServiceProcesses(t *testing.T, procs ...*fakeServiceProcess) {
	t.Helper()
	prev := openServiceProcessFn
	t.Cleanup(func() { openServiceProcessFn = prev })
	openServiceProcessFn = func(pid int) (serviceProcessHandle, error) {
		for _, f := range procs {
			if f.pid == pid {
				return f, nil
			}
		}
		return nil, errors.New("the process is gone")
	}
}

// ownedWithMiner is an owned lookup whose service runs the miner minerPid.
func ownedWithMiner(minerPid int, others ...int) providerServiceOwnership {
	o := testOwnership(serviceOwned)
	o.SupervisorPid, o.MinerPid = 4000, minerPid
	o.ServicePids = append([]int{minerPid}, others...)
	return o
}

func stubOwner(t *testing.T, o providerServiceOwnership) {
	t.Helper()
	prev := providerOwnedByServiceFn
	t.Cleanup(func() { providerOwnedByServiceFn = prev })
	providerOwnedByServiceFn = func(Provider) providerServiceOwnership { return o }
}

// The miner can be outside the supervisor's job (assigning it failed), so it
// lives on after the supervisor stopped. The stop must end it.
func TestOwnedStopEndsAMinerThatOutlivesTheService(t *testing.T) {
	stubWindowsLifecycle(t, serviceOwned, nil)
	stubOwner(t, ownedWithMiner(4243))
	miner := &fakeServiceProcess{pid: 4243, alive: true, endsOnTerminate: true}
	stubServiceProcesses(t, miner)
	if err := stopWindowsProvider(windowsRoutingFixture(), false, false); err != nil {
		t.Fatalf("stopWindowsProvider: %v", err)
	}
	if miner.terminated != 1 || miner.alive {
		t.Fatalf("miner terminated %d times, alive %v; want it ended once", miner.terminated, miner.alive)
	}
	if !miner.closed {
		t.Error("the miner's handle must be closed")
	}
}

// An inaccessible miner is not proof that it has exited. Stop must not begin
// when opening any captured service process fails.
func TestOwnedStopRefusesWhenProcessCannotBeOpened(t *testing.T) {
	calls := stubWindowsLifecycle(t, serviceOwned, nil)
	stubOwner(t, ownedWithMiner(4243))
	previous := openServiceProcessFn
	t.Cleanup(func() { openServiceProcessFn = previous })
	openServiceProcessFn = func(int) (serviceProcessHandle, error) {
		return nil, errors.New("access is denied")
	}
	err := stopWindowsProvider(windowsRoutingFixture(), false, false)
	if err == nil || !strings.Contains(err.Error(), "4243") || !strings.Contains(err.Error(), "access is denied") {
		t.Fatalf("err = %v, want refusal naming the inaccessible process", err)
	}
	if calls.serviceStop != 0 {
		t.Fatal("the service must not be stopped without a process handle")
	}
}

// A miner that can't be ended means the provider did not stop: say so.
func TestOwnedStopFailsWhenTheMinerWillNotEnd(t *testing.T) {
	stubWindowsLifecycle(t, serviceOwned, nil)
	stubOwner(t, ownedWithMiner(4243))
	stubServiceProcesses(t, &fakeServiceProcess{pid: 4243, alive: true})
	err := stopWindowsProvider(windowsRoutingFixture(), false, false)
	if err == nil || !strings.Contains(err.Error(), "could not stop provider") || !strings.Contains(err.Error(), "4243") || !strings.Contains(err.Error(), "Task Manager") {
		t.Fatalf("err = %v, want a could-not-stop error naming the miner and the next step", err)
	}
}

// Another process the supervisor started (an update check) is not ended,
// but the stop can't claim success while it runs.
func TestOwnedStopReportsAnotherServiceProcessStillRunning(t *testing.T) {
	stubWindowsLifecycle(t, serviceOwned, nil)
	stubOwner(t, ownedWithMiner(4243, 4250))
	miner := &fakeServiceProcess{pid: 4243}
	update := &fakeServiceProcess{pid: 4250, alive: true, endsOnTerminate: true}
	stubServiceProcesses(t, miner, update)
	err := stopWindowsProvider(windowsRoutingFixture(), false, false)
	if err == nil || !strings.Contains(err.Error(), "4250") {
		t.Fatalf("err = %v, want it to name process 4250", err)
	}
	if update.terminated != 0 {
		t.Fatal("a process that is not the miner must not be ended")
	}
}

// A miner that ended with its supervisor needs nothing more.
func TestOwnedStopLeavesAnEndedMinerAlone(t *testing.T) {
	stubWindowsLifecycle(t, serviceOwned, nil)
	stubOwner(t, ownedWithMiner(4243))
	miner := &fakeServiceProcess{pid: 4243}
	stubServiceProcesses(t, miner)
	if err := stopWindowsProvider(windowsRoutingFixture(), false, false); err != nil {
		t.Fatalf("stopWindowsProvider: %v", err)
	}
	if miner.terminated != 0 {
		t.Fatal("an ended miner must not be terminated")
	}
}

// Uninstall must not remove anything while the miner still runs.
func TestUninstallKeepsFilesWhenTheMinerSurvives(t *testing.T) {
	p := uninstallFixture(t)
	stubCleanup(t)
	stubStopService(t, func(Provider) error { return nil })
	stubOwnership(t, serviceOwned, func(Provider) error { t.Error("an owned provider is stopped through its service"); return nil }, func() { t.Error("service files must not be removed") })
	stubOwner(t, ownedWithMiner(4243))
	stubServiceProcesses(t, &fakeServiceProcess{pid: 4243, alive: true})
	err := uninstallProvider(p)
	if err == nil || !strings.Contains(err.Error(), "4243") || !strings.Contains(err.Error(), "Nothing was removed") {
		t.Fatalf("err = %v, want an error naming the miner and that nothing was removed", err)
	}
	if _, err := os.Stat(p.Binary); err != nil {
		t.Errorf("binary must be untouched: %v", err)
	}
	if _, err := os.Stat(p.StateDir); err != nil {
		t.Errorf("state dir must be untouched: %v", err)
	}
}

// The lookup names the miner: the one the supervisor recorded when it is
// still under it, else the selected provider when it is the miner.
func TestClassifyServiceOwnershipFindsTheMiner(t *testing.T) {
	const exe, state = `C:\u\urnetwork.exe`, `C:\u\.urnetwork`
	tree := map[int]serviceProcessEntry{
		100: {Parent: 1, Name: "urnetwork.exe"},   // supervisor
		200: {Parent: 100, Name: "urnetwork.exe"}, // miner
		210: {Parent: 100, Name: "urnetwork.exe"}, // update check
		220: {Parent: 100, Name: "conhost.exe"},   // not a provider
		300: {Parent: 1, Name: "urnetwork.exe"},   // bare provider
	}
	lookup := func(recorded int) serviceLookup {
		return serviceLookup{Executable: exe, StateDir: state, SupervisorPid: 100, Processes: tree, RecordedMinerPid: recorded}
	}
	cases := []struct {
		name      string
		p         Provider
		recorded  int
		wantMiner int
	}{
		{"recorded miner, supervisor selected", Provider{PID: 100, Running: true}, 200, 200},
		{"no record, miner selected", Provider{PID: 200, Running: true}, 0, 200},
		{"stale record (bare provider), miner selected", Provider{PID: 200, Running: true}, 300, 200},
		{"no record, supervisor selected", Provider{PID: 100, Running: true}, 0, 0},
	}
	for _, c := range cases {
		got := classifyServiceOwnership(c.p, lookup(c.recorded))
		if got.State != serviceOwned || got.MinerPid != c.wantMiner {
			t.Errorf("%s: state %d miner %d, want owned and %d", c.name, got.State, got.MinerPid, c.wantMiner)
		}
		// a known miner is the only service process a stop must wait for: the
		// update check (210) is not the miner and must not block a restart.
		// With no miner known, every provider process under the supervisor
		// is held.
		want := []int{200}
		if c.wantMiner == 0 {
			want = []int{200, 210}
		}
		if len(got.ServicePids) != len(want) || got.ServicePids[0] != want[0] || (len(want) > 1 && got.ServicePids[1] != want[1]) {
			t.Errorf("%s: service pids %v, want %v", c.name, got.ServicePids, want)
		}
	}
}
