package urnettools

import (
	"fmt"
	"strings"
)

// The FreeBSD auto-update crontab bookkeeping, kept untagged so the matching
// RULE is unit-testable on any platform. The functions that use it
// (writeBSDUpdateCron / removeBSDUpdateCron) only exist on FreeBSD.

// bsdCronMarker tags every line this tool writes so a schedule change can
// remove its own lines without touching anything else in the crontab.
const bsdCronMarker = "# urnet-tools auto-update"

// matchBSDCronLine reports whether a crontab line is the auto-update entry for
// this specific service.
//
// The marker must be matched at the END of the line. bsdCronMarker is a common
// prefix of every marker written, so a Contains match on "... auto-update
// urnetwork" also matches "... auto-update urnetwork_beta" — which would let a
// schedule change for the stable provider delete the beta provider's entry on a
// box running both. Trimming first makes a trailing space harmless.
func matchBSDCronLine(line, service string) bool {
	return strings.HasSuffix(strings.TrimSpace(line), bsdCronMarker+" "+service)
}

// bsdRootToolPath is the root-owned urnet-tools copy the FreeBSD installer
// stages. It must match install_path in scripts/Provider_Install_FreeBSD.sh:
// /usr/local/libexec/urnetwork-provider/bin/urnet-tools.
const bsdRootToolPath = "/usr/local/libexec/urnetwork-provider/bin/urnet-tools"

// bsdRootLogDir is where root's unattended update log lives. Root must not
// log into a home directory (root's own is fine but invisible to operators
// hunting failures, and the old fallback ~service-user is user-writable).
const bsdRootLogDir = "/var/log/urnetwork"

// bsdScheduledUpdate picks the tool path and log dir for a root/user cron
// entry. stagedToolOK reports whether the installer's root-owned tool copy
// exists (and is root-owned); runningTool/userLogDir are the non-root
// fallback, where the crontab runs as the same user who wrote it so a
// user-owned tool path is not an escalation.
//
// Root's crontab must only ever execute a path no non-root user can
// influence. Scheduling `sudo urnet-tools auto-update weekly` against the
// invoker's binary (os.Executable — typically the service user's tree) would
// hand that user root code execution at the next cron tick: they can rename
// the binary aside and plant their own. So root either uses the staged copy
// or gets an error; there is no silent fallback.
func bsdScheduledUpdate(euid int, stagedToolOK bool, runningTool, userLogDir string) (tool, logDir string, err error) {
	if euid != 0 {
		return runningTool, userLogDir, nil
	}
	if !stagedToolOK {
		return "", "", fmt.Errorf("refusing to install root's auto-update: no root-owned urnet-tools at %s — install it there first (the installer stages it), or run auto-update as your own user", bsdRootToolPath)
	}
	// The staged copy self-updates (update -f replaces the running
	// executable), so a root-run tick keeps it current.
	return bsdRootToolPath, bsdRootLogDir, nil
}

// bsdTreeOwnedByRoot reports whether a binary path lives under the
// root-owned FreeBSD layout. installBinary consults this before chowning a
// freshly swapped binary to the service user: chowning a file in the
// root-owned tree would hand the file itself to an unprivileged account that
// root later executes (update-time version verify, the next urnet-tools update).
// The service user only needs read+exec, which the 0755 above already delivers.
func bsdTreeOwnedByRoot(path string) bool {
	return strings.HasPrefix(path, "/usr/local/libexec/urnetwork-provider")
}
