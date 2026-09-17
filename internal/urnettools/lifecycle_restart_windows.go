//go:build windows

package urnettools

import (
	"fmt"
	"path/filepath"
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

// restartProviderWindows restarts the provider during an update. Unlike
// cmdRestartWindows, it does NOT attempt HotSwap — updateProvider already
// tried that and failed. This performs only stop + start to avoid the
// double-hotswap / state-tracking bypass that routing back to
// cmdRestartWindows would cause.
func restartProviderWindows(p Provider) error {
	return stopStartFallback(p, false, false)
}
