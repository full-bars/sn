package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// bsdSysctlWrites returns the FreeBSD tuning keys and their target values.
//
// These are the FreeBSD namesakes of the Linux set, and the two are not
// interchangeable: net.core.rmem_max does not exist here (it is
// net.inet.tcp.recvspace), kern.maxfiles has no fs.file-max analogue, and there
// is no nf_conntrack at all — FreeBSD's firewall is pf, whose table limit is
// tuned per-rule rather than with a global sysctl. So a conntrack-sized write
// would be a silent no-op at best.
//
// Every key here is asserted to exist on a real kernel by the FreeBSD CI job,
// which extracts this list rather than repeating it. An earlier draft also
// listed "net.inet.tcp.sendspace.max", which does not exist — `optimize` aborts
// on the first unreadable key and rolls the whole set back, so a wrong name here
// means the command fails on every box. The TCP autotune ceilings
// (autosndbufmax / autorcvbufmax) would be the way to raise the maximum
// autotuned buffer; they are deliberately NOT set until their names are
// confirmed against a real kernel, because guessing costs a CI cycle and a red
// job, and an unverified key in this list is worse than no key.
func bsdSysctlWrites() [][]string {
	return [][]string{
		// Socket buffer ceiling FIRST, and it is load-bearing: tcp_attach
		// reserves the default send/receive space through soreserve(), whose
		// sbreserve() refuses any value above sb_max_adj (about
		// kern.ipc.maxsockbuf scaled by MCLBYTES/(MSIZE+MCLBYTES)). Setting the
		// TCP defaults to 4 MiB while maxsockbuf stayed at its ~2 MiB default
		// makes every new TCP socket fail with ENOBUFS — and because these are
		// persisted, that state would survive the reboot. Order matters: the
		// ceiling is raised before the values that depend on it.
		{"kern.ipc.maxsockbuf", "16777216"},
		// Socket buffers for the UDP/WebRTC and TCP transfer paths.
		{"net.inet.tcp.recvspace", "4194304"},
		{"net.inet.tcp.sendspace", "4194304"},
		// Descriptor budget: one open socket per proxied connection is the
		// provider's load-bearing resource.
		{"kern.maxfiles", "200000"},
		{"kern.maxfilesperproc", "100000"},
	}
}

// effectiveBSDValue returns the value to apply for one tuning key.
//
// Every key here RAISES a limit, so a host whose current value is already
// higher must keep it: writing the target unconditionally would LOWER a
// larger site default (a 500k kern.maxfiles becoming 200k) both live and at
// boot, which is the opposite of tuning. Non-numeric readings fall back to
// the target so a caller that could not parse still gets a sane value.
func effectiveBSDValue(current, target string) string {
	cur, cerr := strconv.ParseInt(strings.TrimSpace(current), 10, 64)
	tgt, terr := strconv.ParseInt(strings.TrimSpace(target), 10, 64)
	if cerr != nil || terr != nil {
		return target
	}
	if cur > tgt {
		return strings.TrimSpace(current)
	}
	return target
}

// effectiveBSDMaxsockbuf returns the kern.ipc.maxsockbuf value to apply,
// covering the TCP spaces the box will actually run with.
//
// The per-key raise is not enough for this key: sb_max_adj is only about
// MCLBYTES/(MSIZE+MCLBYTES) ≈ 0.889 of maxsockbuf, and a host whose
// recvspace/sendspace already sit above the configured targets (a tuned
// site default of 32 MiB, for example) keeps those spaces under
// effectiveBSDValue — while maxsockbuf raised only to its own target would
// leave sb_max_adj BELOW the spaces, so tcp_attach fails every new socket
// with ENOBUFS. The spaces are also persisted, so the breakage would
// survive a reboot. The floor is ceil(1.15 × max(effective spaces)): 1/0.889
// ≈ 1.125, and the extra margin covers sb_max_adj drift. A non-numeric
// reading anywhere falls back to the plain per-key raise.
func effectiveBSDMaxsockbuf(currentSockbuf, currentRecv, currentSend string) string {
	var targetSockbuf, targetRecv, targetSend string
	for _, w := range bsdSysctlWrites() {
		switch w[0] {
		case "kern.ipc.maxsockbuf":
			targetSockbuf = w[1]
		case "net.inet.tcp.recvspace":
			targetRecv = w[1]
		case "net.inet.tcp.sendspace":
			targetSend = w[1]
		}
	}
	base := effectiveBSDValue(currentSockbuf, targetSockbuf)
	effRecv, rerr := strconv.ParseInt(strings.TrimSpace(effectiveBSDValue(currentRecv, targetRecv)), 10, 64)
	effSend, serr := strconv.ParseInt(strings.TrimSpace(effectiveBSDValue(currentSend, targetSend)), 10, 64)
	baseInt, berr := strconv.ParseInt(strings.TrimSpace(base), 10, 64)
	if rerr != nil || serr != nil || berr != nil {
		return base
	}
	need := effRecv
	if effSend > need {
		need = effSend
	}
	floor := (115*need + 99) / 100 // ceil(1.15 × need)
	if baseInt > floor {
		return base
	}
	return strconv.FormatInt(floor, 10)
}

// WHY /etc/sysctl.conf AND NOT sysrc(8): FreeBSD persists runtime sysctl
// tunables in /etc/sysctl.conf, which /etc/rc.d/sysctl applies at boot in
// multi-user mode (sysctl.conf(5)). sysrc(8) is NOT an alternative — it edits
// rc.conf(5), whose variables are SHELL variable names, and a dotted sysctl MIB
// is not a valid shell assignment. Writing `net.inet.tcp.recvspace="..."` into
// rc.conf produces a line no boot script can consume, so the live setting would
// revert at the next reboot. An earlier version persisted through sysrc on
// exactly that reasoning and persisted nothing while reporting success.
//
// kern.maxfiles and kern.maxfilesperproc are also settable at runtime and so
// belong in the same file; /boot/loader.conf is only needed when a value must
// be in place before any rc script runs.

// bsdSysctlConfPath is the file /etc/rc.d/sysctl reads at boot.
const bsdSysctlConfPath = "/etc/sysctl.conf"

// Markers delimiting the block this tool owns. Everything between them is
// rewritten on each run; lines outside it are never touched.
const (
	bsdConfBeginMarker = "# --- urnet-tools optimize (managed block) ---"
	bsdConfEndMarker   = "# --- end urnet-tools optimize ---"
)

// renderBSDConfLine returns the sysctl.conf line for one key. sysctl.conf uses
// the sysctl(8) syntax `mib=value` and treats '#' as a comment introducer, so a
// key carrying one could be silently truncated to a comment.
func renderBSDConfLine(key, value string) string {
	return key + "=" + value
}

// upsertBSDConfLines returns conf with every managed key set to its target
// value. A key already present is REPLACED in place, preserving the file's
// other content and comment order; a key absent is appended under a single
// marked block.
//
// Pure, so the file rewriting is testable without root. A value is rejected
// rather than written if it is not a plain decimal number, because sysctl.conf
// is sourced by the boot scripts and anything else is at best a syntax error.
func upsertBSDConfLines(conf string, writes [][]string) (string, error) {
	lines := strings.Split(conf, "\n")

	for _, w := range writes {
		key, value := w[0], w[1]
		if err := validateBSDConfValue(key, value); err != nil {
			return "", err
		}
		want := renderBSDConfLine(key, value)

		// Replace every existing assignment to this key, keeping the first. The loop
		// used to stop at the first match, so a /etc/sysctl.conf that already sets
		// the key twice kept its later line untouched — and /etc/rc.d/sysctl applies
		// lines in file order, so the stale value won at boot while the command
		// still printed "persisted". Rewrite the first occurrence and drop the rest.
		replaced := false
		var dropIdx []int
		for i, l := range lines {
			trimmed := strings.TrimSpace(l)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if k, _, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(k) == key {
				if replaced {
					dropIdx = append(dropIdx, i) // a later duplicate: remove it so it cannot win at boot
					continue
				}
				lines[i] = want
				replaced = true
			}
		}
		if replaced {
			lines = dropLinesAt(lines, dropIdx)
			continue
		}

		// Otherwise append into the managed block, creating it on first use.
		if begin := indexOfLine(lines, bsdConfBeginMarker); begin >= 0 {
			end := indexOfLine(lines, bsdConfEndMarker)
			if end < begin {
				return "", fmt.Errorf("sysctl.conf has a begin marker with no end marker; refusing to edit")
			}
			block := make([]string, 0, end-begin+1)
			block = append(block, lines[begin+1:end]...)
			block = append(block, want)
			rest := append([]string{}, lines[end:]...)
			lines = append(append(append([]string{}, lines[:begin+1]...), block...), rest...)
			continue
		}

		// No managed block yet: create one, keeping a trailing newline tidy.
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines[n-1] = bsdConfBeginMarker
			lines = append(lines, want, bsdConfEndMarker, "")
		} else {
			lines = append(lines, "", bsdConfBeginMarker, want, bsdConfEndMarker)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// dropLinesAt returns lines with the given indexes removed, preserving order.
// Used to remove duplicate key assignments that were neutralised, without
// touching unrelated blank lines the file already had — dropping every empty
// line instead would strip a trailing newline the renderer relies on and break
// idempotence.
func dropLinesAt(lines []string, drop []int) []string {
	if len(drop) == 0 {
		return lines
	}
	gone := make(map[int]bool, len(drop))
	for _, i := range drop {
		gone[i] = true
	}
	out := make([]string, 0, len(lines)-len(drop))
	for i, l := range lines {
		if gone[i] {
			continue
		}
		out = append(out, l)
	}
	return out
}

// indexOfLine returns the index of the first line equal to want, or -1.
func indexOfLine(lines []string, want string) int {
	for i, l := range lines {
		if strings.TrimSpace(l) == want {
			return i
		}
	}
	return -1
}

// validateBSDConfValue rejects a value that is not a plain decimal number.
// sysctl.conf is consumed by shell at boot; a value carrying a quote, a space,
// a newline or a '#' would either be a syntax error or truncate the line into a
// comment, and either way the boot value would not be the one asked for.
func validateBSDConfValue(key, value string) error {
	if value == "" {
		return fmt.Errorf("refusing to persist an empty value for %s", key)
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return fmt.Errorf("refusing to persist non-numeric value %q for %s", value, key)
		}
	}
	return nil
}

// optimizeFreeBSD applies the FreeBSD equivalents of the golden-fleet tuning.
//
// The shape matches optimizeLinux deliberately: snapshot the prior live values,
// apply, roll back on any failure, then persist. Persisting writes
// /etc/sysctl.conf through a temp file and a rename, so an interrupted run cannot
// leave a half-written boot file.
func optimizeFreeBSD() error {
	if os.Geteuid() != 0 {
		self, _ := os.Executable()
		if self == "" {
			self = "urnet-tools"
		}
		return fmt.Errorf("optimize: sysctl requires root (running as uid %d); run: sudo %s optimize",
			os.Geteuid(), self)
	}

	writes := bsdSysctlWrites()

	// Snapshot first: a key we cannot read is a key we cannot roll back, so
	// abort before changing anything rather than half-way through.
	prior := make(map[string]string, len(writes))
	for _, w := range writes {
		out, err := exec.Command("sysctl", "-n", w[0]).Output()
		if err != nil {
			return fmt.Errorf("optimize: cannot read current %s (%v); aborting", w[0], err)
		}
		prior[w[0]] = strings.TrimSpace(string(out))
	}

	// Apply max(current, target) per key so an already-higher site value is
	// never lowered. maxsockbuf additionally floors at ceil(1.15× the
	// effective TCP spaces, see effectiveBSDMaxsockbuf) — the TCP spaces are
	// computed here first so that floor sees them; the slice still keeps
	// bsdSysctlWrites order (maxsockbuf before the spaces it must cover).
	eff := make(map[string]string, len(writes))
	for _, w := range writes {
		eff[w[0]] = effectiveBSDValue(prior[w[0]], w[1])
	}
	eff["kern.ipc.maxsockbuf"] = effectiveBSDMaxsockbuf(
		prior["kern.ipc.maxsockbuf"],
		prior["net.inet.tcp.recvspace"],
		prior["net.inet.tcp.sendspace"],
	)
	effective := make([][]string, 0, len(writes))
	for _, w := range writes {
		effective = append(effective, []string{w[0], eff[w[0]]})
	}

	// Snapshot the boot file too, so a persist failure restores it exactly. The
	// bytes are the source of truth, not a re-render: re-rendering would lose
	// entries this tool does not manage.
	confBefore, confExisted, err := readBSDConf()
	if err != nil {
		return fmt.Errorf("optimize: cannot read %s (%v); aborting", bsdSysctlConfPath, err)
	}

	applied := 0
	for _, w := range effective {
		if out, err := exec.Command("sysctl", "-w", w[0]+"="+w[1]).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "optimize: warning: sysctl -w %s failed: %v (%s); rolling back\n",
				w[0], err, strings.TrimSpace(string(out)))
			rollbackSysctls(effective[:applied], prior)
			return fmt.Errorf("optimize: live apply of %s failed: %w", w[0], err)
		}
		applied++
	}

	newConf, err := upsertBSDConfLines(confBefore, effective)
	if err != nil {
		// The file is untouched at this point, so only the live values need
		// reverting — but they must be, or the box runs tuned until reboot.
		fmt.Fprintf(os.Stderr, "optimize: warning: rendering %s failed: %v; rolling back\n",
			bsdSysctlConfPath, err)
		rollbackSysctls(effective, prior)
		return fmt.Errorf("optimize: persist %s failed (%v); rolled back live settings",
			bsdSysctlConfPath, err)
	}
	if err := writeBSDConf(newConf, confExisted); err != nil {
		fmt.Fprintf(os.Stderr, "optimize: warning: writing %s failed: %v; rolling back\n",
			bsdSysctlConfPath, err)
		rollbackBSDConf(confBefore, confExisted)
		rollbackSysctls(effective, prior)
		return fmt.Errorf("optimize: persist %s failed (%v); rolled back live and boot settings",
			bsdSysctlConfPath, err)
	}

	fmt.Println("optimize: done (live + " + bsdSysctlConfPath + " persisted)")
	return nil
}

// readBSDConf returns the current contents of the boot file and whether it
// existed. A missing file is not an error: it will be created.
func readBSDConf() (content string, existed bool, err error) {
	data, err := os.ReadFile(bsdSysctlConfPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(data), true, nil
}

// writeBSDConf replaces the boot file atomically, preserving its existing mode
// when there is one. It goes through writeFileAtomic: the file is fsync'd
// before the rename and the parent directory after it, so a power loss cannot
// commit the rename while leaving the contents unflushed (a truncated or
// empty /etc/sysctl.conf at boot, silently dropping every managed tunable).
func writeBSDConf(content string, existed bool) error {
	mode := os.FileMode(0o644)
	if existed {
		if fi, err := os.Stat(bsdSysctlConfPath); err == nil {
			mode = fi.Mode().Perm()
		}
	}
	return writeFileAtomic(bsdSysctlConfPath, []byte(content), mode)
}

// rollbackBSDConf restores the exact prior bytes of the boot file, or removes it
// if it did not exist before. Leaving behind a file this tool created and then
// failed to write would change the box's boot behaviour even though the command
// reported a rollback.
func rollbackBSDConf(content string, existed bool) {
	if !existed {
		if err := os.Remove(bsdSysctlConfPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "optimize: warning: removing %s failed: %v\n",
				bsdSysctlConfPath, err)
		}
		return
	}
	if err := writeBSDConf(content, true); err != nil {
		fmt.Fprintf(os.Stderr, "optimize: warning: restoring %s failed: %v\n",
			bsdSysctlConfPath, err)
	}
}
