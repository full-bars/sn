//go:build windows

package urnettools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/urfoundation/sn/internal/winservice"
)

// Windows lifecycle uses Task Scheduler (schtasks.exe) instead of systemd.
// The provider runs as a user-level process on Windows, so tasks are created
// without /ru (current user, runs only while logged on — no stored
// credentials, matching the provider's user-session model).

// setAutoStart enables or disables login auto-start for the provider by
// registering (or removing) a Task Scheduler task that runs on logon.
// The legacy Startup-folder .lnk is removed in BOTH directions so an
// upgrade never auto-starts twice and auto-start off actually stops it
// (heavyweight review S6).
func setAutoStart(p Provider, on bool) error {
	taskName := autoStartTaskName(p)
	if on {
		if p.Binary == "" {
			return fmt.Errorf("provider %s has no binary path", providerLabel(p))
		}
		// The provider starts with the "provide" subcommand.
		tr := fmt.Sprintf(`"%s" provide`, p.Binary)
		if err := runSchtasks("/create", "/f", "/tn", taskName, "/tr", tr, "/sc", "onlogon"); err != nil {
			return err
		}
		return removeLegacyStartupLnk()
	}
	if err := deleteTaskIfExists(taskName, runSchtasks); err != nil {
		return err
	}
	return removeLegacyStartupLnk()
}

// autoStartTaskName derives a per-provider autostart task name so two
// providers do not overwrite each other's task (heavyweight review S8).
func autoStartTaskName(p Provider) string {
	label := autoUpdateLabel(p)
	if label == "" || label == "urnetwork-update" {
		return "urnetwork-autostart"
	}
	return label + "-autostart"
}

// removeLegacyStartupLnk removes the pre-Go-tool Startup-folder shortcut so
// the schtasks mechanism is the only autostart.
func removeLegacyStartupLnk() error {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return nil
	}
	lnk := filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "urnetwork.lnk")
	if err := os.Remove(lnk); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// setAutoUpdateSchedule manages the auto-update scheduled task. label is the
// platform-neutral identifier; the concrete task name is fixed on Windows.
// Weekly is the default posture and maps to midnight Sunday.
func setAutoUpdateSchedule(p Provider, label, interval string) error {
	// Per-provider task name so two providers do not overwrite each other's
	// update task (heavyweight review S8). label is "<unit>-update" on
	// systemd-derived providers, else the fixed default.
	taskName := label
	if taskName == "" {
		taskName = "urnetwork-update"
	}
	switch interval {
	case "off":
		return deleteTaskIfExists(taskName, runSchtasks)
	case "daily":
		// -f: the task runs outside a shell with no TTY, so the update
		// confirm prompt would fail on EOF; force skips it.
		tr := fmt.Sprintf(`"%s" update -f`, toolExePath())
		return runSchtasks("/create", "/f", "/tn", taskName, "/tr", tr, "/sc", "daily", "/st", "00:00")
	case "weekly":
		tr := fmt.Sprintf(`"%s" update -f`, toolExePath())
		return runSchtasks("/create", "/f", "/tn", taskName, "/tr", tr, "/sc", "weekly", "/d", "SUN", "/st", "00:00")
	case "monthly":
		// Explicit /d 1 so "monthly" means the 1st, not schtasks' silent
		// day-1 default.
		tr := fmt.Sprintf(`"%s" update -f`, toolExePath())
		return runSchtasks("/create", "/f", "/tn", taskName, "/tr", tr, "/sc", "monthly", "/d", "1", "/st", "00:00")
	}
	return fmt.Errorf("invalid interval %q", interval)
}

// toolExePath returns the running tool's absolute path, which the scheduled
// task uses as its executable (the task runs outside a shell, so PATH is not
// available).
func toolExePath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Clean(exe)
	}
	return "urnet-tools.exe"
}

// runSchtasks executes schtasks.exe with the given args and wraps failures
// with the captured output. The subcommand MUST carry the leading slash
// (schtasks /create, /delete, /query) — `schtasks create` is invalid
// (caught by live Windows test 2026-08-16).
func runSchtasks(subcommand string, args ...string) error {
	full := append([]string{subcommand}, args...)
	out, err := exec.Command("schtasks", full...).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		if subcommand == "/query" && schtasksTaskNotFoundOutput(message) {
			return fmt.Errorf("%w: schtasks %s: %v (%s)", errScheduledTaskNotFound, subcommand, err, message)
		}
		return fmt.Errorf("schtasks %s: %w (%s)", subcommand, err, message)
	}
	return nil
}

// cleanupLifecycle removes the Windows scheduled tasks on uninstall. A task
// that survives uninstall keeps launching a binary that is being removed, so
// a failed delete is reported on stderr rather than dropped.
func cleanupLifecycle(p Provider) {
	for _, taskName := range []string{"urnetwork-update", "urnetwork-autostart"} {
		if err := deleteTaskIfExists(taskName, runSchtasks); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove scheduled task %q: %v\n", taskName, err)
		}
	}
}

// removeProviderServiceFiles deletes the supervisor's Startup shortcut and its
// saved config and lock. Uninstall calls it only for a provider the service
// owns (providerOwnedByService); another provider's supervisor keeps them.
// Left behind, the shortcut starts a supervisor at the
// next sign-in whose executable is gone. The supervisor is already stopped
// (stopProviderService), as winservice.Remove requires.
func removeProviderServiceFiles() {
	shortcut, err := winservice.ShortcutPath(windowsProviderServiceName)
	if err == nil {
		if rmErr := os.Remove(shortcut); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = rmErr
		} else {
			err = nil
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove the startup shortcut: %v\n", err)
	}
	if err := winservice.Remove(windowsProviderServiceName); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove the service config: %v\n", err)
	}
}

// windowsServiceStopWait mirrors the miner's own stop wait: the supervisor
// gives its miner 30 seconds to end.
const windowsServiceStopWait = 45 * time.Second

// stopProviderService stops the per-user supervisor and, through its job
// object, the miner it runs. Callers check providerOwnedByService first. Killing only the miner is not enough: the
// supervisor starts it again 15 seconds later. winservice.Stop works by
// service name and checks the supervisor's lock, so it never touches another
// process, and it does nothing when no supervisor is running. The separate
// Windows service "urnetworkd" is not involved.
func stopProviderService(_ Provider) error {
	return winservice.Stop(windowsProviderServiceName, windowsServiceStopWait)
}

// providerOwnedByService reports whether the saved provider service owns the
// selected provider, so a lifecycle command or uninstall of the service
// cannot hit an unrelated supervisor. No saved service owns nothing; a lookup
// that failed in any other way leaves ownership unknown.
func providerOwnedByService(p Provider) providerServiceOwnership {
	var l serviceLookup
	config, err := loadWindowsProviderService(windowsProviderServiceName)
	if err != nil {
		if windowsPathMissing(err) {
			l.NotInstalled = true
		} else {
			l.LoadErr = err
		}
		return classifyServiceOwnership(p, l)
	}
	l.Executable, l.StateDir = config.Executable, config.StateDir
	l.LogPath = filepath.Join(config.StateDir, winservice.LogName)
	l.SupervisorPid, l.SupervisorErr = windowsServiceSupervisorPid(windowsProviderServiceName)
	if l.SupervisorErr == nil && l.SupervisorPid > 0 {
		l.Processes, l.ProcessesErr = windowsProcessSnapshot()
		l.RecordedMinerPid = windowsServiceMinerPid(windowsProviderServiceName)
	}
	return classifyServiceOwnership(p, l)
}

// The saved service lookups, replaced by tests so they never read or create
// files in the real profile folder (Running creates the lock file) or depend
// on the machine's processes.
var (
	loadWindowsProviderService  = winservice.Load
	windowsServiceSupervisorPid = winservice.SupervisorPid
	windowsProcessSnapshot      = processSnapshot
	windowsServiceMinerPid      = winservice.MinerPid
)

// processSnapshot reads every process's parent id and image name from a
// toolhelp snapshot.
func processSnapshot() (map[int]serviceProcessEntry, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	out := map[int]serviceProcessEntry{}
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		out[int(entry.ProcessID)] = serviceProcessEntry{Parent: int(entry.ParentProcessID), Name: windows.UTF16ToString(entry.ExeFile[:])}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	return out, nil
}

// stopBareProvider stops a running provider the service does not own. It goes
// by pid: the control socket is shared by every provider of this user, so a
// graceful shutdown request could reach the other provider instead.
func stopBareProvider(p Provider) error {
	if !p.Running || p.PID <= 0 {
		return nil
	}
	return cmdStopWindows(p, true, false)
}

// windowsServiceProcess is a service process held open by handle. While the
// handle is open the process id can't be reused, so Terminate can only hit
// the process that was seen under the supervisor.
type windowsServiceProcess struct {
	pid    int
	handle windows.Handle
}

func openServiceProcess(pid int) (serviceProcessHandle, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return nil, err
	}
	return &windowsServiceProcess{pid: pid, handle: handle}, nil
}

func (self *windowsServiceProcess) Pid() int { return self.pid }

func (self *windowsServiceProcess) Exited(wait time.Duration) bool {
	event, err := windows.WaitForSingleObject(self.handle, uint32(wait/time.Millisecond))
	return err == nil && event == windows.WAIT_OBJECT_0
}

func (self *windowsServiceProcess) Terminate() error {
	return windows.TerminateProcess(self.handle, 1)
}

func (self *windowsServiceProcess) Close() { windows.CloseHandle(self.handle) }
