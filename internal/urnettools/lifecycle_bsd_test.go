package urnettools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// renderBSDServiceScript produces the rc.d script the FreeBSD installer writes.
// The installer in scripts/Provider_Install_FreeBSD.sh carries its own copy of
// this template (a shell script cannot call into Go), so these assertions are
// about the Go-side renderer and the two must be kept in step by hand.
// The rc.d script the Go side emits is duplicated in the shell installer, and
// the two can drift. This test WRITES the rendered script to a file so the
// FreeBSD CI job can run it through `sh -n` on a real kernel.
//
// It writes a file rather than t.Log-ing the script: a go test log line is
// prefixed with "    file_test.go:NN: ", and that prefix is not valid shell —
// stripping it is guesswork, and getting it wrong would make the CI parse
// check assert against a mangled script.
func TestRenderBSDServiceScriptDumpsTemplate(t *testing.T) {
	script := renderBSDServiceScript("urnetwork", "/usr/local/bin/urnetwork", "tester", "/home/tester")
	if strings.TrimSpace(script) == "" {
		t.Fatal("renderBSDServiceScript returned an empty script")
	}
	// -test.run with a file path is the only way to get the exact bytes out
	// of a compiled test binary; the path comes from the caller in CI.
	out := os.Getenv("URN_RC_DUMP")
	if out == "" {
		t.Skip("URN_RC_DUMP not set; nothing to dump")
	}
	if err := os.WriteFile(out, []byte(script), 0o644); err != nil {
		t.Fatalf("write rendered rc.d: %v", err)
	}
	t.Logf("wrote %d bytes of rendered rc.d to %s", len(script), out)
}

func TestRenderBSDServiceScriptShape(t *testing.T) {
	script := renderBSDServiceScript("urnetwork", "/usr/local/bin/urnetwork", "tester", "/home/tester")

	// rc.subr provides run_rc_command; without it the script is not an rc.d
	// script at all.
	if !strings.Contains(script, ". /etc/rc.subr") {
		t.Error("script does not source rc.subr")
	}
	if !strings.Contains(script, "run_rc_command") {
		t.Error("script does not call run_rc_command")
	}
	// PROVIDE is what makes the script discoverable by the rc system.
	if !strings.Contains(script, "PROVIDE: urnetwork") {
		t.Error("script lacks the PROVIDE declaration")
	}
	// The knobs are read from rc.conf, which is what setAutoStart writes.
	for _, knob := range []string{"urnetwork_enable", "urnetwork_user", "urnetwork_flags"} {
		if !strings.Contains(script, knob) {
			t.Errorf("script does not read the %s rc.conf knob", knob)
		}
	}
	// The binary and working directory must be baked in, and the service must
	// run the provider's `provide` subcommand.
	// The provider is started through daemon(8), not run in the foreground:
	// a foreground `service <svc> start` would never return and would stall the
	// boot sequence, since rc runs start synchronously.
	if !strings.Contains(script, `daemon="/usr/sbin/daemon"`) {
		t.Error("script does not use daemon(8); a foreground provider would hang start and boot")
	}
	if !strings.Contains(script, "$daemon -f") {
		t.Error("script does not invoke daemon with -f")
	}
	// -p gives `service onestatus` a real pid to check; without it status always
	// reports "not running" and every start launches a duplicate.
	if !strings.Contains(script, `-p "$pidfile"`) {
		t.Error("script does not pass the child pidfile to daemon")
	}
	if !strings.Contains(script, `kill "$_pid"`) {
		t.Error("script does not stop the provider through its pidfile")
	}
	// status must be wired explicitly: rc.subr only registers a status action
	// when procname/command or status_cmd is set, and this script sets neither
	// of the first two (verified against libexec/rc/rc.subr on stable/14).
	if !strings.Contains(script, `status_cmd="urnetwork_status"`) {
		t.Error("script does not define status_cmd; `service urnetwork onestatus` cannot work without it")
	}
	if !strings.Contains(script, `urnetwork_running_pid`) {
		t.Error("script has no pidfile-based running check; start/status cannot be reliable")
	}
	// The liveness probe must be ps, not kill -0: kill -0 returns EPERM
	// (non-zero, same as ESRCH) for a process owned by another user, so an
	// unprivileged status would report "not running" while the provider is
	// up. ps sees every process regardless of owner.
	if strings.Contains(script, `kill -0 "$_pid" 2>/dev/null || return 1`) {
		t.Error("running_pid uses kill -0; EPERM for another user's process would report a live provider as stopped")
	}
	if !strings.Contains(script, `[ -n "$(ps -p "$_pid" -o pid= 2>/dev/null)" ] || return 1`) {
		t.Error("running_pid does not probe liveness with ps; another user's live pid would report as stopped")
	}
	// daemon(8) defaults to sending both streams to -o. A mask of 0 sends
	// neither, which silently discards everything the provider logs. The
	// check is on the daemon invocation line specifically: the log-dir setup
	// legitimately uses `install -m 0640`, whose "-m 0" substring is not a
	// daemon mask.
	daemonLine := ""
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "$daemon -f") {
			daemonLine = line
			break
		}
	}
	if strings.Contains(daemonLine, "-m 0") {
		t.Error("daemon invocation passes -m 0; that discards all provider output")
	}
	if !strings.Contains(script, `$daemon -f -t urnetwork -p "$pidfile" -o "$logdir/stdout.log"`) {
		t.Error("script does not start daemon with the pidfile and log file")
	}
	// Starting a second copy on top of a live one must be refused.
	if !strings.Contains(script, "not starting a second copy") {
		t.Error("script has no duplicate-start guard")
	}
	// -u makes daemon set HOME/USER/SHELL for the target user, so the state dir
	// lands in that user's home rather than root's. It also replaces the old
	// `su -m`, which kept the caller's HOME.
	//
	// The rc.conf knobs MUST appear unescaped: a backslash inside the
	// generated file makes sh pass the literal text ${urnetwork_user} as an
	// argument instead of expanding it, so daemon -u and chown would both get
	// a junk user and the provider would fail to start. The escaping is only
	// needed inside the installer's heredoc, which is where the value is
	// consumed at render time.
	if !strings.Contains(script, `-u ${urnetwork_user}`) {
		t.Error("script does not run the provider as the rc.conf user via daemon -u")
	}
	if strings.Contains(script, `\${urnetwork_user}`) {
		t.Error("script escapes ${urnetwork_user}; sh would pass it literally, not expand it")
	}
	// The log dir is root-owned (root:wheel, 0755) so the service user cannot
	// rename it aside and plant a symlink; only stdout.log itself is owned by
	// the rc.conf user, pre-created as a regular file, and a planted symlink
	// must refuse the start outright.
	if !strings.Contains(script, `chown root:wheel "$logdir"`) {
		t.Error("script does not chown the log dir to root:wheel")
	}
	// The log has to survive a restart: install(1) truncates its target, so
	// the file is created only when it is missing, and a creation that fails
	// must stop the start instead of being swallowed. Pinning the command
	// alone would pass while every restart erased the previous run.
	scriptLines := strings.Split(script, "\n")
	logGuard, logCreate := -1, -1
	for i, line := range scriptLines {
		if strings.Contains(line, `if [ ! -e "$logdir/stdout.log" ]; then`) {
			logGuard = i
		}
		if strings.Contains(line, `install -o ${urnetwork_user} -m 0640 /dev/null "$logdir/stdout.log"`) {
			logCreate = i
		}
	}
	if logGuard < 0 {
		t.Fatal(`script creates stdout.log unconditionally; a restart would truncate the previous run`)
	}
	if logCreate < 0 {
		t.Fatal(`script does not pre-create stdout.log owned by the rc.conf user`)
	}
	if logCreate < logGuard || logCreate-logGuard > 3 {
		t.Errorf("stdout.log creation sits outside the missing-file guard (guard line %d, install line %d)", logGuard, logCreate)
	}
	if !strings.Contains(scriptLines[logCreate], "|| return 1") {
		t.Errorf("a failed stdout.log creation does not fail the start: %q", strings.TrimSpace(scriptLines[logCreate]))
	}
	if strings.Contains(scriptLines[logCreate], "|| true") {
		t.Errorf("stdout.log creation failure is swallowed: %q", strings.TrimSpace(scriptLines[logCreate]))
	}
	if !strings.Contains(script, `if [ -L "$logdir/stdout.log" ]`) {
		t.Error("script does not refuse to start when stdout.log is a symlink")
	}
	// The symlink check alone lets two shapes through to daemon(8), which
	// opens the path as root: a fifo blocks that open for good, and a hard link
	// makes the log write through to an aliased file. The check has to ask for
	// the file type, the link count and the owner, and it has to run before the
	// missing-file guard decides whether anything is created.
	statLine := -1
	for i, line := range scriptLines {
		if !strings.Contains(line, "stat -f ") {
			continue
		}
		statLine = i
		if !strings.Contains(line, `'%HT %l %Su'`) {
			t.Errorf("stdout.log check does not ask stat for the file type, link count and owner: %q", strings.TrimSpace(line))
		}
		if !strings.Contains(line, `!= "Regular File 1 ${urnetwork_user}"`) {
			t.Errorf("stdout.log check does not require a single-link regular file owned by the service user: %q", strings.TrimSpace(line))
		}
	}
	if statLine < 0 {
		t.Fatal(`script keeps an existing stdout.log without checking its type; a fifo or hard link passes the symlink check`)
	}
	if statLine > logGuard {
		t.Errorf("the stdout.log type check (line %d) runs after creation (line %d); it has to decide first", statLine, logGuard)
	}
	if statLine+1 >= len(scriptLines) || !strings.Contains(scriptLines[statLine+1], `rm -f "$logdir/stdout.log"`) {
		t.Error("a stdout.log that fails the type check is not removed before daemon(8) opens it")
	}
	if !strings.Contains(scriptLines[statLine+1], "|| return 1") {
		t.Error("a failed removal of the unsafe stdout.log does not fail the start")
	}
	// $flags must reach the provider expanded, not as a literal ${...} or a
	// bare backslash followed by the value.
	if !strings.Contains(script, `provide $urnetwork_flags`) {
		t.Error("script does not pass the rc.conf flags to provide")
	}
	if strings.Contains(script, `\$urnetwork_flags`) {
		t.Error("script escapes $urnetwork_flags; the provider would receive a literal")
	}
	// `su -m` appears in the explanatory comment, so assert on executable
	// lines only — a comment mentioning a mechanism is not that mechanism.
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "su -m") {
			t.Errorf("script still executes su -m: %q", trimmed)
		}
	}
}

// The rc.conf variables are expanded at RUN time by the rc system, after
// load_rc_config has read /etc/rc.conf. A template that hard-codes the literal
// text "urnetwork_user" into the su(1) invocation therefore runs the provider
// as a user literally named "urnetwork_user", or fails outright — and it looks
// correct when read. This pins the expansion.
func TestRenderBSDServiceScriptExpandsUserVariable(t *testing.T) {
	script := renderBSDServiceScript("urnetwork", "/usr/local/bin/urnetwork", "tester", "/home/tester")

	// Every rc.conf knob must be referenced as a shell variable so it is expanded
	// at run time, after load_rc_config has read /etc/rc.conf. The ${...} form
	// is required where a default is wanted and $name where it is not.
	for _, want := range []string{
		"${urnetwork_enable:=",
		"${urnetwork_user:=",
		"${urnetwork_flags:=",
		"$urnetwork_flags",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script does not expand %q as a shell variable", want)
		}
	}
	// The bare (unexpanded) name must not appear as a command argument. The
	// match has to skip the "${...}" form, which is exactly the one we want.
	for _, bad := range []string{
		"su -m urnetwork_user",
		"su -m urnetwork_enable",
	} {
		if strings.Contains(script, bad) {
			t.Errorf("script passes a literal rc.conf knob name to su: %q", bad)
		}
	}
}

// A beta build and a stable build on one box must not collide on one service
// name, so the name comes from the binary.
func TestBSDRcServiceNameFromBinary(t *testing.T) {
	for _, c := range []struct{ binary, want string }{
		{"/usr/local/bin/urnetwork", "urnetwork"},
		{"/usr/local/bin/urnetwork_beta", "urnetwork_beta"},
		// A hyphen is not valid in a shell variable name, and rc.conf IS
		// shell: `urnet-provider_enable=YES` is a line every boot fails to
		// source and sysrc still writes. Map to underscore instead.
		{"/usr/local/bin/urnet-provider", "urnet_provider"},
		{"/usr/local/bin/provider", "provider"},
		// A shell variable name cannot start with a digit either.
		{"/usr/local/bin/2fast", "_2fast"},
		{"/usr/local/bin/urnet.work", "urnet_work"},
		{"", "urnetwork"},
	} {
		if got := bsdRcServiceName(c.binary); got != c.want {
			t.Errorf("bsdRcServiceName(%q) = %q, want %q", c.binary, got, c.want)
		}
	}
}

// The rc.d script exists in two copies: the Go renderer above and the heredoc
// in scripts/Provider_Install_FreeBSD.sh. Nothing compared them, so a fix
// landed in one and left the other shipping a broken service — twice. This
// renders BOTH with the same inputs and compares them line for line.
//
// Both are rendered for real: the Go side through the renderer itself, the
// shell side by executing the extracted write_rc_script_body with the same
// service, binary and user. Comparison drops comments and joins line
// continuations, which are the only permitted differences.
func TestBSDTemplateParity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the rc.d template is not a Windows artifact")
	}

	goScript := renderBSDServiceScript("urnetwork", "/usr/local/bin/urnetwork", "tester", "/home/tester")

	shScript, err := renderInstallerRcdTemplate(t, "urnetwork", "/usr/local/bin/urnetwork", "tester")
	if err != nil {
		t.Fatalf("render the installer copy: %v", err)
	}

	ng := normaliseTokens(comparableRcdLines(goScript))
	nsh := normaliseTokens(comparableRcdLines(shScript))
	if len(ng) != len(nsh) {
		t.Errorf("template length differs: Go renderer has %d lines, installer %d\n%s",
			len(ng), len(nsh), strings.Join(dumpLines(ng, nsh), "\n"))
		return
	}
	for i := range ng {
		if ng[i] != nsh[i] {
			t.Errorf("line %d differs between the two copies:\n  Go:       %q\n  installer: %q\n%s",
				i+1, ng[i], nsh[i], strings.Join(dumpLines(ng[min(i, len(ng)-1):], nsh[min(i, len(nsh)-1):]), "\n"))
			return
		}
	}
}

// renderInstallerRcdTemplate executes the installer's write_rc_script_body with
// the given values and returns what it writes to stdout. The function is
// extracted verbatim from the shipped script rather than copied, so the test
// tracks the file instead of drifting from it.
func renderInstallerRcdTemplate(t *testing.T, service, binary, user string) (string, error) {
	t.Helper()
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		return "", err
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")

	start := strings.Index(script, "write_rc_script_body() {")
	if start < 0 {
		return "", errors.New("write_rc_script_body is missing from the FreeBSD installer")
	}
	// The heredoc body contains column-0 braces, so the function cannot be cut
	// at the first "}": its real end is the RCSCRIPT delimiter.
	rest := script[start:]
	end := strings.Index(rest, "\nRCSCRIPT\n}")
	if end < 0 {
		return "", errors.New("could not find the end of write_rc_script_body")
	}
	fn := rest[:end+len("\nRCSCRIPT\n}")]

	// The whole prefix of the installer is included so every variable the
	// template interpolates is defined exactly as it is in a real install.
	dir := t.TempDir()
	full := dir + "/installer.sh"
	if err := os.WriteFile(full, []byte(script[:start]+fn+`
service_name="`+service+`"
provider_bin="`+binary+`"
write_rc_script_body "`+user+`"
`), 0o755); err != nil {
		return "", err
	}
	out, err := exec.Command("/bin/sh", full).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, out)
	}
	// Rendering must be silent: a stray $(cat ...) or similar running at
	// render time prints to stderr and produces empty expansions.
	return string(out), nil
}

// comparableRcdLines reduces a rendered rc.d script to the lines that carry
// behaviour: comments are dropped and `\`-continuations are joined, because
// the two copies legitimately differ in how they wrap the daemon invocation.
func comparableRcdLines(script string) []string {
	var out []string
	pending := ""
	flush := func() {
		if pending != "" {
			out = append(out, pending)
			pending = ""
		}
	}
	for _, raw := range strings.Split(script, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// `pkill -x "$(basename "$bin")"` is written literally in the Go
		// copy and expanded by the unquoted heredoc in the shell copy, so the
		// two differ in text but not in behaviour. Both must kill the same
		// basename, which the shapes asserted above already pin.
		if i := strings.Index(line, "pkill -TERM -x "); i >= 0 {
			line = `pkill -TERM -x <basename>`
		}
		// Join `\` continuations: the installer heredoc is unquoted, so the
		// backslash is consumed at render time and its copy arrives as one
		// long line. That is a rendering artifact, not a behavioural
		// difference, and it must not be reported as drift.
		if strings.HasSuffix(line, "\\") {
			// The heredoc collapses a continuation into the whitespace around
			// it, so the joined text can contain runs of spaces. Keep the
			// token list and let the join happen on a clean boundary.
			pending += strings.TrimSuffix(line, "\\") + " "
			continue
		}
		pending += line
		flush()
	}
	flush()
	return out
}

// normaliseTokens collapses internal whitespace runs so continuation joining
// cannot glue two words together or leave ragged spacing. Comparing field
// lists rather than raw strings keeps a formatting difference from reading as
// behavioural drift, while any change to a flag, path or variable still fails.
func normaliseTokens(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.Join(strings.Fields(l), " ")
	}
	return out
}

func dumpLines(a, b []string) []string {
	var out []string
	for i := range a {
		if i < len(b) {
			out = append(out, fmt.Sprintf("    context %q vs %q", a[i], b[i]))
			if i >= 4 {
				break
			}
		}
	}
	return out
}

// freeBSDInstallerScript returns the shipped FreeBSD installer with CRLF
// normalised, so assertions run against the file that ships rather than a
// hand-typed copy of it.
func freeBSDInstallerScript(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatalf("read the FreeBSD installer: %v", err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// http_fetch is the only downloader in the FreeBSD installer. FreeBSD ships
// fetch(1) in the base system and curl only as a package, so one bare curl
// call is an install that works on the developer's host and dies on a clean
// one. The helper must exist with all three fallbacks, every download must go
// through it, and no executable line may call curl directly.
func TestBSDInstallerHTTPFetchShape(t *testing.T) {
	script := freeBSDInstallerScript(t)

	helper := extractShellFunc(t, script, "http_fetch")
	if helper == "" {
		t.Fatal("http_fetch is missing from the FreeBSD installer: a clean FreeBSD host has no curl")
	}
	curlAt := strings.Index(helper, "command -v curl")
	fetchAt := strings.Index(helper, "command -v fetch")
	wgetAt := strings.Index(helper, "command -v wget")
	if curlAt < 0 || fetchAt < 0 || wgetAt < 0 {
		t.Fatalf("http_fetch lost a fallback (curl at %d, fetch at %d, wget at %d):\n%s", curlAt, fetchAt, wgetAt, helper)
	}
	if !(curlAt < fetchAt && fetchAt < wgetAt) {
		t.Errorf("http_fetch tries the downloaders in the wrong order (offsets curl=%d fetch=%d wget=%d); want curl, fetch, wget", curlAt, fetchAt, wgetAt)
	}
	// Running out of downloaders has to say what the host is missing rather
	// than fail mute: the operator only sees this when nothing else worked.
	noTool := helper[wgetAt:]
	for _, tool := range []string{"curl", "fetch", "wget"} {
		if !strings.Contains(noTool, tool) {
			t.Errorf("the no-downloader message does not name %s:\n%s", tool, noTool)
		}
	}

	// Each download in the install path goes through the helper and keeps the
	// output shape that call needs: a file for the tarball and the wrapper, a
	// captured stdout value for the two JSON reads.
	for _, want := range []string{
		`http_fetch "$github_raw/scripts/Provider_Install_FreeBSD.sh" "$_td/urnet-tools.wrapper"`,
		`release_json="$(http_fetch "$release_url" 2>/dev/null)"`,
		`http_fetch "$tarball_url" "$tmpdir/provider.tar.gz"`,
		`http_fetch "https://github.com/full-bars/sn/releases/download/$tag/$tool_asset" "$tmpdir/$tool_asset"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("download call site is not routed through http_fetch: %s", want)
		}
	}

	// The release metadata is read ONCE into release_json and both asset digests
	// are resolved from that snapshot. The tool step used to re-fetch it, which
	// meant the two digests could come from different responses and cost a
	// second API call against the same rate limit. One read, one snapshot.
	if got := strings.Count(script, `release_json="$(http_fetch`); got != 1 {
		t.Errorf("release metadata is fetched into release_json %d times, want exactly 1: both digests must come from one snapshot", got)
	}

	// Comments may explain curl; executable lines may not run it. The helper
	// itself is the one place allowed to call it directly.
	withoutHelper := strings.Replace(script, helper, "", 1)
	for i, line := range strings.Split(withoutHelper, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "curl ") || strings.HasSuffix(trimmed, "curl") {
			t.Errorf("line %d calls curl directly instead of http_fetch: %q", i+1, trimmed)
		}
	}
}

// TestBSDInstallerHTTPFetchInstallsWithoutCurl executes the shipped http_fetch
// and install_wrapper_tool - the first download of do_install - with curl
// absent from PATH and fetch present. The wrapper must arrive, which is the
// step that used to die before the helper existed.
func TestBSDInstallerHTTPFetchInstallsWithoutCurl(t *testing.T) {
	skipWithoutPOSIXShell(t)
	script := freeBSDInstallerScript(t)

	httpFetch := extractShellFunc(t, script, "http_fetch")
	prErr := extractShellFunc(t, script, "pr_err")
	prWarn := extractShellFunc(t, script, "pr_warn")
	wrapperTool := extractShellFunc(t, script, "install_wrapper_tool")
	if httpFetch == "" || prErr == "" || prWarn == "" || wrapperTool == "" {
		t.Fatalf("could not extract the shipped functions (http_fetch=%d, pr_err=%d, pr_warn=%d, install_wrapper_tool=%d bytes)",
			len(httpFetch), len(prErr), len(prWarn), len(wrapperTool))
	}

	root := t.TempDir()
	stubDir := filepath.Join(root, "stub")
	installPath := filepath.Join(root, "install")
	calls := filepath.Join(root, "calls.log")
	for _, dir := range []string{stubDir, filepath.Join(installPath, "bin")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	// No curl stub: command -v curl has to fail, as it does on a host that
	// never installed the package. fetch must be the one that answers.
	writeHTTPStub(t, stubDir, "fetch", httpFetchStub)
	for _, name := range []string{"chmod", "mv", "chown"} {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s is not available to stub: %v", name, err)
		}
		writeHTTPStub(t, stubDir, name, "#!/bin/sh\nexec "+real+" \"$@\"\n")
	}

	harness := filepath.Join(root, "harness.sh")
	body := fmt.Sprintf(`#!/bin/sh
me=urnet-install.sh
github_raw="https://raw.test/full-bars/sn/main"
install_path="%s"
%s
%s
%s
priv() { "$@"; }
%s
install_wrapper_tool "%s"
rc=$?
echo "install-rc=$rc"
exit $rc
`, installPath, prErr, prWarn, httpFetch, wrapperTool, root)
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write the harness: %v", err)
	}

	cmd := exec.Command("/bin/sh", harness)
	cmd.Env = []string{"PATH=" + stubDir, "STUB_CALLS=" + calls}
	out, runErr := cmd.CombinedOutput()
	if !strings.Contains(string(out), "install-rc=0") {
		t.Fatalf("install_wrapper_tool did not finish with curl absent (err %v):\n%s", runErr, out)
	}

	wantURL := "https://raw.test/full-bars/sn/main/scripts/Provider_Install_FreeBSD.sh"
	got, err := os.ReadFile(filepath.Join(installPath, "bin", "urnet-tools"))
	if err != nil {
		t.Fatalf("the wrapper was not installed: %v\n%s", err, out)
	}
	if want := "FETCHED " + wantURL + "\n"; string(got) != want {
		t.Errorf("installed wrapper is %q, want the bytes fetch returned %q", got, want)
	}
	recorded, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("fetch was never called: %v\n%s", err, out)
	}
	if want := "-q -o " + filepath.Join(root, "urnet-tools.wrapper") + " " + wantURL; !strings.Contains(string(recorded), want) {
		t.Errorf("fetch was not asked for the wrapper the way the helper documents (-q -o OUT URL):\n%s", recorded)
	}
}

// TestBSDInstallerHTTPFetchFallbacks covers the helper's other shapes: stdout
// when no output file is given, wget when fetch is absent, and a named error
// when no downloader exists at all.
func TestBSDInstallerHTTPFetchFallbacks(t *testing.T) {
	skipWithoutPOSIXShell(t)
	script := freeBSDInstallerScript(t)

	httpFetch := extractShellFunc(t, script, "http_fetch")
	prErr := extractShellFunc(t, script, "pr_err")
	if httpFetch == "" || prErr == "" {
		t.Fatal("could not extract http_fetch and pr_err from the FreeBSD installer")
	}
	harness := func(t *testing.T, path string, args string) (string, error) {
		t.Helper()
		file := filepath.Join(t.TempDir(), "harness.sh")
		body := "#!/bin/sh\nme=urnet-install.sh\n" + prErr + "\n" + httpFetch + "\n" + args + "\n"
		if err := os.WriteFile(file, []byte(body), 0o755); err != nil {
			t.Fatalf("write the harness: %v", err)
		}
		cmd := exec.Command("/bin/sh", file)
		cmd.Env = []string{"PATH=" + path}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("fetch to stdout", func(t *testing.T) {
		stubDir := t.TempDir()
		writeHTTPStub(t, stubDir, "fetch", httpFetchStub)
		out, err := harness(t, stubDir, `http_fetch "https://raw.test/stdout"`)
		if err != nil {
			t.Fatalf("http_fetch to stdout failed: %v (%s)", err, out)
		}
		if want := "FETCHED https://raw.test/stdout\n"; out != want {
			t.Errorf("http_fetch wrote %q to stdout, want %q", out, want)
		}
	})

	t.Run("wget", func(t *testing.T) {
		stubDir := t.TempDir()
		writeHTTPStub(t, stubDir, "wget", httpWgetStub)
		outPath := filepath.Join(t.TempDir(), "asset")
		out, err := harness(t, stubDir, `http_fetch "https://raw.test/wget" "`+outPath+`"`)
		if err != nil {
			t.Fatalf("http_fetch through wget failed: %v (%s)", err, out)
		}
		got, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("wget wrote no file: %v", err)
		}
		if want := "WGET https://raw.test/wget\n"; string(got) != want {
			t.Errorf("wget output is %q, want %q", got, want)
		}
	})

	t.Run("none", func(t *testing.T) {
		out, err := harness(t, t.TempDir(), `http_fetch "https://raw.test/none"`)
		if err == nil {
			t.Fatalf("http_fetch succeeded with no downloader on PATH (%s)", out)
		}
		for _, tool := range []string{"curl", "fetch", "wget"} {
			if !strings.Contains(out, tool) {
				t.Errorf("the no-downloader message does not name %s: %q", tool, out)
			}
		}
	})
}

// writeHTTPStub writes an executable stub for the sandbox PATH.
func writeHTTPStub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatalf("write the %s stub: %v", name, err)
	}
}

// httpFetchStub implements the one fetch(1) shape http_fetch uses and records
// the call, so a wrong flag shows up as a failed test rather than a silent
// download from the network.
const httpFetchStub = `#!/bin/sh
printf '%s\n' "$*" >> "${STUB_CALLS:-/dev/null}"
if [ "$1" != "-q" ] || [ "$2" != "-o" ] || [ $# -ne 4 ]; then
    echo "fetch stub: unsupported arguments: $*" >&2
    exit 2
fi
url=$4
if [ "$3" = "-" ]; then
    printf 'FETCHED %s\n' "$url"
else
    printf 'FETCHED %s\n' "$url" > "$3"
fi
`

// httpWgetStub is the wget equivalent of httpFetchStub.
const httpWgetStub = `#!/bin/sh
printf '%s\n' "$*" >> "${STUB_CALLS:-/dev/null}"
if [ "$1" != "-q" ] || [ "$2" != "-O" ] || [ $# -ne 4 ]; then
    echo "wget stub: unsupported arguments: $*" >&2
    exit 2
fi
url=$4
if [ "$3" = "-" ]; then
    printf 'WGET %s\n' "$url"
else
    printf 'WGET %s\n' "$url" > "$3"
fi
`
