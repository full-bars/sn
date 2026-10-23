package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// renderBSDServiceScript returns the rc.d script for a provider. It is a pure
// function of its inputs so the script shape can be asserted in a test without
// writing to /usr/local/etc.
//
// rc.conf carries the overrides (<service>_enable, _user, _flags) because that
// is the rc.subr convention: editing rc.conf is how an operator changes the
// service's settings.
func renderBSDServiceScript(service, binary, user, workingDir string) string {
	return fmt.Sprintf(`#!/bin/sh
#
# PROVIDE: %[1]s
# REQUIRE: LOGIN NETWORKING
# KEYWORD: shutdown
#
# rc.conf knobs:
#   %[1]s_enable="YES"   start at boot (set by urnet-tools auto-start)
#   %[1]s_user="%[2]s"   run as this user
#   %[1]s_flags=""       extra flags for the provider
#
# To run at boot:
#   sysrc %[1]s_enable=YES

. /etc/rc.subr

name="%[1]s"
rcvar=%[1]s_enable

load_rc_config $name

: ${%[1]s_enable:="NO"}
: ${%[1]s_user:="%[2]s"}
: ${%[1]s_flags:=""}

start_cmd="%[1]s_start"
stop_cmd="%[1]s_stop"
status_cmd="%[1]s_status"

# The provider is long-lived, so start it through daemon(8). Running it in the
# foreground would make "service %[1]s start" never return AND stall the boot
# sequence, because rc runs start synchronously.
#
# daemon(8) also settles two things a bare "su -m ... &" would not:
#   - -u sets HOME, USER and SHELL for the target user, so the provider's state
#     dir lands in that user's home rather than root's.
#   - -p writes the child pid through pidfile(3), giving stop and status a
#     precise handle on the child process.
#
# No -m flag: daemon(8) defaults to sending BOTH stdout and stderr to the -o
# file. A mask of 0 (the previous value) plumbed neither, so the log stayed
# empty and the provider ran with its output discarded.
#
# rc.subr's own status/start pid logic is not used here: it keys off
# procname/command, and this script sets neither, so rc_pid would always be
# empty (start would launch duplicates, status would always report stopped).
# The pidfile is checked directly instead.
daemon="/usr/sbin/daemon"
pidfile="/var/run/%[1]s.pid"
logdir="/var/log/%[1]s"

%[1]s_running_pid()
{
    # Print the live provider pid, or nothing when it is not running.
    # daemon(8) opens the pidfile with a hard-coded mode 0600 (pidfile_open
    # in daemon.c, not configurable through -p/-P) and start runs as root, so
    # an unprivileged one*status cannot read the file at all. When it is
    # unreadable, fall back to scanning the process table.
    if [ -f "$pidfile" ] && [ -r "$pidfile" ]; then
        _pid="$(cat "$pidfile" 2>/dev/null)"
        [ -n "$_pid" ] || return 1
        # ps, not kill -0: kill -0 returns EPERM (non-zero, same as ESRCH) for a
        # process owned by another user, so an unprivileged service status would
        # report "not running" while the provider is up. ps sees every process,
        # so a pid ps lists is alive regardless of who owns it.
        [ -n "$(ps -p "$_pid" -o pid= 2>/dev/null)" ] || return 1
        echo "$_pid"
        return 0
    fi
    # -U selects processes by username; -u would only select the output
    # format. The match is done in awk on the argv fields on purpose: a
    # "grep -F '<binary> provide'" in this same pipeline carries the search
    # string in its own argv, and "ps -U <service user>" lists that grep when
    # the caller is the service user. The grep then matches itself and a
    # stopped provider reports as running (and start skips a real start).
    _pid="$(ps -U "${%[1]s_user}" -o pid=,args= 2>/dev/null | awk -v b='%[4]s' '$2 == b && $3 == "provide" {print $1; exit}')"
    [ -n "$_pid" ] || return 1
    echo "$_pid"
}

%[1]s_start()
{
    if _pid="$(${name}_running_pid)"; then
        echo "%[1]s is already running as pid $_pid; not starting a second copy."
        return 0
    fi
    rm -f "$pidfile"
    mkdir -p "$logdir"
    chown root:wheel "$logdir" 2>/dev/null || true
    chmod 0755 "$logdir" 2>/dev/null || true
    # Pre-create stdout.log as a regular file owned by the service user.
    # Must be a regular file (not a symlink) to prevent privilege escalation
    # via planted symlinks. If it exists and is a symlink, refuse to start.
    if [ -L "$logdir/stdout.log" ]; then
        echo "Refusing to start: $logdir/stdout.log is a symlink (privilege escalation risk)."
        return 1
    fi
    install -o ${%[1]s_user} -m 0640 /dev/null "$logdir/stdout.log" 2>/dev/null || true
    echo "Starting %[1]s."
    $daemon -f -t %[1]s -p "$pidfile" -o "$logdir/stdout.log" \
        -u ${%[1]s_user} \
        "%[4]s" provide $%[1]s_flags
}

%[1]s_stop()
{
    echo "Stopping %[1]s."
    if ! _pid="$(${name}_running_pid)"; then
        echo "%[1]s is not running."
        rm -f "$pidfile"
        return 0
    fi
    kill "$_pid" 2>/dev/null || true
    _n=0
    while [ "$_n" -lt 30 ]; do
        kill -0 "$_pid" 2>/dev/null || break
        sleep 1
        _n=$((_n + 1))
    done
    if kill -0 "$_pid" 2>/dev/null; then
        echo "%[1]s did not exit within 30s; sending SIGKILL."
        kill -9 "$_pid" 2>/dev/null || true
    fi
    rm -f "$pidfile"
}

%[1]s_status()
{
    if _pid="$(${name}_running_pid)"; then
        echo "%[1]s is running as pid $_pid."
        return 0
    fi
    echo "%[1]s is not running."
    return 1
}

run_rc_command "$1"
`, service, user, workingDir, binary)
}

// bsdRcScriptDir is where FreeBSD keeps rc.d service scripts.
const bsdRcScriptDir = "/usr/local/etc/rc.d"

// bsdRcScriptWriteFn writes the installed rc.d script. A var so tests point
// the destination at a temp file instead of /usr/local/etc/rc.d.
var bsdRcScriptWriteFn = writeFileAtomic

// bsdServiceRunFn runs a service-control or sysrc command. Package var so
// tests can intercept the invocation without a live rc.d.
var bsdServiceRunFn = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// bsdServiceInstall is one rc.d service installation: the script to write and
// the account to run the provider as.
type bsdServiceInstall struct {
	Service string
	Binary  string
	User    string
	Home    string
	Script  string
}

// bsdPrivilegedArgv prefixes a command with sudo when the caller is not root.
// Writing the rc.d script, sysrc and `service start` all need root, and a
// non-root invocation that failed silently would report a service that does
// not exist.
func bsdPrivilegedArgv(euid int, name string, args ...string) (string, []string, error) {
	if euid == 0 {
		return name, args, nil
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return "", nil, fmt.Errorf("%s %s needs root and sudo is not installed: re-run as root", name, strings.Join(args, " "))
	}
	return "sudo", append([]string{name}, args...), nil
}

// bsdInstallService writes the rc.d script, flips the rc.conf knobs and starts
// the service. It mirrors do_install_service in
// scripts/Provider_Install_FreeBSD.sh, including the refusal to install a
// service that runs as root: the provider is an internet-facing relay, and
// sysrc would happily write root and every boot would then run it with full
// privileges.
//
// euid is passed rather than read so the decision is testable on any platform.
func bsdInstallService(in bsdServiceInstall, euid int, dryRun bool) error {
	if in.User == "" || in.User == "root" {
		return fmt.Errorf("refusing to install %s: it would run as %q, and the internet-facing relay must not run as root (pass --user <account>)", in.Service, in.User)
	}
	if in.Service == "" || in.Binary == "" || in.Script == "" {
		return fmt.Errorf("install-service: incomplete rc.d target (service=%q binary=%q script=%q)", in.Service, in.Binary, in.Script)
	}
	// sysrc edits /etc/rc.conf, and `service onestart` drives rc.subr; both are
	// root actions. onestart (not start) bypasses the _enable gate the way the
	// rest of this package does, so a service enabled by this command starts
	// even on a host whose rc.conf was not re-read.
	steps := [][]string{
		{"sysrc", in.Service + "_enable=YES"},
		{"sysrc", in.Service + "_user=" + in.User},
		{"service", in.Service, "onestart"},
	}
	if dryRun {
		fmt.Printf("[dry-run] would write %s (user %s, binary %s), then run:\n", in.Script, in.User, in.Binary)
		for _, s := range steps {
			fmt.Printf("[dry-run]   %s\n", strings.Join(s, " "))
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(in.Script), 0o755); err != nil {
		return fmt.Errorf("create rc.d directory %s: %w", filepath.Dir(in.Script), err)
	}
	if err := bsdRcScriptWriteFn(in.Script, []byte(renderBSDServiceScript(in.Service, in.Binary, in.User, in.Home)), 0o755); err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("write rc.d script %s: %w (re-run as root: sudo urnet-tools install-service)", in.Script, err)
		}
		return fmt.Errorf("write rc.d script %s: %w", in.Script, err)
	}
	for _, s := range steps {
		name, args, err := bsdPrivilegedArgv(euid, s[0], s[1:]...)
		if err != nil {
			return err
		}
		out, err := bsdServiceRunFn(name, args...)
		if err != nil {
			return fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	fmt.Printf("Installed rc.d service %s (user %s) and started it.\n", in.Service, in.User)
	return nil
}

// bsdRcScriptServesBinary reports whether an installed rc.d script runs this
// provider's binary. It is the target rule for uninstall: removing the rc.d
// service is correct only when the provider being uninstalled is the one the
// service actually launches. A bare provider on a box that also runs the
// service must not take the service down.
//
// The binary is read from the script, not assumed from the service name:
// install-service accepts --binary, and the shell installer has its own copy of
// the template, so the name alone proves nothing. Two shapes count: the
// generated start command ("<binary>" provide) and a command= assignment an
// operator added by hand. A path that merely appears in a comment does not.
func bsdRcScriptServesBinary(scriptPath, binary string) bool {
	if scriptPath == "" || binary == "" {
		return false
	}
	b, err := os.ReadFile(scriptPath)
	if err != nil {
		return false
	}
	want := filepath.Clean(binary)
	quoted := `"` + want + `"`
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "command=") {
			got := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "command=")), `"'`)
			if got != "" && filepath.Clean(got) == want {
				return true
			}
			continue
		}
		if strings.Contains(line, quoted+" provide") {
			return true
		}
	}
	return false
}

// bsdRemoveService stops and de-registers an rc.d service and removes its
// script. Every step is best-effort with a warning: uninstall must not abort
// halfway, and a missing service is not an error.
//
// Leaving the script and the rc.conf flag behind is what makes an uninstall
// dangerous: the binary is deleted, rc.conf still says enable=YES, and the next
// boot starts a path that no longer exists.
func bsdRemoveService(service string, euid int, scriptPath string) {
	steps := [][]string{
		{"service", service, "onestop"},
		{"sysrc", "-x", service + "_enable", service + "_user"},
	}
	for _, s := range steps {
		name, args, err := bsdPrivilegedArgv(euid, s[0], s[1:]...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "uninstall: warning: %v\n", err)
			continue
		}
		if out, err := bsdServiceRunFn(name, args...); err != nil {
			fmt.Fprintf(os.Stderr, "uninstall: warning: %s %s: %v (%s)\n",
				name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	if err := os.Remove(scriptPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "uninstall: warning: could not remove %s: %v (run: sudo rm -f %s)\n", scriptPath, err, scriptPath)
	}
}

// bsdRcScriptPath is the on-disk path of the rc.d script for a service name.
func bsdRcScriptPath(service string) string {
	return bsdRcScriptDir + "/" + service
}

// bsdRcServiceName is the rc.d service name for a provider binary. Derived from
// the binary so a beta build and a stable build on one box get different
// services instead of fighting over a single name.
func bsdRcServiceName(binary string) string {
	base := providerBinaryBasename(binary)
	if base == "" {
		return "urnetwork"
	}
	// rc.conf variables are SHELL variables: a hyphen, dot or space in the
	// name would make `<name>_enable` an invalid assignment. sysrc still
	// writes such a line and every boot then fails to source it, which is the
	// same silent-no-op this package already fixed for sysctl persistence. So
	// map anything outside [a-z0-9_] to _, the same way the kernel's rc
	// scripts sanitize service names.
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	name := b.String()
	if name == "" {
		return "urnetwork"
	}
	// A shell variable name cannot start with a digit.
	if name[0] >= '0' && name[0] <= '9' {
		name = "_" + name
	}
	return name
}

// providerBinaryBasename is the lowercase basename of a provider binary, used
// to name the rc.d service after the binary it runs.
func providerBinaryBasename(binary string) string {
	base := strings.ToLower(filepath.Base(binary))
	// filepath.Base("") is ".", which would name the service "." — refuse it
	// rather than produce a path-shaped service name.
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

// bsdRcConfUser reads the rc.conf user for the named rc.d service via
// sysrc(8). It returns the configured user, or "" when unset or unreadable.
// This is the authoritative source for which account a stopped service
// would run as: the passwd database is consulted only by the running
// process path, and a stopped install has no process to read credentials
// from, so the rc.conf knob is the only honest answer.
func bsdRcConfUser(service string) string {
	out, err := exec.Command("sysrc", bsdRcConfUserArgs(service)...).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// bsdRcConfUserArgs is the sysrc query behind bsdRcConfUser, split out so the
// service scope is asserted by a test on any platform.
//
// -s makes sysrc read the service's own rc.conf.d overrides, i.e.
// /etc/rc.conf.d/<service> and /usr/local/etc/rc.conf.d/<service>, the same
// files load_rc_config consults. Without it a query reads only
// /etc/rc.conf (and .local) and an operator's service-specific _user override
// is invisible, so stopped-provider discovery lands on the wrong user and
// state directory.
func bsdRcConfUserArgs(service string) []string {
	return []string{"-s", service, "-n", service + "_user"}
}
