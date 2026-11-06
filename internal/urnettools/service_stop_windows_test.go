//go:build windows

package urnettools

import (
	"os/exec"
	"testing"
	"time"
)

// The real process handle: a miner that outlived its supervisor (here a
// throwaway ping) is ended through the handle taken before the stop. The
// service stop itself is stubbed; no real service is touched.
func TestOwnedStopEndsARealSurvivingProcess(t *testing.T) {
	child := exec.Command("ping", "-n", "60", "127.0.0.1")
	if err := child.Start(); err != nil {
		t.Skipf("can't start ping: %v", err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	stubStopService(t, func(Provider) error { return nil })
	grace := serviceProcessExitGrace
	serviceProcessExitGrace = 200 * time.Millisecond
	t.Cleanup(func() { serviceProcessExitGrace = grace })

	owner := testOwnership(serviceOwned)
	owner.SupervisorPid, owner.MinerPid, owner.ServicePids = 1, child.Process.Pid, []int{child.Process.Pid}
	if err := stopOwnedProviderService(windowsRoutingFixture(), owner, "stop"); err != nil {
		t.Fatalf("stopOwnedProviderService: %v", err)
	}
	if pidIsAlive(child.Process.Pid) {
		t.Fatal("the surviving process is still running")
	}
}
