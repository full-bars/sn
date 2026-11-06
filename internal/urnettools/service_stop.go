package urnettools

import (
	"fmt"
	"time"
)

// A supervisor that stopped does not prove its miner did. The miner may not
// be in the supervisor's job (assigning it can fail, and the supervisor runs
// on), and a supervisor ended after its graceful wait only takes the miner
// down through that job. So an owned stop holds every service process open
// from before the stop, which also keeps its id from being reused, and checks
// afterwards that each one ended. A miner still running is ended by its
// handle: the service owns it, so this cannot hit another provider. Any other
// process still running (an update check the supervisor started) is reported,
// not ended.

// serviceProcessHandle is one process held open across a stop.
type serviceProcessHandle interface {
	Pid() int
	// Exited waits up to wait for the process to end and reports whether it has.
	Exited(wait time.Duration) bool
	Terminate() error
	Close()
}

var (
	// openServiceProcessFn opens a process by pid; failures do not prove exit.
	openServiceProcessFn = openServiceProcess
	// how long a service process gets to end on its own after the supervisor
	// stopped, and how long a miner gets after it was ended
	serviceProcessExitGrace = 3 * time.Second
	minerTerminateWait      = 10 * time.Second
)

// stopOwnedProviderService stops the supervisor that owns p and proves that
// the provider processes under it ended. action names the command for the
// next-step hint. force gives the supervisor a short wait instead of the
// graceful one, and ends a miner still running without a grace period.
func stopOwnedProviderService(p Provider, owner providerServiceOwnership, action string, force bool) error {
	var held []serviceProcessHandle
	defer func() {
		for _, process := range held {
			process.Close()
		}
	}()
	for _, pid := range owner.ServicePids {
		process, err := openServiceProcessFn(pid)
		if err != nil {
			return fmt.Errorf("cannot verify service process %d before %s: %w", pid, action, err)
		}
		held = append(held, process)
	}
	grace := serviceProcessExitGrace
	if force {
		fmt.Printf("force: stopping the %s service without its graceful wait, then ending its miner if it still runs\n", windowsProviderServiceName)
		grace = 0
	}
	if err := stopProviderServiceFn(p, force); err != nil {
		return fmt.Errorf("stop the %s service: %w", windowsProviderServiceName, err)
	}
	var stillRunning []int
	for _, process := range held {
		if process.Exited(grace) {
			continue
		}
		pid := process.Pid()
		if pid != owner.MinerPid {
			stillRunning = append(stillRunning, pid)
			continue
		}
		fmt.Printf("the %s service stopped, but its miner (pid %d) is still running; ending it...\n", windowsProviderServiceName, pid)
		err := process.Terminate()
		if err == nil && process.Exited(minerTerminateWait) {
			fmt.Printf("ended the miner (pid %d)\n", pid)
			continue
		}
		if err == nil {
			err = fmt.Errorf("it is still running after %s", minerTerminateWait)
		}
		return fmt.Errorf("the %s service stopped, but its miner (pid %d) could not be ended: %v. End it in Task Manager, then run \"urnet-tools %s\" again", windowsProviderServiceName, pid, err, action)
	}
	if len(stillRunning) > 0 {
		return fmt.Errorf("the %s service stopped, but process %v it started is still running. Wait for it to finish or end it in Task Manager, then run \"urnet-tools %s\" again", windowsProviderServiceName, stillRunning, action)
	}
	return nil
}
