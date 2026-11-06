package urnettools

import "fmt"

// windowsProviderServiceName is the supervisor's service name, the one
// "provider service install" saves and the Startup shortcut runs as
// "service run urnetwork-provider" (miner minerServiceDefaultName).
const windowsProviderServiceName = "urnetwork-provider"

// On Windows a provider is usually run by the per-user supervisor ("service
// run urnetwork-provider"), which starts its miner again whenever it exits.
// Stopping only the miner by pid therefore does not stop anything (the
// supervisor brings it back about 15 seconds later), and starting "provide"
// by hand runs a second provider beside the supervisor's. So plain start,
// stop and restart go through the supervisor when it owns the selected
// provider, and keep the direct process path for a provider it does not own.
// providerOwnedByServiceFn decides ownership (process ancestry for a running
// provider, the saved config's executable and state dir for a stopped one).
// When it cannot tell (the saved service or the process list could not be
// read), the command refuses rather than guess: either path could be wrong.

// Seams so the routing below can be tested on any platform.
var (
	cmdStopWindowsFn       = cmdStopWindows
	cmdStartWindowsFn      = cmdStartWindows
	cmdRestartWindowsFn    = cmdRestartWindows
	startProviderServiceFn = startProviderService
)

// stopWindowsProvider is "urnet-tools stop" on Windows.
func stopWindowsProvider(p Provider, force, dryRun bool) error {
	owner := providerOwnedByServiceFn(p)
	switch owner.State {
	case serviceOwnershipUnknown:
		return ownershipUnknownError("stop", p, owner)
	case serviceNotOwned:
		return cmdStopWindowsFn(p, force, dryRun)
	}
	fmt.Printf("stopping the %s service, which runs %s...\n", windowsProviderServiceName, providerLabel(p))
	if err := stopProviderServiceFn(p); err != nil {
		return fmt.Errorf("could not stop provider %s: stop the %s service: %w", providerLabel(p), windowsProviderServiceName, err)
	}
	fmt.Printf("stopped %s and its %s service. Run \"urnet-tools start\" to start it again.\n", providerLabel(p), windowsProviderServiceName)
	return nil
}

// startWindowsProvider is "urnet-tools start" on Windows.
func startWindowsProvider(p Provider, force, dryRun bool) error {
	owner := providerOwnedByServiceFn(p)
	switch owner.State {
	case serviceOwnershipUnknown:
		return ownershipUnknownError("start", p, owner)
	case serviceNotOwned:
		return cmdStartWindowsFn(p, force, dryRun)
	}
	fmt.Printf("starting the %s service, which runs %s...\n", windowsProviderServiceName, providerLabel(p))
	if err := startProviderServiceFn(p); err != nil {
		return fmt.Errorf("could not start provider %s: %w", providerLabel(p), err)
	}
	fmt.Printf("started %s through its %s service\n", providerLabel(p), windowsProviderServiceName)
	return nil
}

// restartWindowsProvider is "urnet-tools restart" on Windows. An owned
// provider restarts the supervisor as a whole, the same path update and tune
// use (restartProviderWindows).
func restartWindowsProvider(p Provider, force, dryRun bool) error {
	owner := providerOwnedByServiceFn(p)
	switch owner.State {
	case serviceOwnershipUnknown:
		return ownershipUnknownError("restart", p, owner)
	case serviceNotOwned:
		return cmdRestartWindowsFn(p, force, dryRun)
	}
	fmt.Printf("restarting the %s service, which runs %s...\n", windowsProviderServiceName, providerLabel(p))
	return restartProviderWindowsFn(p)
}
