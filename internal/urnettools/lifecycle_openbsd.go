package urnettools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func openbsdRcctl(args ...string) ([]byte, error) {
	_, doasErr := exec.LookPath("doas")
	_, sudoErr := exec.LookPath("sudo")
	name, argv, err := openbsdRcctlArgv(os.Geteuid(), doasErr == nil, sudoErr == nil, args...)
	if err != nil {
		return nil, err
	}
	return bsdServiceRunFn(name, argv...)
}

func openbsdRequireScript(p Provider) (string, error) {
	service := bsdRcServiceName(p.Binary)
	path := openbsdRcScriptPath(service)
	if err := openbsdCheckScript(path, p.Binary); err != nil {
		return "", fmt.Errorf("rc.d script %s for provider %s: %w", path, providerLabel(p), err)
	}
	return service, nil
}

var openbsdServicePID = func() int {
	data, err := os.ReadFile("/var/run/rc.d/urnetwork")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		return -1
	}
	return openbsdManagedPID(string(data), func(table, pattern string) ([]byte, error) {
		out, err := execWithTimeout(context.Background(), 5*time.Second, "pgrep", "-T", table, "-xf", pattern)
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return out, err
	})
}

func openbsdRequireManaged(p Provider) error {
	if p.Supervisor != "openbsd-rcd" || p.PID <= 0 || openbsdServicePID() != p.PID {
		return fmt.Errorf("%s is not the uniquely matched rc.d-managed provider; refusing service action", providerLabel(p))
	}
	return nil
}

func bsdServiceControl(p Provider, action string) error {
	if p.Binary == "" {
		return fmt.Errorf("%s has no provider binary; refusing service action", providerLabel(p))
	}
	if p.Running || (action != "start" && action != "enable" && action != "disable") {
		if err := openbsdRequireManaged(p); err != nil {
			return err
		}
	} else if p.Supervisor != "openbsd-rcd" || p.PID != 0 || openbsdServicePID() != 0 {
		return fmt.Errorf("%s is not a stopped rc.d-managed provider", providerLabel(p))
	}
	service, err := openbsdRequireScript(p)
	if err != nil {
		return err
	}
	out, err := openbsdRcctl(action, service)
	if err != nil {
		return fmt.Errorf("rcctl %s %s: %w (%s)", action, service, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func setAutoStart(p Provider, on bool) error {
	action := "disable"
	if on {
		action = "enable"
	}
	return bsdServiceControl(p, action)
}

// Root's cron must not execute a path that an unprivileged user can replace.
func openbsdRootOwnedPath(name string) bool {
	if !filepath.IsAbs(name) {
		return false
	}
	for current := filepath.Clean(name); ; current = filepath.Dir(current) {
		fi, err := os.Lstat(current)
		if err != nil || fi.Mode().Perm()&0o022 != 0 {
			return false
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 {
			return false
		}
		if current == name {
			if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
				return false
			}
		} else if !fi.IsDir() {
			return false
		}
		if current == "/" {
			return true
		}
	}
}

func setAutoUpdateSchedule(p Provider, label, interval string) error {
	if os.Geteuid() != 0 && bsdTreeOwnedByRoot(p.Binary) {
		return fmt.Errorf("auto-update for %s must be scheduled as root", providerLabel(p))
	}
	if interval == "off" {
		return openbsdUpdateCron(p, "")
	}
	tool := "urnet-tools"
	if exe, err := os.Executable(); err == nil && exe != "" {
		tool = filepath.Clean(exe)
	}
	logDir := "/var/tmp"
	if home := homeForUser(currentUserName()); home != "" {
		logDir = filepath.Join(home, ".urnetwork")
	}
	trusted := openbsdRootOwnedPath(bsdRootToolPath)
	tool, logDir, err := bsdScheduledUpdate(os.Geteuid(), trusted, tool, logDir)
	if err != nil {
		return err
	}
	minute := 17
	if interval == "weekly" {
		minute, err = randomBSDCronMinute()
		if err != nil {
			return err
		}
	}
	spec, err := bsdCronSchedule(interval, minute)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	selector, err := bsdUpdateSelector(p)
	if err != nil {
		return err
	}
	line := spec + " " + bsdCronCommand(tool, logDir, selector...)
	return openbsdUpdateCron(p, line)
}

func openbsdUpdateCron(p Provider, line string) error {
	cmd := exec.Command("crontab", "-l")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && !strings.Contains(strings.ToLower(stderr.String()), "no crontab") {
		return fmt.Errorf("crontab -l: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	service := bsdRcServiceName(p.Binary)
	var kept []string
	changed := false
	for _, old := range strings.Split(string(out), "\n") {
		if matchBSDCronLine(old, service) {
			changed = true
		} else if strings.TrimSpace(old) != "" {
			kept = append(kept, old)
		}
	}
	if line != "" {
		kept = append(kept, line+" "+bsdCronMarker+" "+service)
		changed = true
	}
	if !changed {
		return nil
	}
	cmd = exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(strings.Join(kept, "\n") + "\n")
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("crontab install: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func cleanupLifecycle(p Provider) { _ = cleanupOpenBSDLifecycle(p) }

func cleanupOpenBSDLifecycle(p Provider) error {
	if p.Supervisor != "openbsd-rcd" {
		if p.Running {
			return stopUnitlessProvider(p, false)
		}
		return nil
	}
	service, err := openbsdRequireScript(p)
	if errors.Is(err, os.ErrNotExist) && p.Running {
		return fmt.Errorf("%s: service script is missing; refusing to remove a running provider", providerLabel(p))
	}
	if handled, cleanupErr := openbsdCleanupMissingScript(err, func() error { return openbsdUpdateCron(p, "") }); handled || cleanupErr != nil {
		return cleanupErr
	}
	stop, err := openbsdCleanupNeedsStop(p, openbsdServicePID())
	if err != nil {
		return err
	}
	if stop {
		if out, err := openbsdRcctl("stop", service); err != nil {
			return fmt.Errorf("rcctl stop %s: %w (%s)", service, err, strings.TrimSpace(string(out)))
		}
	}
	if out, err := openbsdRcctl("disable", service); err != nil {
		return fmt.Errorf("rcctl disable %s: %w (%s)", service, err, strings.TrimSpace(string(out)))
	}
	if err := openbsdUpdateCron(p, ""); err != nil {
		return fmt.Errorf("remove auto-update cron entry: %w", err)
	}
	return nil
}

func init() {
	cleanupLifecycleFn = cleanupOpenBSDLifecycle
	renderSystemctlStatus = func(p Provider) error {
		service, err := openbsdRequireScript(p)
		if err != nil {
			return err
		}
		cmd := exec.Command("rcctl", "check", service)
		cmd.Stdout = stdoutWriter()
		cmd.Stderr = stderrWriter()
		return cmd.Run()
	}
}
