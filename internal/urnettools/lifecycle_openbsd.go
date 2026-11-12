package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func openbsdRcctl(args ...string) ([]byte, error) {
	_, doasErr := exec.LookPath("doas")
	_, configErr := os.Stat("/etc/doas.conf")
	_, sudoErr := exec.LookPath("sudo")
	name, argv, err := openbsdRcctlArgv(os.Geteuid(), doasErr == nil && configErr == nil, sudoErr == nil, args...)
	if err != nil {
		return nil, err
	}
	return bsdServiceRunFn(name, argv...)
}

func openbsdRequireScript(p Provider) (string, error) {
	service := bsdRcServiceName(p.Binary)
	if _, err := os.Stat(openbsdRcScriptPath(service)); err != nil {
		return "", fmt.Errorf("no rc.d script at %s for provider %s: %w", openbsdRcScriptPath(service), providerLabel(p), err)
	}
	return service, nil
}

func bsdServiceControl(p Provider, action string) error {
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
			if !fi.Mode().IsRegular() {
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
	if interval == "off" {
		return openbsdUpdateCron(p, "")
	}
	if os.Geteuid() != 0 && bsdTreeOwnedByRoot(p.Binary) {
		return fmt.Errorf("auto-update for %s must be scheduled as root", providerLabel(p))
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
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	line := fmt.Sprintf("%s %s update -f >> %s 2>&1", spec, quote(tool), quote(filepath.Join(logDir, "auto-update.log")))
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

func cleanupLifecycle(p Provider) {
	if err := openbsdUpdateCron(p, ""); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: could not remove auto-update cron entry: %v\n", err)
	}
	service, err := openbsdRequireScript(p)
	if err != nil {
		return
	}
	script, err := os.ReadFile(openbsdRcScriptPath(service))
	if err != nil || !openbsdScriptServesBinary(string(script), p.Binary) {
		return
	}
	for _, action := range []string{"stop", "disable"} {
		if out, err := openbsdRcctl(action, service); err != nil {
			fmt.Fprintf(os.Stderr, "uninstall: rcctl %s %s: %v (%s)\n", action, service, err, strings.TrimSpace(string(out)))
		}
	}
}

func init() {
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
