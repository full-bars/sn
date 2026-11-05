//go:build windows

package urnettools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/urfoundation/sn/internal/winservice"
)

// cmdRestartWindows restarts the provider on Windows. HotSwap is not
// yet supported on Windows (see hotswap_windows.go triggerHotSwap stub),
// so this always goes through stop + start. When force is true the stop
// phase uses TerminateProcess directly instead of the 15-second graceful
// shutdown timeout.
func cmdRestartWindows(p Provider, force, dryRun bool) error {
	return stopStartFallback(p, force, dryRun)
}

// stopStartFallback stops the provider and starts it again.
func stopStartFallback(p Provider, force, dryRun bool) error {
	fmt.Printf("stopping %s...\n", providerLabel(p))
	if err := cmdStopWindows(p, force, dryRun); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	fmt.Printf("starting %s...\n", providerLabel(p))
	if err := cmdStartWindows(p, force, dryRun); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	fmt.Printf("restarted %s\n", providerLabel(p))
	return nil
}

// providerSocketPath returns the control socket path for a provider.
func providerSocketPath(p Provider) string {
	return filepath.Join(p.StateDir, "provider.sock")
}

// restartProviderWindows restarts the provider during an update or tuning
// change. The provider is a per-user supervisor ("service run
// urnetwork-provider") that restarts its miner whenever it dies, so stopping
// only the miner (what stopStartFallback does by PID) races the supervisor,
// and launching "provide" by hand starts a second provider. Restart the
// supervisor as a whole, the same way "provider service restart" does.
//
// A provider that was not started by the supervisor has no saved service
// config; there is nothing safe to restart, so say so.
func restartProviderWindows(p Provider) error {
	config, err := winservice.Load(windowsProviderServiceName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no provider service is installed for %s; restart it manually", providerLabel(p))
		}
		return fmt.Errorf("read the provider service config: %w", err)
	}
	if err := stopProviderService(p); err != nil {
		return fmt.Errorf("stop the provider service: %w", err)
	}
	if err := winservice.Launch(config.Executable, "service", "run", windowsProviderServiceName); err != nil {
		return fmt.Errorf("start the provider service: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if running, _ := winservice.Running(windowsProviderServiceName); running {
			fmt.Printf("restarted %s\n", providerLabel(p))
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the provider service did not start; see %s", filepath.Join(config.StateDir, winservice.LogName))
		}
		time.Sleep(200 * time.Millisecond)
	}
}
