package urnettools

// Execution coverage for the OpenRC elevation paths. The installer can set
// SUDO=sudo for a non-root caller whose sudo works non-interactively, but the
// stop and teardown paths only acted when the caller's UID was already root.
// A forced update therefore left the old process running against a replaced
// binary, and an uninstall run by the service user removed the install tree
// while leaving the service enabled to respawn a provider with no binary.
//
// These cases execute the REAL functions with a stub sudo on PATH.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sudoRecorder builds a directory containing a `sudo` stub that appends every
// invocation to a file, so the test can see which privileged commands the
// installer actually issued.
func sudoRecorder(t *testing.T) (dir, record string) {
	t.Helper()
	dir = t.TempDir()
	record = filepath.Join(dir, "sudo-calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + record + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The teardown also calls rc-service/rc-update directly when SUDO is empty;
	// stub them so a root-path run does not touch the host.
	for _, name := range []string{"rc-service", "rc-update"} {
		body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + record + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir, record
}

// TestOpenRCTeardownElevatesWithWorkingSudo pins the uninstall half of the
// finding: with SUDO=sudo and a working sudo, the teardown must run its
// privileged commands through sudo instead of only printing them.
func TestOpenRCTeardownElevatesWithWorkingSudo(t *testing.T) {
	skipWithoutPOSIXShell(t)
	fn := extractShellFunc(t, readInstaller(t), "openrc_teardown_service")
	if fn == "" {
		t.Fatal("could not extract openrc_teardown_service from the Linux installer")
	}

	dir, record := sudoRecorder(t)
	home := t.TempDir()

	out, err := runShellScriptWithPath(t, dir, `
SUDO=sudo
install_path="/usr/local/lib/urnetwork-provider"
openrc_initd_file="/etc/init.d/urnetwork"
openrc_user_home() { printf '%s\n' "`+home+`"; }
pr_info() { :; }
pr_warn() { printf 'WARN: %s\n' "$*"; }
pr_err() { printf 'ERROR: %s\n' "$*"; }
`+fn+`
openrc_teardown_service
`)
	if err != nil {
		t.Fatalf("teardown failed: %v (%s)", err, out)
	}

	calls, readErr := os.ReadFile(record)
	if readErr != nil {
		t.Fatalf("sudo was never invoked, so the service was not torn down: %s", out)
	}
	got := string(calls)
	for _, want := range []string{"rc-service urnetwork stop", "rc-update del urnetwork default"} {
		if !strings.Contains(got, want) {
			t.Errorf("teardown did not run %q through sudo; got:\n%s", want, got)
		}
	}
	if strings.Contains(out, "Removing the system service needs root") {
		t.Error("the teardown bailed out to the manual-instructions path even though " +
			"sudo worked non-interactively")
	}
}

// TestOpenRCStopUnitsElevatesWithWorkingSudo pins the update half: a forced
// update by a non-root caller with working sudo must actually stop the service
// rather than leave it running against a replaced binary.
func TestOpenRCStopUnitsElevatesWithWorkingSudo(t *testing.T) {
	skipWithoutPOSIXShell(t)
	fn := extractShellFunc(t, readInstaller(t), "stop_openrc_units")
	if fn == "" {
		t.Fatal("could not extract stop_openrc_units from the Linux installer")
	}

	dir, record := sudoRecorder(t)

	out, err := runShellScriptWithPath(t, dir, `
SUDO=sudo
FORCE=1
operation=update
openrc_service_running() { return 0; }
openrc_pause_update_entry() { :; }
confirm_restart() { :; }
pr_info() { printf 'INFO: %s\n' "$*"; }
pr_warn() { printf 'WARN: %s\n' "$*"; }
pr_err() { printf 'ERROR: %s\n' "$*"; }
`+fn+`
stop_openrc_units
printf 'STOPPED_FLAG=%s\n' "${openrc_units_stopped:-unset}"
`)
	if err != nil {
		t.Fatalf("stop_openrc_units failed: %v (%s)", err, out)
	}

	calls, readErr := os.ReadFile(record)
	got := ""
	if readErr == nil {
		got = string(calls)
	}
	if !strings.Contains(got, "rc-service urnetwork stop") {
		t.Errorf("stop_openrc_units did not stop the service through sudo; got:\n%s", got)
	}
	if !strings.Contains(out, "STOPPED_FLAG=1") {
		t.Errorf("openrc_units_stopped must be set so the rollback and restart paths "+
			"know the service was stopped; output:\n%s", out)
	}
	if strings.Contains(out, "the installer cannot restart it as a non-root user") {
		t.Error("stop_openrc_units bailed out to the non-root message even though " +
			"sudo worked non-interactively")
	}
}

// TestOpenRCLifecycleStillGuardedWithoutSudo pins the other half: with no usable
// sudo the privileged work must NOT be attempted, because a silent failure
// looks like success. The installer must still say what to run by hand.
func TestOpenRCLifecycleStillGuardedWithoutSudo(t *testing.T) {
	skipWithoutPOSIXShell(t)
	fn := extractShellFunc(t, readInstaller(t), "openrc_teardown_service")
	if fn == "" {
		t.Fatal("could not extract openrc_teardown_service from the Linux installer")
	}

	// An empty PATH means command -v finds neither sudo nor rc-service.
	out, err := runShellScript(t, `
SUDO=
install_path="/usr/local/lib/urnetwork-provider"
openrc_initd_file="/etc/init.d/urnetwork"
openrc_user_home() { printf '/tmp\n'; }
pr_info() { :; }
pr_warn() { printf 'WARN: %s\n' "$*"; }
pr_err() { printf 'ERROR: %s\n' "$*"; }
`+fn+`
openrc_teardown_service
`)
	if err != nil {
		t.Fatalf("teardown failed: %v (%s)", err, out)
	}
	if !strings.Contains(out, "Removing the system service needs root") {
		t.Errorf("with no usable sudo the teardown must print the manual commands, got:\n%s", out)
	}
}
