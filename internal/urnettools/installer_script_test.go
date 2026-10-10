package urnettools

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// The do_install body was once pasted twice into Provider_Install_Linux.sh, so
// every install downloaded and installed everything two times. These markers
// each belong to exactly one step of do_install.
//
// sn's do_install keeps TWO genuine flows — a fresh install and the
// update/reinstall path (separate `case "$operation" in` blocks) — so the
// download and unit-install steps legitimately appear once per flow. The
// count floor is therefore 2 for flow markers (a pasted third copy trips it)
// and exactly 1 for markers of single-flow steps.
func TestInstallerScriptDoInstallBodyNotDuplicated(t *testing.T) {
	b, err := os.ReadFile("../../scripts/Provider_Install_Linux.sh")
	if err != nil {
		t.Fatal(err)
	}
	// a Windows checkout may convert the script to CRLF; the markers span lines
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	for _, marker := range []string{
		`pr_info "Fetching release information for tag`, // once per flow (install, update/reinstall)
		`install_systemd_units
    fi`,
	} {
		if n := strings.Count(script, marker); n != 2 {
			t.Errorf("marker %q appears %d times in Provider_Install_Linux.sh, want exactly 2 (one per flow)", marker, n)
		}
	}
	for _, marker := range []string{
		`ensure_tools_on_path "$install_path"`,
		`pr_info "Installation complete`,
	} {
		if n := strings.Count(script, marker); n != 1 {
			t.Errorf("marker %q appears %d times in Provider_Install_Linux.sh, want exactly 1", marker, n)
		}
	}
}

// resolve_service_user is the only thing deciding which account the FreeBSD
// provider runs as, and getting it wrong means the internet-facing relay runs
// as root. These cases EXECUTE the real function from the script (extracted
// verbatim, not re-implemented) with each precedence branch.
func TestFreeBSDResolveServiceUserPrecedence(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")

	// Extract the function verbatim: from its definition to the closing brace
	// at column zero, so the test exercises shipped logic, not a copy of it.
	start := strings.Index(script, "resolve_service_user() {")
	if start < 0 {
		t.Fatal("resolve_service_user is missing from the FreeBSD installer")
	}
	rest := script[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of resolve_service_user")
	}
	fn := rest[:end+2]

	for _, c := range []struct {
		name string
		env  []string
		want string
	}{
		{"explicit override wins", []string{"SERVICE_USER_OVERRIDE=bob"}, "bob"},
		{"sudo parent beats logname", []string{"SUDO_USER=alice"}, "alice"},
		{"override beats sudo", []string{"SERVICE_USER_OVERRIDE=bob", "SUDO_USER=alice"}, "bob"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// env is set on the command, not inherited from the test process,
			// so each branch is exercised deterministically.
			cmd := exec.Command("/bin/sh", "-c", fn+"; resolve_service_user")
			cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, c.env...)
			got, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("resolve_service_user failed: %v (%s)", err, got)
			}
			if strings.TrimSpace(string(got)) != c.want {
				t.Errorf("got %q, want %q", strings.TrimSpace(string(got)), c.want)
			}
		})
	}
}

// Without sudo or an override the answer is the login owner - and it must
// never come back as an empty string, which would render an rc.d script with
// an empty user= and silently start the provider as root.
func TestFreeBSDResolveServiceUserNeverEmpty(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	// Guarded extraction: a raw Index would slice from -1 and panic the whole
	// package run if the function is renamed, instead of failing this test.
	fn := extractShellFunc(t, script, "resolve_service_user")
	if fn == "" {
		t.Fatal("could not extract resolve_service_user from the FreeBSD installer")
	}

	cmd := exec.Command("/bin/sh", "-c", fn+"; resolve_service_user")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	got, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve_service_user failed: %v (%s)", err, got)
	}
	resolved := strings.TrimSpace(string(got))
	if resolved == "" {
		t.Fatal("resolve_service_user returned empty; the rc script would render an empty user")
	}
	// With no sudo parent, no override and no login tty, the answer must be
	// this process's own user (the `id -un` step). An earlier version asserted
	// only non-emptiness, which `id -un` cannot violate, so the test could
	// never fail (mutation-proved: deleting the logname branch did not matter,
	// and neither would mis-resolving the fallthrough).
	want, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Fatalf("id -un: %v", err)
	}
	if resolved != strings.TrimSpace(string(want)) {
		t.Errorf("resolve_service_user = %q, want the current user %q", resolved, strings.TrimSpace(string(want)))
	}
}

// Updating a running provider must never copy onto the executing image:
// ETXTBSY makes `update` fail on a live box. Stage and rename instead.
func TestFreeBSDInstallerNeverCopiesOntoRunningBinary(t *testing.T) {
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")

	body := extractShellFunc(t, script, "do_install")
	if body == "" {
		t.Fatal("could not extract do_install from the installer")
	}
	// Executable lines only: a commented-out `mv -f` satisfied the old
	// string check while the live code copied (mutation-proved).
	var execLines []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		execLines = append(execLines, trimmed)
	}

	stagedRename, refusalIdx, firstFetch := -1, -1, -1
	for i, line := range execLines {
		switch {
		case strings.Contains(line, `mv -f "$staged" "$provider_bin"`):
			stagedRename = i
		case strings.Contains(line, `if [ "$service_user" = "root" ]; then`):
			if refusalIdx < 0 {
				refusalIdx = i
			}
		case strings.Contains(line, "curl -fsSL") && firstFetch < 0:
			firstFetch = i
		}
		// Any copy whose destination is the live binary is the ETXTBSY bug,
		// whatever flags it carries (`cp -f`, `cp -p`, quoted variants).
		if strings.Contains(line, "cp ") && strings.Contains(line, `"$provider_bin"`) {
			t.Errorf("do_install copies onto the live binary (ETXTBSY on update): %q", line)
		}
	}
	if stagedRename < 0 {
		t.Error("do_install does not rename the staged binary into place")
	}
	// The refusal must be INSIDE do_install and BEFORE any download. Checking
	// the whole file passed even when the refusal was deleted from do_install,
	// because do_install_service carries the same line (mutation-proved).
	if refusalIdx < 0 {
		t.Error("do_install has no refusal for a root service user")
	} else if firstFetch >= 0 && refusalIdx > firstFetch {
		t.Error("do_install downloads before refusing a root service user; the refusal must come first")
	}
	// The install tree is executed by root on every hot-swap and by rc.d on
	// restart, so it must stay ROOT-OWNED. Handing it to the service user
	// (the old behaviour) let that account replace the provider binary, which
	// is a privilege boundary the installer must not create. Only the STATE
	// directory belongs to the service user.
	if strings.Contains(body, `chown -R "$service_user" "$install_path"`) {
		t.Error("do_install hands the install tree to the service user; a root-executed binary must stay root-owned")
	}
	for _, want := range []string{
		`chown -R "$service_user" "$state_dir"`,
		`chown -R root:wheel "$install_path"`,
		`chmod -R go-w "$install_path"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("do_install is missing %q; the tree must be root-owned and not group/other-writable while the state dir stays with the service user", want)
		}
	}
	// The service must not be started when a failure to start would be
	// reported as success: the start/restart outcome is checked.
	if !strings.Contains(body, "Failed to start the service") {
		t.Error("do_install does not fail when the service fails to start")
	}
}

// OpenRC installs run the tree ROOT-OWNED at /usr/local/lib/urnetwork-provider,
// and only when the operator did not name an explicit -i path. An unset
// install_path_explicit used to make the guard `[ "" -eq 0 ]` fail with an
// "integer expected" error that aborted the redirect (H1). In H2, non-root runs
// with working sudo also redirect into the root-owned tree and set SUDO=sudo.
// These cases execute the REAL function from the script and assert what
// actually gets installed.
func TestOpenRCRedirectInstallPath(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_Linux.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	fn := extractShellFunc(t, script, "openrc_maybe_redirect_install_path")
	if fn == "" {
		t.Fatal("could not extract openrc_maybe_redirect_install_path from the Linux installer")
	}

	harness := `
pr_info() { return 0; }
pr_warn() { return 0; }
openrc_user_home() { echo /home/urnet; }
openrc_install_root=/usr/local/lib/urnetwork-provider
`

	run := func(idStub, sudoStub, setup string) string {
		cmd := exec.Command("/bin/sh", "-c",
			harness+idStub+"\n"+sudoStub+"\n"+fn+"\n"+
				`has_openrc=1; has_systemd=0; openrc_migrating=0; `+setup+"\n"+
				`install_path="/root/.local/share/urnetwork-provider"; `+
				`openrc_maybe_redirect_install_path; `+
				`echo "PATH=$install_path SUDO=${SUDO:-} ELEVATE=${openrc_elevate:-0}"`)
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("openrc_maybe_redirect_install_path failed: %v (%s)", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// Default root OpenRC install redirects into the root-owned tree.
	if got := run("id() { echo 0; }", "", "install_path_explicit=0"); !strings.HasPrefix(got, "PATH=/usr/local/lib/urnetwork-provider") {
		t.Errorf("default OpenRC root install = %q, want the root-owned tree", got)
	}
	// Explicit -i path as root is left alone.
	if got := run("id() { echo 0; }", "", "install_path_explicit=1"); !strings.HasPrefix(got, "PATH=/root/.local/share/urnetwork-provider") {
		t.Errorf("explicit -i path as root = %q, want it left alone", got)
	}
	// Non-root with working sudo redirects to root-owned tree and sets SUDO=sudo, openrc_elevate=1.
	sudoWorks := "command() { [ \"$1\" = \"-v\" ] && [ \"$2\" = \"sudo\" ]; }; sudo() { [ \"$1\" = \"-n\" ] && [ \"$2\" = \"true\" ]; }"
	if got := run("id() { echo 1000; }", sudoWorks, "install_path_explicit=0"); got != "PATH=/usr/local/lib/urnetwork-provider SUDO=sudo ELEVATE=1" {
		t.Errorf("non-root with sudo = %q, want redirect to root-owned tree with SUDO=sudo ELEVATE=1", got)
	}
	// Non-root without sudo does NOT redirect.
	sudoFails := "command() { return 1; }; sudo() { return 1; }"
	if got := run("id() { echo 1000; }", sudoFails, "install_path_explicit=0"); !strings.HasPrefix(got, "PATH=/root/.local/share/urnetwork-provider") {
		t.Errorf("non-root without sudo = %q, want no redirect", got)
	}
}

// H2: install_openrc_units must refuse an explicit install path inside the
// service user's home (where user could modify root-executed binaries) and
// must refuse unsafe paths.
func TestOpenRCInstallUnitsRefusesUnsafePath(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_Linux.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	fn := extractShellFunc(t, script, "install_openrc_units")
	if fn == "" {
		t.Fatal("could not extract install_openrc_units from the Linux installer")
	}

	harness := `
pr_info() { return 0; }
pr_warn() { return 0; }
pr_err() { return 0; }
openrc_user_home() { echo /home/urnet; }
openrc_restore_update_entry() { return 0; }
openrc_user=urnet
openrc_install_root=/usr/local/lib/urnetwork-provider
`
	runPath := func(path string) int {
		cmd := exec.Command("/bin/sh", "-c",
			harness+fn+"\n"+
				`install_path="`+path+`"; `+
				`id() { echo 0; }; `+
				`chmod() { return 0; }; `+
				`cat() { return 0; }; `+
				`install_openrc_units; exit 0`)
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		err := cmd.Run()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return exitErr.ExitCode()
			}
			return 1
		}
		return 0
	}

	// Refuses install inside service user home (exit 1).
	if code := runPath("/home/urnet/.local/share/urnetwork-provider"); code != 1 {
		t.Errorf("install inside home returned exit code %d, want 1", code)
	}
	// Refuses unsafe path outside approved tree (exit 1).
	if code := runPath("/tmp/arbitrary"); code != 1 {
		t.Errorf("install in arbitrary path returned exit code %d, want 1", code)
	}
}

// extractShellFunc returns the body of a top-level shell function, from its
// definition line to the closing brace at column 0. Used so assertions can
// target ONE function instead of the whole file, which is how a string present
// in a sibling function satisfied a check about do_install.
func extractShellFunc(t *testing.T, script, name string) string {
	t.Helper()
	// The script declares functions in two styles: `name() {` and
	// `name ()` with the brace on the following line. Match either and take
	// the earliest, so a rename-vs-decl drift cannot silently empty the body.
	start := -1
	for _, cand := range []string{name + "()", name + " ()"} {
		if i := strings.Index(script, cand); i >= 0 && (start < 0 || i < start) {
			start = i
		}
	}
	if start < 0 {
		return ""
	}
	rest := script[start:]
	lines := strings.Split(rest, "\n")
	var heredocTag string
	endIdx := -1
	charCount := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if heredocTag != "" {
			if trimmed == heredocTag {
				heredocTag = ""
			}
		} else {
			if idx := strings.Index(line, "<<"); idx >= 0 {
				tag := strings.TrimSpace(line[idx+2:])
				tag = strings.Trim(tag, "'\"")
				if tag != "" && !strings.ContainsAny(tag, " \t;") {
					heredocTag = tag
				}
			} else if i > 0 && line == "}" {
				endIdx = charCount + len(line)
				break
			}
		}
		charCount += len(line) + 1
	}
	if endIdx < 0 {
		return ""
	}
	return rest[:endIdx]
}

// A release asset with no digest must read as ABSENT. jq prints the string
// "null" for a missing or null field, which passed the non-empty test and then
// failed the comparison, so every install fell back to the shell wrapper while
// reporting success. These cases run the real expression out of the shipped
// installer against the JSON shapes the releases API actually returns.
func TestFreeBSDToolDigestLookup(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	i := strings.Index(script, `tool_digest="$(printf`)
	j := strings.Index(script, `if [ -n "$tool_digest" ]`)
	if i < 0 || j < 0 || j <= i {
		t.Fatal("could not locate the tool digest lookup in the FreeBSD installer")
	}
	block := script[i:j]

	const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		json string
		want string
	}{
		{"digest field absent", `{"assets":[{"name":"urnet-tools-freebsd-amd64"}]}`, ""},
		{"digest explicitly null", `{"assets":[{"name":"urnet-tools-freebsd-amd64","digest":null}]}`, ""},
		{"digest empty string", `{"assets":[{"name":"urnet-tools-freebsd-amd64","digest":""}]}`, ""},
		{"not a hex digest", `{"assets":[{"name":"urnet-tools-freebsd-amd64","digest":"deadbeef"}]}`, ""},
		{"different algorithm", `{"assets":[{"name":"urnet-tools-freebsd-amd64","digest":"sha512:` + hex64 + `"}]}`, ""},
		{"other asset matched", `{"assets":[{"name":"urnet-tools-linux-amd64","digest":"sha256:` + hex64 + `"}]}`, ""},
		{"real sha256", `{"assets":[{"name":"urnet-tools-freebsd-amd64","digest":"sha256:` + hex64 + `"}]}`, hex64},
		{"bare hex", `{"assets":[{"name":"urnet-tools-freebsd-amd64","digest":"` + hex64 + `"}]}`, hex64},
	}

	// jq and python3 are PACKAGES on FreeBSD, not base system: the FreeBSD CI
	// VM has neither unless installed. Skip a path whose tool is absent (and
	// skip the whole test when both are) rather than failing on the host's
	// package set — but never skip SILENTLY for a missing marker, which is a
	// real drift and must fail.
	haveJq := false
	if _, err := exec.LookPath("jq"); err == nil {
		haveJq = true
	}
	havePy := false
	if _, err := exec.LookPath("python3"); err == nil {
		havePy = true
	}
	if !haveJq && !havePy {
		t.Skip("neither jq nor python3 on this host; cannot execute the digest expressions")
	}

	// jq path: the expression is embedded in single quotes in the shell.
	// A missing marker is a FAILURE, not a skip: the previous version wrapped
	// this in `if found {`, so adding one space to the expression made every
	// jq subtest vanish while the test reported success (mutation-proved).
	jqStart := strings.Index(block, "jq -r --arg a \"$tool_asset\" '")
	if !haveJq {
		t.Log("jq not installed here; exercising only the python path")
	} else if jqStart < 0 {
		t.Error("could not find the jq digest expression in the installer; the marker moved and this test would otherwise pass vacuously")
	} else {
		expr := block[jqStart+len("jq -r --arg a \"$tool_asset\" '"):]
		if end := strings.Index(expr, "' 2>/dev/null"); end >= 0 {
			expr = expr[:end]
			for _, c := range cases {
				t.Run("jq/"+c.name, func(t *testing.T) {
					// jq reads the release JSON on stdin, so it must be wired:
					// Output() alone would leave it with no input and it would
					// quietly return nothing for every case.
					cmd := exec.Command("jq", "-r", "--arg", "a", "urnet-tools-freebsd-amd64", expr)
					cmd.Stdin = strings.NewReader(c.json)
					out, err := cmd.Output()
					if err != nil {
						t.Fatalf("jq failed: %v", err)
					}
					if got := strings.TrimSpace(string(out)); got != c.want {
						t.Errorf("got %q, want %q", got, c.want)
					}
				})
			}
		}
	}

	// python path; same rule: a missing marker fails rather than skips.
	pyStart := strings.Index(block, "python3 -c '")
	if !havePy {
		t.Log("python3 not installed here; exercising only the jq path")
	} else if pyStart < 0 {
		t.Error("could not find the python digest expression in the installer; the marker moved and this test would otherwise pass vacuously")
	} else {
		py := block[pyStart+len("python3 -c '"):]
		pyEnd := strings.Index(py, "' \"$tool_asset\"")
		if pyEnd < 0 {
			t.Error("could not find the end of the python digest expression; the marker moved")
		} else {
			py = py[:pyEnd]
			for _, c := range cases {
				t.Run("py/"+c.name, func(t *testing.T) {
					cmd := exec.Command("python3", "-c", py, "urnet-tools-freebsd-amd64")
					cmd.Stdin = strings.NewReader(c.json)
					out, err := cmd.Output()
					if err != nil {
						t.Fatalf("python failed: %v", err)
					}
					if got := strings.TrimSpace(string(out)); got != c.want {
						t.Errorf("got %q, want %q", got, c.want)
					}
				})
			}
		}
	}
}

// H4 + L1: openrc_prompt_auth must read the code silently (stty -echo or read -rs)
// and pass it to urnetwork via URNETWORK_AUTH_CODE and auth -f without term.ReadPassword.
func TestOpenRCPromptAuthNonInteractiveHop(t *testing.T) {
	b, err := os.ReadFile("../../scripts/Provider_Install_Linux.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	fn := extractShellFunc(t, script, "openrc_prompt_auth")
	if fn == "" {
		t.Fatal("could not extract openrc_prompt_auth from the Linux installer")
	}

	if !strings.Contains(fn, "URNETWORK_AUTH_CODE=$c exec urnetwork auth -f") {
		t.Error("openrc_prompt_auth does not pass code via URNETWORK_AUTH_CODE or missing load-bearing -f")
	}
	if !strings.Contains(fn, "stty -echo") && !strings.Contains(fn, "read -rs") {
		t.Error("openrc_prompt_auth does not suppress echo (L1 violation)")
	}
}

// skipWithoutPOSIXShell skips tests that execute the FreeBSD installer with
// /bin/sh. The suite also runs on Windows, where that binary does not exist,
// so without this the test fails on a platform it was never meant to
// exercise. The FreeBSD check runs these against a real kernel and the Linux
// lifecycle check runs them under /bin/sh.
func skipWithoutPOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires /bin/sh; this suite also runs on Windows")
	}
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skipf("no /bin/sh on this host: %v", err)
	}
}
