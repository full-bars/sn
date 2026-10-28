//go:build linux

package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// setAutoStart enables or disables login auto-start for the provider's
// owning systemd unit. On Linux/macOS the provider runs as a systemd (or
// launchd) unit, so this is the systemctl enable/disable path.
func setAutoStart(p Provider, on bool) error {
	if p.Unit == "" {
		return fmt.Errorf("provider %s has no owning unit", providerLabel(p))
	}
	action := "disable"
	if on {
		action = "enable"
	}
	if isUserUnit(p.Unit) && p.User != "" {
		// When enabling a user unit, ensure the user session manager starts
		// at boot. Without loginctl enable-linger, a non-login service
		// account's user manager does not start at boot, and systemctl
		// enable reports success but the unit never runs. The linger check
		// is only relevant on the enable path — disabling does not need it.
		if on {
			if linger, err := execWithTimeout(5*time.Second, "loginctl", "show-user", p.User, "-p", "Linger", "--value"); err == nil {
				if strings.TrimSpace(string(linger)) != "yes" {
					return fmt.Errorf("linger is not enabled for %s; run sudo loginctl enable-linger %s before enabling this unit", p.User, p.User)
				}
			} else {
				fmt.Fprintf(os.Stderr, "setAutoStart: warning: cannot check linger for %s: %v\n", p.User, err)
			}
		}
		args := append(systemctlUserArgs(p.User), action, p.Unit)
		return exec.Command("systemctl", args...).Run()
	}
	return exec.Command("systemctl", action, p.Unit).Run()
}

// setAutoUpdateSchedule manages the auto-update systemd timer. label is the
// platform-neutral identifier ("<unit>-update" or "urnetwork-update"); the
// concrete timer unit name is derived from the provider unit.
func setAutoUpdateSchedule(p Provider, label, interval string) error {
	if p.Unit == "" {
		return fmt.Errorf("provider %s has no owning unit", providerLabel(p))
	}
	timer := strings.TrimSuffix(p.Unit, ".service") + "-update.timer"
	// Derive scope from the PROVIDER's unit, not the timer name.
	// On a fresh box the timer doesn't exist yet, so isUserUnit(timer)
	// always returns true — even for system-scoped providers.
	userScope := isUserUnit(p.Unit) && p.User != ""
	switch interval {
	case "off":
		// The deterministic part is the FILE removal — a disabled-but-present
		// timer file would keep firing. A failed disable (e.g. no session bus
		// on a CI runner) is logged, not propagated: the caller cares that the
		// timer is gone, and it is. The systemd manager state resolves on the
		// next daemon-reload even when disable could not reach it now.
		// Stop and disable the existing timer. If systemctl fails, do NOT
		// remove the timer file — a failed disable leaves the timer active
		// and removing the file would orphan a running timer.
		if userScope {
			args := append(systemctlUserArgs(p.User), "disable", "--now", timer)
			if err := exec.Command("systemctl", args...).Run(); err != nil {
				return fmt.Errorf("auto-update off: disable %s: %w", timer, err)
			}
		} else if err := exec.Command("systemctl", "disable", "--now", timer).Run(); err != nil {
			return fmt.Errorf("auto-update off: disable %s: %w", timer, err)
		}
		return removeTimerFile(p, timer, userScope)
	case "daily":
		return writeTimerCalendar(timer, p, "daily", userScope)
	case "weekly":
		return writeTimerCalendar(timer, p, "Sun *-*-* 00:00:00 UTC", userScope)
	case "monthly":
		return writeTimerCalendar(timer, p, "monthly", userScope)
	}
	return fmt.Errorf("invalid interval %q", interval)
}

// removeTimerFile deletes the timer unit file for a provider, mirroring the
// shell wrapper's `rm -f` on auto-update off (Provider_Install_Linux.sh).
// A missing file is not an error.
func removeTimerFile(p Provider, timer string, userScope bool) error {
	var path string
	if userScope {
		home := homeForUser(p.User)
		if home == "" {
			return fmt.Errorf("cannot resolve home for user %s", p.User)
		}
		path = filepath.Join(home, ".config/systemd/user", timer)
	} else {
		path = "/etc/systemd/system/" + timer
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// timerUnitContents renders a new timer or updates the calendar and fleet jitter
// of an existing timer. RandomizedDelaySec caps the added delay after a scheduled
// firing at six hours, keeping the weekly cadence predictable for operators.
func timerUnitContents(timer, calendar string, existing []byte) ([]byte, error) {
	if existing == nil {
		content := fmt.Sprintf("[Unit]\nDescription=Run URnetwork Update\n\n[Timer]\nOnCalendar=%s\nRandomizedDelaySec=6h\nPersistent=true\nUnit=%s.service\n\n[Install]\nWantedBy=default.target\n", calendar, strings.TrimSuffix(timer, ".timer"))
		return []byte(content), nil
	}
	updateUnit := strings.TrimSuffix(timer, ".timer") + ".service"
	lines := strings.Split(string(existing), "\n")
	var out []string
	replaced := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "OnCalendar="):
			out = append(out, "OnCalendar="+calendar, "RandomizedDelaySec=6h")
			replaced = true
		case strings.HasPrefix(line, "RandomizedDelaySec="):
			// replaced beside OnCalendar above
		case strings.HasPrefix(line, "Unit=") && strings.TrimSpace(line) != "Unit="+updateUnit:
			// A timer this tool wrote earlier, or one the shell installer
			// wrote, restarts the provider instead of running the updater.
			out = append(out, "Unit="+updateUnit)
		default:
			out = append(out, line)
		}
	}
	if !replaced {
		return nil, fmt.Errorf("no OnCalendar line found in %s", timer)
	}
	return []byte(strings.Join(out, "\n")), nil
}

// writeTimerCalendar rewrites a timer unit's OnCalendar line and reloads. If
// the timer file does not exist yet (fresh install — the Go tool, unlike the
// shell installer, may be the first thing to set auto-update), it is CREATED
// with the same [Unit]/[Timer]/[Install] shape the shell wrapper writes at
// install time (Provider_Install_Linux.sh:685).
func writeTimerCalendar(timer string, p Provider, calendar string, userScope bool) error {
	// Locate the timer unit file (system or user).
	var path string
	if userScope {
		home := homeForUser(p.User)
		if home == "" {
			return fmt.Errorf("cannot resolve home for user %s", p.User)
		}
		path = filepath.Join(home, ".config/systemd/user", timer)
	} else {
		path = "/etc/systemd/system/" + timer
	}
	return writeTimerCalendarAt(path, timer, p, calendar)
}

func writeTimerCalendarAt(path, timer string, p Provider, calendar string) error {
	updateUnit := strings.TrimSuffix(timer, ".timer") + ".service"
	// The updater service is made (or repaired) whether or not the timer
	// exists: a timer this tool wrote earlier points at the provider unit.
	if err := ensureUpdaterService(filepath.Join(filepath.Dir(path), updateUnit), updateUnit, p); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		content, err := timerUnitContents(timer, calendar, nil)
		if err != nil {
			return err
		}
		if err := writeTimerUnitAtomic(path, string(content)); err != nil {
			return fmt.Errorf("create timer %s: %w", timer, err)
		}
		return enableTimer(p, timer)
	} else if err != nil {
		return fmt.Errorf("read timer %s: %w", timer, err)
	}
	content, err := timerUnitContents(timer, calendar, data)
	if err != nil {
		return err
	}
	if err := writeTimerUnitAtomic(path, string(content)); err != nil {
		return err
	}
	return enableTimer(p, timer)
}

// ensureUpdaterService writes <name>-update.service when it is missing, and
// rewrites it when its ExecStart is not an update invocation (the shell
// installer's `urnet-tools update -y` counts as one).
func ensureUpdaterService(path, updateUnit string, p Provider) error {
	existing, err := os.ReadFile(path)
	if err == nil && updaterServiceUpdates(string(existing)) {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("check updater service %s: %w", updateUnit, err)
	}
	executable, err := updaterExecutable(p)
	if err != nil {
		return err
	}
	service := fmt.Sprintf(`[Unit]
Description=URnetwork Update

[Service]
Type=oneshot
ExecStart=%s update -y --unit %s
`, systemdExecArg(executable), systemdExecArg(p.Unit))
	// writeFileAtomic does not create the directory, and on a fresh
	// ~/.config/systemd/user it is not there yet.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create updater service %s: %w", updateUnit, err)
	}
	if err := writeFileAtomic(path, []byte(service), 0o644); err != nil {
		return fmt.Errorf("create updater service %s: %w", updateUnit, err)
	}
	return nil
}

// updaterExecutable picks the urnet-tools the updater service runs. For a
// provider in a user's systemd manager the service runs as that user, so a
// copy only the operator can reach (a 0700 home, run by root with --unit)
// would fail 203/EXEC on every tick with nothing to show for it: prefer the
// tool installed beside the provider binary, then this binary if the user
// can run it, and otherwise say so. The system scope keeps this binary.
func updaterExecutable(p Provider) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve updater executable: %w", err)
	}
	if !isUserUnit(p.Unit) || p.User == "" {
		return self, nil
	}
	account, err := user.Lookup(p.User)
	if err != nil {
		return "", fmt.Errorf("resolve updater executable: look up %s: %w", p.User, err)
	}
	var candidates []string
	if p.Binary != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(p.Binary), "urnet-tools"))
	}
	if home := homeForUser(p.User); home != "" {
		candidates = append(candidates, filepath.Join(home, ".local/share/urnetwork-provider/bin/urnet-tools"))
	}
	candidates = append(candidates, self)
	for _, candidate := range candidates {
		if executableBy(account, candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("the updater service runs as %s, who cannot execute %s or an installed urnet-tools; install urnet-tools for that user (or run auto-update as them) and retry", p.User, self)
}

// executableBy reports whether account can run path: a regular file with the
// matching execute bit, under directories it can search.
func executableBy(account *user.User, path string) bool {
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return false
	}
	gids := map[int]bool{}
	if gid, err := strconv.Atoi(account.Gid); err == nil {
		gids[gid] = true
	}
	if groups, err := account.GroupIds(); err == nil {
		for _, group := range groups {
			if gid, err := strconv.Atoi(group); err == nil {
				gids[gid] = true
			}
		}
	}
	allowed := func(info os.FileInfo, bit os.FileMode) bool {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return false
		}
		mode := info.Mode().Perm()
		switch {
		case int(stat.Uid) == uid:
			return mode&(bit<<6) != 0
		case gids[int(stat.Gid)]:
			return mode&(bit<<3) != 0
		default:
			return mode&bit != 0
		}
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || !allowed(info, 1) {
		return false
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err != nil || !allowed(info, 1) {
			return false
		}
		if dir == filepath.Dir(dir) {
			return true
		}
	}
}

func updaterServiceUpdates(service string) bool {
	for _, line := range strings.Split(service, "\n") {
		if strings.HasPrefix(line, "ExecStart=") && strings.Contains(line, " update -y") {
			return true
		}
	}
	return false
}

func systemdExecArg(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, "$", "$$")
	return strconv.Quote(value)
}

// enableTimer runs daemon-reload + enable --now for a timer unit, choosing
// the user or system manager from the provider's unit placement.
func enableTimer(p Provider, timer string) error {
	if isUserUnit(p.Unit) && p.User != "" {
		args := append(systemctlUserArgs(p.User), "daemon-reload")
		_ = exec.Command("systemctl", args...).Run()
		args = append(systemctlUserArgs(p.User), "enable", "--now", timer)
		return exec.Command("systemctl", args...).Run()
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
	return exec.Command("systemctl", "enable", "--now", timer).Run()
}

// cleanupLifecycle on Unix disables the auto-update timer. The unit disable
// in cmdUninstall handles the service, but the <unit>-update.timer would
// keep firing for a provider that is gone (heavyweight review S7). On an
// OpenRC host the cleanup is TARGET-AWARE: the service artifacts are removed
// only when the uninstall targets the service's own supervised provider (or
// no service exists at all, in which case only the periodic entry — which is
// not service-bound — is swept). Uninstalling a bare provider while the
// service is installed must leave the service untouched.
func cleanupLifecycle(p Provider) {
	if openrcActive() {
		switch {
		case p.Unit == "" && (p.Supervisor == "openrc" || providerSupervisedByOpenRCFn(p)):
			// The service's own supervised provider: full cleanup.
			openrcCleanup()
		case !openrcServiceInstalled():
			// No service exists; the periodic auto-update entry (not
			// service-bound) still must not outlive the install.
			openrcCleanupCronEntries()
		}
		// Fall through: a provider that ALSO has a systemd unit (migration
		// edge) must still get that unit's timer disabled below. The normal
		// OpenRC case has p.Unit == "" and returns immediately.
	}
	if p.Unit == "" {
		return
	}
	timer := strings.TrimSuffix(p.Unit, ".service") + "-update.timer"
	if isUserUnit(p.Unit) && p.User != "" {
		args := append(systemctlUserArgs(p.User), "disable", "--now", timer)
		_ = systemdTimerDisableFn(args...)
		return
	}
	_ = systemdTimerDisableFn("disable", "--now", timer)
}

// systemdTimerDisableFn runs one systemctl command for the auto-update timer
// cleanup. Seam for tests so unit tests never execute a real systemctl.
var systemdTimerDisableFn = func(args ...string) error {
	return exec.Command("systemctl", args...).Run()
}

// renderSystemctlStatus reproduces the pre-rewrite Linux `status` behavior:
// runs `systemctl status <unit>` (user or system scope) exactly as the old
// tool did (`systemctl --user status urnetwork.service`), giving the full
// systemd view (load, ActiveState, uptime, memory, tasks, unit path).
func init() {
	renderSystemctlStatus = renderSystemctlStatusLinux
}

func renderSystemctlStatusLinux(p Provider) error {
	if p.Unit == "" {
		return fmt.Errorf("provider %s has no owning unit (bare process)", providerLabel(p))
	}
	var args []string
	if isUserUnit(p.Unit) && p.User != "" {
		args = append(systemctlUserArgs(p.User), "status", p.Unit)
	} else {
		args = []string{"status", p.Unit}
	}
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}
