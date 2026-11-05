//go:build windows

package urnettools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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
// config, and one that runs beside a saved service is not that service's
// miner; restarting the service would bounce a different provider and leave
// the selected one on its old image. Either way there is nothing safe to
// restart, so say so. Every failure carries restartProvider's "could not
// restart provider" wording, the same as the other platforms.
func restartProviderWindows(p Provider) error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("could not restart provider %s: %s", providerLabel(p), fmt.Sprintf(format, a...))
	}
	// Ownership needs a running pid or a binary path to match. Without
	// either nothing ties the provider to the service, so decide that
	// before touching the machine's service state.
	if p.PID <= 0 && p.Binary == "" {
		return fail("no process or binary ties it to the %s service; restart it manually", windowsProviderServiceName)
	}
	config, err := winservice.Load(windowsProviderServiceName)
	if err != nil {
		if windowsPathMissing(err) {
			return fail("no provider service is installed; restart it manually")
		}
		return fail("read the provider service config: %v", err)
	}
	if !providerOwnedByConfig(p, config.Executable) {
		return fail("it is not managed by the %s service; restart it manually", windowsProviderServiceName)
	}
	if err := stopProviderService(p); err != nil {
		return fail("stop the provider service: %v", err)
	}
	if err := winservice.Launch(config.Executable, "service", "run", windowsProviderServiceName); err != nil {
		return fail("start the provider service: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if running, _ := winservice.Running(windowsProviderServiceName); running {
			fmt.Printf("restarted %s\n", providerLabel(p))
			return nil
		}
		if time.Now().After(deadline) {
			return fail("the provider service did not start; see %s", filepath.Join(config.StateDir, winservice.LogName))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// windowsPathMissing reports a missing file or folder. Besides os.ErrNotExist
// it matches the HRESULT forms (0x80070002, 0x80070003) that known-folder
// lookups return: errors.Is does not map those to os.ErrNotExist, so a
// missing profile folder used to read as a damaged service config.
func windowsPathMissing(err error) bool {
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == 0x80070002 || errno == 0x80070003
	}
	return false
}
