package miner

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"sync"
)

// system_audit.go ports the host half of the 3.23-fix provider's startup
// system audit (provider/system_audit_linux.go: RunSystemAudit) onto the
// miner. The disk half already landed separately (system_audit_disk_linux.go,
// startup_disk_audit.go); what that split left behind is this: the host
// limits a node needs before it can hold thousands of sockets.
//
// Three checks, all read-only and all warnings:
//
//   - the process's soft RLIMIT_NOFILE, because every proxy connection costs
//     descriptors, and an exhausted table shows up as a node that quietly
//     stops accepting clients rather than as an error;
//   - nf_conntrack_max against what this host's RAM calls for, because an
//     undersized table drops new flows silently;
//   - nf_conntrack_tcp_timeout_established, because a multi-day idle timeout
//     keeps closed flows in that table long after their peer is gone.
//
// The provider's numbers are kept where they are gates: a soft limit below
// 65535 and an established timeout above four hours. Where the provider also
// printed a target of its own, this file prints the value `urnet-tools
// optimize` writes instead, because that command is what the hint tells the
// operator to run: recommending a number the fix will not set would leave the
// warning firing after the fix. That is why the conntrack target is scaled by
// RAM like optimize's (hostAuditConntrackMaxForRAM) and the timeout target is
// 3600s rather than the provider's 5400s.
//
// Deliberately not ported from the provider file:
//
//   - the disk benchmark, which is the slow half and already has a miner port
//     (system_audit_disk_linux.go, startup_disk_audit.go);
//   - the provider's "conntrack kernel module not found" warning: a host
//     without the module has nothing to measure, and this audit never turns
//     "cannot read" into a warning;
//   - SN key handling and provider auth: those are not host limits and live in
//     the registration path (provider_key_warning.go, provider_registration.go).
//
// There is no on-demand entry point because the provider has none: its audit
// runs once at startup and is gated only by URNETWORK_SKIP_AUDIT=1. Inventing
// a new CLI surface for it here would be a different feature, and the values
// it reads are already visible in the control socket's resource report and in
// /metrics (urnet_conntrack_used_ratio, the fd pressure gauge).

const (
	// hostAuditFdFloor is the soft RLIMIT_NOFILE below which the node is
	// treated as unable to hold its sockets. It is the provider's gate, not
	// its printed target: distro and container defaults sit between 1024 and
	// 524288 on hosts that run fine, so warning below the golden value would
	// fire on healthy deployments.
	hostAuditFdFloor = 65535
	// hostAuditFdTarget is the golden-fleet soft limit, printed as the
	// recommendation (provider GoldenUlimit). Note that `urnet-tools optimize`
	// does not raise it: it writes fs.file-max, which is a different knob, so
	// the file-descriptor hint names the per-process fix instead.
	hostAuditFdTarget = 1048576
	// hostAuditConntrackTimeoutLimit is the provider's suboptimal gate for
	// nf_conntrack_tcp_timeout_established: above four hours, closed flows
	// hold their table slot long after the peer is gone.
	hostAuditConntrackTimeoutLimit = 14400
	// hostAuditConntrackTimeoutTarget is what `urnet-tools optimize` writes
	// (internal/urnettools/legacy_cmds.go: timeoutEstablished = 3600).
	hostAuditConntrackTimeoutTarget = 3600
)

// hostAuditObservations is what the platform could read. A nil field means
// "not observable here" (no /proc entry, a kernel without conntrack, a
// non-Linux host) and contributes no finding: the audit reports values it
// saw, never values it guessed.
type hostAuditObservations struct {
	// FdLimit is the process's soft RLIMIT_NOFILE.
	FdLimit *uint64
	// ConntrackMax is nf_conntrack_max.
	ConntrackMax *int64
	// ConntrackMaxTarget is the nf_conntrack_max this host's RAM calls for,
	// derived from MemTotal the way `urnet-tools optimize` sizes it. Nil when
	// MemTotal could not be read, which skips the conntrack-max check rather
	// than warning against a made-up target.
	ConntrackMaxTarget *int64
	// ConntrackTimeoutEstablished is nf_conntrack_tcp_timeout_established.
	ConntrackTimeoutEstablished *int64
}

// hostAuditFinding is one failed check: the value the host has and the value
// it should have, kept apart from the formatting so a test can assert both
// numbers without matching a whole sentence.
type hostAuditFinding struct {
	Check    string
	Observed string
	Expected string
}

// line is the provider's line format (RunSystemAudit), so an operator's log
// grep works the same on both binaries.
func (f hostAuditFinding) line() string {
	return fmt.Sprintf("[audit] %s: %s (Suboptimal! Target: %s)\n", f.Check, f.Observed, f.Expected)
}

// auditHost grades observations. It is pure: the platform reader decides what
// was observable, this decides what is wrong, and a test can supply either.
func auditHost(obs hostAuditObservations) []hostAuditFinding {
	var findings []hostAuditFinding
	if obs.FdLimit != nil && *obs.FdLimit < hostAuditFdFloor {
		findings = append(findings, hostAuditFinding{
			Check:    "File Descriptors",
			Observed: strconv.FormatUint(*obs.FdLimit, 10),
			Expected: strconv.Itoa(hostAuditFdTarget) + "+",
		})
	}
	if obs.ConntrackMax != nil && obs.ConntrackMaxTarget != nil && *obs.ConntrackMax < *obs.ConntrackMaxTarget {
		findings = append(findings, hostAuditFinding{
			Check:    "Conntrack Max",
			Observed: strconv.FormatInt(*obs.ConntrackMax, 10),
			Expected: strconv.FormatInt(*obs.ConntrackMaxTarget, 10),
		})
	}
	if obs.ConntrackTimeoutEstablished != nil && *obs.ConntrackTimeoutEstablished > hostAuditConntrackTimeoutLimit {
		findings = append(findings, hostAuditFinding{
			Check:    "Conntrack Timeout",
			Observed: strconv.FormatInt(*obs.ConntrackTimeoutEstablished, 10) + "s",
			Expected: strconv.Itoa(hostAuditConntrackTimeoutTarget) + "s",
		})
	}
	return findings
}

// hostAuditHints turns findings into the advice lines the provider prints
// after its checks. They are per-resource because the fixes differ: only the
// conntrack values are what `urnet-tools optimize` writes, while the
// descriptor limit is per process and no optimize run can raise it. Naming
// the wrong fix is worse than naming none.
func hostAuditHints(findings []hostAuditFinding, inContainer bool) []string {
	if len(findings) == 0 {
		return nil
	}
	needFd := false
	needConntrack := false
	for _, finding := range findings {
		if finding.Check == "File Descriptors" {
			needFd = true
		} else {
			needConntrack = true
		}
	}

	var hints []string
	if needFd {
		hints = append(hints, fmt.Sprintf(
			"[audit] Hint: raise the process soft file descriptor limit to %d+: LimitNOFILE= in the systemd unit, --ulimit nofile=<soft>:<hard> for the container, or `ulimit -n %d` in the shell that starts the miner.\n",
			hostAuditFdTarget, hostAuditFdTarget))
	}
	if needConntrack {
		if inContainer {
			hints = append(hints,
				"[audit] Hint: Container is isolated from host network optimizations; set the nf_conntrack sysctls on the container (docker run --sysctl ...) or on the host that owns them.\n",
				"[audit] Hint: `urnet-tools optimize` on the host writes both conntrack values.\n")
		} else {
			hints = append(hints,
				"[audit] Hint: System is not optimized for high volume. Run 'urnet-tools optimize' to fix (it will prompt for sudo).\n")
		}
	}
	return hints
}

// hostAuditContainerMarkers are the files Docker and Podman leave at the
// filesystem root. A variable so a test can point the marker scan at a fixture
// directory instead of depending on how the test host is built.
var hostAuditContainerMarkers = []string{"/.dockerenv", "/run/.containerenv"}

// hostAuditInContainer only changes which hint is printed. Docker and Podman
// both leave a marker; the provider checks only /.dockerenv.
func hostAuditInContainer() bool {
	return hostAuditInContainerAt(hostAuditContainerMarkers)
}

// hostAuditInContainerAt is the marker scan itself, split out so the container
// branch is testable without a container: it reports true when any marker path
// exists.
func hostAuditInContainerAt(markers []string) bool {
	for _, marker := range markers {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

// reportHostAudit writes one line per failed check plus the hints, and
// returns the number of failed checks. A clean host writes nothing at all,
// which is what keeps the startup log silent on a tuned node.
func reportHostAudit(w io.Writer, obs hostAuditObservations) int {
	findings := auditHost(obs)
	if len(findings) == 0 {
		return 0
	}
	if w == nil {
		// fmt.Fprint(nil, ...) dereferences the nil interface and panics.
		// The audit's contract is that it is never fatal, so an unset sink
		// falls back to where proxyManagerLogWriter starts out pointing.
		w = os.Stderr
	}
	for _, finding := range findings {
		fmt.Fprint(w, finding.line())
	}
	for _, hint := range hostAuditHints(findings, hostAuditInContainer()) {
		fmt.Fprint(w, hint)
	}
	return len(findings)
}

// hostAuditGather is the platform reader. A variable so tests can drive the
// startup entry with observation values instead of the test host's real
// limits (which a test cannot set).
var hostAuditGather = gatherHostAuditObservations

// hostAuditOnce makes the startup audit exactly one check per process: a
// repeated hook, or a hot-restart path that runs it again, is a no-op. A
// skipped run does not consume it (see startStartupHostAudit).
var hostAuditOnce sync.Once

// hostAuditPanicLog is where a recovered audit panic is reported: os.Stderr in
// production, a captured buffer in the test that pins the recover.
var hostAuditPanicLog io.Writer = os.Stderr

// recoverHostAuditPanic is the deferred recover for the audit goroutine. An
// unrecovered panic in any goroutine ends the process, and this check is never
// allowed to be fatal, so it is recovered here. It is not swallowed, though:
// the panic value and a stack go to the log, because a panic here is a
// programming error and losing it would leave the audit silently dead.
func recoverHostAuditPanic() {
	recovered := recover()
	if recovered == nil {
		return
	}
	defer func() {
		// Reporting the panic must not become a second, unrecovered panic
		// (the report sink is an io.Writer like any other).
		_ = recover()
	}()
	w := hostAuditPanicLog
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "[audit] host audit panicked and was recovered (startup continues): %v\n%s", recovered, debug.Stack())
}

// startStartupHostAudit runs the host checks at most once per process and
// returns a channel that closes when they have been reported (nil when the
// audit is skipped or already ran). It is called from Run() on the
// long-running provide path, before any provider starts.
//
// The check runs in its own goroutine for the same reason the disk audit does
// (startup_disk_audit.go): startup must not wait for it. Here the work is only
// three /proc reads and one getrlimit, so the goroutine is not about cost — it
// is the structural guarantee that a slow or wedged read cannot hold up a
// provider launch. Nothing it does is fatal, and nothing it does writes: the
// goroutine recovers and logs a panic rather than letting it end the process.
//
// URNETWORK_SKIP_AUDIT=1 skips it, as it skips the disk half. The skip stays
// silent because the disk audit prints the shared "[audit] System audit
// skipped" line when it is wired in; a second identical line would only
// confuse the log.
//
// The skip is checked outside hostAuditOnce on purpose. A skip is not a run,
// so it must not consume the once: with the check inside it, a process whose
// first provide cycle ran with the variable set could never audit again, even
// after the variable was cleared.
func startStartupHostAudit() <-chan struct{} {
	if os.Getenv("URNETWORK_SKIP_AUDIT") == "1" {
		return nil
	}
	var done chan struct{}
	hostAuditOnce.Do(func() {
		// Snapshot the writer before the goroutine exists. proxyManagerLogWriter
		// is a package variable, and reading it from the goroutine while a
		// diagnostics path reassigns it would be a data race.
		writer := proxyManagerLogWriter
		done = make(chan struct{})
		go func() {
			// Registered before the recover, so close runs last and the
			// channel closes even when the audit panics. A caller waiting on
			// it must never be left hanging.
			defer close(done)
			defer recoverHostAuditPanic()
			reportHostAudit(writer, hostAuditGather())
		}()
	})
	return done
}
