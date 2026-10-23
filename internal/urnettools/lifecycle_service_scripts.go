package urnettools

import "fmt"

// Service-definition defaults for the two init systems whose service scripts
// the tool writes: OpenRC on Linux and rc.d on FreeBSD. The paths must match
// the installers (scripts/Provider_Install_Linux.sh installs under
// /usr/local/lib/urnetwork-provider, scripts/Provider_Install_FreeBSD.sh under
// /usr/local/libexec/urnetwork-provider). TestServiceInstallDefaultsMatch-
// Installers asserts the two stay in step.
const (
	openrcServiceBinaryPath = "/usr/local/lib/urnetwork-provider/bin/urnetwork"
	bsdServiceBinaryPath    = "/usr/local/libexec/urnetwork-provider/bin/urnetwork"
)

// defaultServiceBinary is the provider path install-service uses when the
// operator does not pass --binary. Empty means this platform has no service
// script for the tool to write.
func defaultServiceBinary(goos string) string {
	switch goos {
	case "linux":
		return openrcServiceBinaryPath
	case "freebsd":
		return bsdServiceBinaryPath
	}
	return ""
}

// renderOpenRCServiceScript returns the openrc-run script for a provider. It is
// a pure function of its inputs so the script shape can be asserted without
// writing to /etc/init.d, and so the FreeBSD build (which also compiles this
// file) does not need the Linux-only OpenRC machinery.
//
// It mirrors install_openrc_units in scripts/Provider_Install_Linux.sh. The
// installer cannot call into Go, so the template exists twice; the static
// directives are covered by TestOpenRCRenderMatchesInstallerDriftMarkers.
func renderOpenRCServiceScript(service, binary, user, home string) string {
	return fmt.Sprintf(`#!/sbin/openrc-run
# URnetwork Provider: OpenRC service (written by urnet-tools install-service)
#
# supervise-daemon keeps the provider running and respawns it on exit.
# command_user makes the provider run as '%[3]s' with HOME=%[4]s, so its
# state lives in %[4]s/.urnetwork.
#
# NOTE: zero-downtime updates (HotSwap) are unavailable under OpenRC:
# supervise-daemon has no sd_notify MainPID handoff, so urnet-tools update
# performs a stop/start service restart under this init system.

command="%[2]s"
command_args="provide"
command_user="%[3]s"
supervisor="supervise-daemon"
# supervise-daemon sets neither INVOCATION_ID nor NOTIFY_SOCKET, so without this
# marker the provider cannot tell it is supervised and refuses the thrash
# watchdog's supervised restart (exit 75).
export URNETWORK_INIT=openrc
# Stop schedule. Without retry= a provider that is hung or ignores SIGTERM
# survives rc-service stop (which still reports success), and the next start
# then runs a second provider with the same identity and state. TERM first with
# room for the 30s connection drain, then KILL.
retry="TERM/40/KILL/5"
# Crash handling. respawn_max=0 means UNLIMITED, which turns a binary that
# cannot start into a restart loop that floods /var/log forever. Bound it the
# way the systemd unit is bounded: respawn promptly, back off as failures
# repeat, then give up so the operator sees a stopped service instead of a
# spinning one.
respawn_delay=5
respawn_max=10
respawn_period=3600
# supervise-daemon opens these files AFTER dropping to command_user, so they
# must be owned by that user or the child cannot start at all. That stays safe
# because the safety property is the DIRECTORY: /var/log is root-owned, so the
# service user cannot create or replace a name in it.
# The names follow the service, so a beta build and a stable build on one box
# never write the same log.
output_log="/var/log/%[1]s.log"
error_log="/var/log/%[1]s.err"

# supervise-daemon --stop busy-loops FOREVER when the pidfile is empty (OpenRC
# parses no pid as 0, kill(0, 0) probes the caller's own process group, which is
# always alive). stop on an already-stopped, never-started, or crashed-clean
# service must be a no-op, not a hang.
stop() {
    if [ ! -s /var/run/supervise-%[1]s.pid ]; then
        ebegin "Stopping %[1]s (already stopped)"
        eend 0
        return 0
    fi
    default_stop
}

depend() {
    use net
    after firewall
}
`, service, binary, user, home)
}
