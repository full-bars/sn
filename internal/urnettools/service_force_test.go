package urnettools

import (
	"strings"
	"testing"
)

// --force on a provider the service owns keeps going through the service
// (a pid kill alone is undone by the supervisor), but gives the supervisor a
// short wait instead of the graceful one, ends a miner still running at once,
// and says that force went through the service.

func TestWindowsForceStopOwnedGoesThroughTheServiceQuickly(t *testing.T) {
	c := stubWindowsLifecycle(t, serviceOwned, nil)
	stubOwner(t, ownedWithMiner(4243))
	miner := &fakeServiceProcess{pid: 4243, alive: true, endsOnTerminate: true}
	stubServiceProcesses(t, miner)
	var err error
	out := captureStdout(t, func() { err = stopWindowsProvider(windowsRoutingFixture(), true, false) })
	if err != nil {
		t.Fatalf("stopWindowsProvider: %v", err)
	}
	if c.serviceStop != 1 || !c.forcedStop || c.directStop != 0 {
		t.Fatalf("service stops = %d (forced %v), pid stops = %d; want one forced service stop and no pid stop", c.serviceStop, c.forcedStop, c.directStop)
	}
	if miner.terminated != 1 || len(miner.waits) == 0 || miner.waits[0] != 0 {
		t.Fatalf("miner terminated %d times after waits %v; want it ended without a grace period", miner.terminated, miner.waits)
	}
	if !strings.Contains(out, "force") || !strings.Contains(out, windowsProviderServiceName+" service") {
		t.Fatalf("output %q must say force went through the service", out)
	}
}

func TestWindowsForceRestartOwnedGoesThroughTheServiceQuickly(t *testing.T) {
	c := stubWindowsLifecycle(t, serviceOwned, nil)
	var err error
	out := captureStdout(t, func() { err = restartWindowsProvider(windowsRoutingFixture(), true, false) })
	if err != nil {
		t.Fatalf("restartWindowsProvider: %v", err)
	}
	if c.serviceStop != 1 || !c.forcedStop || c.serviceStart != 1 || c.directRestart != 0 {
		t.Fatalf("calls = %+v; want one forced service stop, one service start, no direct restart", *c)
	}
	if !strings.Contains(out, "force") {
		t.Fatalf("output %q must say force went through the service", out)
	}
}

// Without --force the stop keeps the graceful wait.
func TestWindowsStopOwnedWithoutForceWaitsGracefully(t *testing.T) {
	c := stubWindowsLifecycle(t, serviceOwned, nil)
	stubOwner(t, ownedWithMiner(4243))
	miner := &fakeServiceProcess{pid: 4243}
	stubServiceProcesses(t, miner)
	if err := stopWindowsProvider(windowsRoutingFixture(), false, false); err != nil {
		t.Fatalf("stopWindowsProvider: %v", err)
	}
	if c.forcedStop || len(miner.waits) == 0 || miner.waits[0] != serviceProcessExitGrace {
		t.Fatalf("forced %v, miner waits %v; want a graceful stop", c.forcedStop, miner.waits)
	}
}
