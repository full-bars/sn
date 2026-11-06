package urnettools

import (
	"fmt"
	"time"
)

// A supervisor that holds its lock has not yet started a miner, and a miner
// that needs setup exits at once and takes the supervisor with it. So start
// and restart of an owned provider report success only once the provider is
// up: its control socket answers, or a miner runs under the supervisor.

// providerUpState is one look at a starting service.
type providerUpState struct {
	Up             bool
	ServiceRunning bool
}

var (
	providerServiceUpFn = providerServiceUp
	providerUpWait      = 30 * time.Second
	providerUpPoll      = 500 * time.Millisecond
)

// observeProviderServiceUp decides one look at a starting service from the
// supervisor pid, the process table and the miner pid the supervisor
// recorded. A control socket that answers is not evidence: a bare provider on
// the same state dir answers it too, so a restart could report the service up
// while only that other provider runs. Up needs a provider process under the
// supervisor (the recorded miner; with an older supervisor that records
// none, any provider process under it).
func observeProviderServiceUp(owner providerServiceOwnership, supervisorPid int, processes map[int]serviceProcessEntry, recordedMiner int) providerUpState {
	found := classifyServiceOwnership(Provider{PID: supervisorPid, Running: true}, serviceLookup{
		Executable: owner.Executable, StateDir: owner.StateDir,
		SupervisorPid: supervisorPid, Processes: processes, RecordedMinerPid: recordedMiner,
	})
	up := found.MinerPid > 0 || (recordedMiner <= 0 && len(found.ServicePids) > 0)
	return providerUpState{Up: up, ServiceRunning: true}
}

// waitProviderServiceUp waits up to providerUpWait for the provider the
// service runs to come up.
func waitProviderServiceUp(p Provider, owner providerServiceOwnership) error {
	fmt.Printf("the %s service is running; waiting up to %s for %s to come up...\n", windowsProviderServiceName, providerUpWait, providerLabel(p))
	deadline := time.Now().Add(providerUpWait)
	for {
		state := providerServiceUpFn(owner)
		if state.Up {
			return nil
		}
		if !state.ServiceRunning {
			return fmt.Errorf("the %s service stopped before %s came up (a device that needs setup does this); see the service log at %s", windowsProviderServiceName, providerLabel(p), owner.LogPath)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the %s service is running, but %s did not come up within %s. The service keeps trying; see the service log at %s, then run \"urnet-tools status\"", windowsProviderServiceName, providerLabel(p), providerUpWait, owner.LogPath)
		}
		time.Sleep(providerUpPoll)
	}
}
