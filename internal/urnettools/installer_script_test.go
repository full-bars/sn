package urnettools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	if runtime.GOOS != "linux" {
		t.Skip("service-user resolution differs off linux (macOS resolves the login user)")
	}
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
	// and neither would mis-resolving the fallthrough). The expectation mirrors
	// the function's own order — logname first, id -un second — because
	// glibc getlogin() reads /proc/self/loginuid, which survives sudo: a
	// developer running `sudo go test` gets their own login name from the
	// child's logname, not root from id -un.
	want := ""
	if out, err := exec.Command("logname").Output(); err == nil {
		want = strings.TrimSpace(string(out))
	}
	if want == "" {
		out, err := exec.Command("id", "-un").Output()
		if err != nil {
			t.Fatalf("id -un: %v", err)
		}
		want = strings.TrimSpace(string(out))
	}
	if resolved != want {
		t.Errorf("resolve_service_user = %q, want %q", resolved, want)
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
	// The state-dir chown carries -h: do_install re-checks the directory right
	// before this root write, and the recursive pass must not dereference a
	// symlink the service user swapped in between the two checks.
	for _, want := range []string{
		`chown -R -h "$service_user" "$state_dir"`,
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

// The version files must be written only after the service step has succeeded.
// They used to be written before it, so a failed start left .version and .date
// naming a release that never ran, and a rollback put the old binary back under
// files that claimed the new one. The code order is right now, but nothing
// pinned it: moving either write earlier kept the suite green.
func TestLinuxVersionFilesWrittenOnlyAfterSuccessfulStart(t *testing.T) {
	tag := "v2026.10.27"
	assetName := "urnetwork-provider-" + tag + ".tar.gz"
	tarballContent := []byte("provider-tarball-bytes")
	sum := sha256.Sum256(tarballContent)
	digest := hex.EncodeToString(sum[:])
	releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [{"name": %q, "digest": "sha256:%s"}]
	}`, tag, assetName, digest)

	read := func(dir, name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}

	t.Run("failed start leaves the previous version files", func(t *testing.T) {
		res := runLinuxInstaller(t, installerHarnessOpts{
			tag:             tag,
			releaseJSON:     releaseJSON,
			tarballData:     tarballContent,
			existingBin:     "previous-provider-binary",
			existingVersion: "v-previous",
			existingDate:    "1700000000",
			failRestart:     true,
		})
		if res.exitCode == 0 {
			t.Fatalf("a failed service start reported success (out:\n%s)", res.stdout)
		}
		if got := read(res.installDir, ".version"); got != "v-previous" {
			t.Errorf(".version = %q after a failed start, want the previous value v-previous: the write ran before the service proved up", got)
		}
		if got := read(res.installDir, ".date"); got != "1700000000" {
			t.Errorf(".date = %q after a failed start, want the previous value 1700000000: the write ran before the service proved up", got)
		}
	})

	t.Run("successful start records the new release", func(t *testing.T) {
		res := runLinuxInstaller(t, installerHarnessOpts{
			tag:             tag,
			releaseJSON:     releaseJSON,
			tarballData:     tarballContent,
			existingBin:     "previous-provider-binary",
			existingVersion: "v-previous",
			existingDate:    "1700000000",
		})
		if res.exitCode != 0 {
			t.Fatalf("a successful start did not install, exit %d (out:\n%s)", res.exitCode, res.stdout)
		}
		if got := read(res.installDir, ".version"); got == "" || got == "v-previous" {
			t.Errorf(".version = %q after a successful start, want the installed release", got)
		}
		if got := read(res.installDir, ".date"); got == "" || got == "1700000000" {
			t.Errorf(".date = %q after a successful start, want the release date", got)
		}
	})
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
			if idx := strings.Index(line, "<<"); idx >= 0 && !strings.HasPrefix(trimmed, "#") {
				rem := line[idx+2:]
				if !strings.HasPrefix(rem, "<") { // ignore here-strings <<<
					rem = strings.TrimPrefix(rem, "-")
					rem = strings.TrimSpace(rem)
					var tag string
					if strings.HasPrefix(rem, "'") {
						if closeIdx := strings.Index(rem[1:], "'"); closeIdx >= 0 {
							tag = rem[1 : 1+closeIdx]
						}
					} else if strings.HasPrefix(rem, "\"") {
						if closeIdx := strings.Index(rem[1:], "\""); closeIdx >= 0 {
							tag = rem[1 : 1+closeIdx]
						}
					} else {
						end := strings.IndexAny(rem, " \t;<>|&)")
						if end >= 0 {
							tag = rem[:end]
						} else {
							tag = rem
						}
					}
					tag = strings.Trim(tag, "'\"\\")
					if tag != "" {
						heredocTag = tag
					}
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
		end := strings.Index(expr, "' 2>/dev/null")
		if end < 0 {
			t.Error("could not find the end of the jq digest expression; the marker moved and this test would otherwise pass vacuously")
		} else {
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

func TestExtractShellFuncHeredocHandling(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   string
	}{
		{
			name: "dash heredoc <<-EOF with inner brace",
			script: `some_func ()
{
	cat <<-EOF
	line with } brace
EOF
	echo done
}`,
			want: `some_func ()
{
	cat <<-EOF
	line with } brace
EOF
	echo done
}`,
		},
		{
			name: "single-quoted heredoc << 'EOF' with inner brace",
			script: `single_quote () {
	cat << 'EOF'
	}
EOF
	return 0
}`,
			want: `single_quote () {
	cat << 'EOF'
	}
EOF
	return 0
}`,
		},
		{
			name: "dash and single-quoted heredoc <<- 'EOF' with space and inner brace",
			script: `dash_single () {
	cat <<- 'EOF'
	}
EOF
}`,
			want: `dash_single () {
	cat <<- 'EOF'
	}
EOF
}`,
		},
		{
			name: "double-quoted heredoc << \"EOF\" with redirection and inner brace",
			script: `double_quote ()
{
	cat << "EOF" > /dev/null
	}
EOF
	exit 0
}`,
			want: `double_quote ()
{
	cat << "EOF" > /dev/null
	}
EOF
	exit 0
}`,
		},
		{
			name: "heredoc with space before tag and pipe << EOF | grep",
			script: `spaced_heredoc () {
	cat << EOF | grep foo
	}
EOF
}`,
			want: `spaced_heredoc () {
	cat << EOF | grep foo
	}
EOF
}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractShellFunc(t, tc.script, strings.Fields(tc.script)[0])
			if got != tc.want {
				t.Errorf("extractShellFunc mismatch:\ngot:\n%s\nwant:\n%s", got, tc.want)
			}
		})
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

type installerHarnessOpts struct {
	tag         string
	releaseJSON string
	tarballData []byte
	existingBin string
	failRestart bool
	failCpToDst bool
	failMv      bool
	operation   string
	// metadataFails makes every release API fetch fail, as an unreachable or
	// rate-limited GitHub API does.
	metadataFails bool
	// restartFailFirst fails only the first service (re)start, so a rollback
	// that restarts the restored binary can succeed. Both installers honour it.
	restartFailFirst bool
	// failRestore makes the Linux rollback's move of the previous binary back
	// into place fail, as a read-only or vanished install tree does.
	failRestore bool
	// failBackup makes every attempt to create the rollback copy fail (the
	// Linux .old file, the FreeBSD .urnetwork.bak file), including the fallback
	// copies, while the staged binary is still written. The swap must abort.
	failBackup bool
	// serviceStopped starts the FreeBSD service stopped.
	serviceStopped bool
	// existingVersion seeds the version file the installer rewrites.
	existingVersion string
	// existingDate seeds the Linux .date file (epoch seconds) the installer
	// rewrites. FreeBSD keeps no date file.
	existingDate string
}

type installerHarnessResult struct {
	exitCode int
	stdout   string
	stderr   string
	tarCalls []string
	finalBin string
	// svcCalls is the FreeBSD service log: one line per start or restart,
	// with the binary that was live at that moment.
	svcCalls []string
	// installDir is the install tree, so tests can read the version file.
	installDir string
	// verifyCalls logs each sha256 comparison the installer made, as
	// "verify <expected digest>". Proceeding is not proof that verification ran;
	// a call with the release digest is.
	verifyCalls []string
}

// verifyLoggingHarness wraps the installer's real verify_sha256_file. The
// extracted implementation is renamed, so every call goes through the wrapper,
// which records the expected digest and then delegates to the real check.
func verifyLoggingHarness(fnVerify string) (harness, renamed string) {
	renamed = strings.Replace(fnVerify, "verify_sha256_file", "verify_sha256_file_impl", 1)
	harness = `
verify_sha256_file() {
    echo "verify $2" >> "$VERIFY_LOG"
    verify_sha256_file_impl "$@"
}
`
	return harness, renamed
}

// installerFlag renders a Go bool as the 0/1 the shell harness compares with.
func installerFlag(on bool) string {
	if on {
		return "1"
	}
	return "0"
}

// readInstallerLines returns the non-empty lines of a harness log file.
func readInstallerLines(path string) []string {
	var lines []string
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func runFreeBSDInstaller(t *testing.T, opts installerHarnessOpts) installerHarnessResult {
	t.Helper()
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_FreeBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")

	tempDir := t.TempDir()
	installPath := filepath.Join(tempDir, "install")
	stateDir := filepath.Join(tempDir, "state")
	rcScript := filepath.Join(tempDir, "rc.d", "urnetwork")
	tarballPath := filepath.Join(tempDir, "provider.tar.gz")
	releaseJSONPath := filepath.Join(tempDir, "release.json")
	logDir := filepath.Join(tempDir, "logs")
	_ = os.MkdirAll(logDir, 0o755)

	svcState := filepath.Join(tempDir, "svc.state")
	svcLog := filepath.Join(tempDir, "svc.log")

	if err := os.WriteFile(tarballPath, opts.tarballData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releaseJSONPath, []byte(opts.releaseJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if opts.existingBin != "" {
		_ = os.MkdirAll(filepath.Join(installPath, "bin"), 0o755)
		_ = os.WriteFile(filepath.Join(installPath, "bin", "urnetwork"), []byte(opts.existingBin), 0o755)
	}
	if opts.existingVersion != "" {
		_ = os.MkdirAll(installPath, 0o755)
		_ = os.WriteFile(filepath.Join(installPath, "version"), []byte(opts.existingVersion+"\n"), 0o644)
	}
	svcStart := "running"
	if opts.serviceStopped {
		svcStart = "stopped"
	}

	fnDigest := extractShellFunc(t, script, "get_asset_digest_from_api_response")
	fnCompute := extractShellFunc(t, script, "compute_sha256_file")
	verifyHarness, fnVerify := verifyLoggingHarness(extractShellFunc(t, script, "verify_sha256_file"))
	fnLists := extractShellFunc(t, script, "release_lists_asset")
	fnDoInstall := extractShellFunc(t, script, "do_install")
	if fnDoInstall == "" {
		t.Fatal("could not extract do_install from FreeBSD script")
	}
	verifyLog := filepath.Join(tempDir, "verify.log")

	tag := opts.tag
	if tag == "" {
		tag = "v2026.10.27"
	}
	failRestartVal := "0"
	if opts.failRestart {
		failRestartVal = "1"
	}
	failCpVal := "0"
	if opts.failCpToDst {
		failCpVal = "1"
	}
	failMvVal := "0"
	if opts.failMv {
		failMvVal = "1"
	}

	harness := fmt.Sprintf(`
pr_err() { printf "[ERROR] %%s\n" "$*" >&2; }
pr_warn() { printf "[WARN] %%s\n" "$*" >&2; }
pr_info() { printf "[INFO] %%s\n" "$*"; }
priv() { "$@"; }
sudo() { "$@"; }
user_shell() { echo "/bin/sh"; }`+verifyHarness+`
assert_state_path_safe() { return 0; }
set_rc_conf_default() { return 0; }
write_rc_script_body() { echo "# rc"; }
install_wrapper_tool() { return 0; }
resolve_service_user() { echo "testuser"; }
# The service is stateful: SVC_STATE holds running|stopped, and every start
# or restart is logged with the binary that was live at that moment. A test
# can then tell "restart attempted on the restored binary" from "never called".
service_running() { [ "$(cat "$SVC_STATE" 2>/dev/null)" = running ]; }
svc_call() {
    _n="$(cat "$SVC_LOG.n" 2>/dev/null || echo 0)"
    _n=$((_n + 1))
    echo "$_n" > "$SVC_LOG.n"
    echo "$1 #$_n live=$(cat "$provider_bin" 2>/dev/null)" >> "$SVC_LOG"
    if [ "${FAIL_RESTART:-0}" = "1" ] || { [ "${FAIL_RESTART_FIRST:-0}" = "1" ] && [ "$_n" -eq 1 ]; }; then
        echo stopped > "$SVC_STATE"
        return 1
    fi
    echo running > "$SVC_STATE"
    return 0
}
restart_service() { svc_call restart; }
load_service() { svc_call load; }
http_fetch() {
    _url="$1"
    _out="${2:-}"
    case "$_url" in
        http://mock/*)
            if [ "${METADATA_FAILS:-0}" = "1" ]; then
                return 1
            fi
            ;;
    esac
    if [ -n "$_out" ]; then
        cp %q "$_out"
        return 0
    else
        cat %q
        return 0
    fi
}
cp() {
    if [ "${FAIL_CP:-0}" = "1" ]; then
        case "$*" in
            *"/bin/.urnetwork"*|*"/bin/urnetwork"*)
                echo "simulated cp failure" >&2
                return 1
                ;;
        esac
    fi
    if [ "${FAIL_BACKUP:-0}" = "1" ]; then
        case "$*" in
            *".urnetwork.bak."*)
                echo "simulated backup failure" >&2
                return 1
                ;;
        esac
    fi
    command cp "$@"
}
ln() {
    if [ "${FAIL_BACKUP:-0}" = "1" ]; then
        case "$*" in
            *".urnetwork.bak."*)
                echo "simulated backup failure" >&2
                return 1
                ;;
        esac
    fi
    command ln "$@"
}
mv() {
    if [ "${FAIL_MV:-0}" = "1" ]; then
        case "$*" in
            *"-f "*"$install_path/bin/urnetwork")
                echo "simulated mv failure" >&2
                return 1
                ;;
        esac
    fi
    command mv "$@"
}
tar() {
    echo "tar $*" >> %q
    if [ -d "$tmpdir" ]; then
        mkdir -p "$tmpdir/freebsd/amd64"
        echo "extracted-new-binary" > "$tmpdir/freebsd/amd64/provider"
        chmod 755 "$tmpdir/freebsd/amd64/provider"
    fi
    return 0
}
`, tarballPath, releaseJSONPath, filepath.Join(logDir, "tar.log"))

	driver := fmt.Sprintf(`
service_user="testuser"
version=%q
install_path=%q
provider_bin="$install_path/bin/urnetwork"
state_dir=%q
rc_script=%q
me="urnet-tools"
github_api="http://mock"
FAIL_RESTART=%q
FAIL_RESTART_FIRST=%q
FAIL_CP=%q
FAIL_MV=%q
METADATA_FAILS=%q
SVC_STATE=%q
SVC_LOG=%q
VERIFY_LOG=%q
echo %s > "$SVC_STATE"
do_install %q
`, tag, installPath, stateDir, rcScript, failRestartVal, installerFlag(opts.restartFailFirst), failCpVal, failMvVal, installerFlag(opts.metadataFails), svcState, svcLog, verifyLog, svcStart, tag)

	fullScript := harness + "\n" + fnDigest + "\n" + fnCompute + "\n" + fnVerify + "\n" + fnLists + "\n" + fnDoInstall + "\n" + driver
	cmd := exec.Command("/bin/sh", "-c", fullScript)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if opts.failBackup {
		cmd.Env = append(cmd.Env, "FAIL_BACKUP=1")
	}
	out, err := cmd.CombinedOutput()

	var exitCode int
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	var tarCalls []string
	if tarLogBytes, rerr := os.ReadFile(filepath.Join(logDir, "tar.log")); rerr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(tarLogBytes)), "\n") {
			if strings.TrimSpace(line) != "" {
				tarCalls = append(tarCalls, line)
			}
		}
	}

	finalBinBytes, _ := os.ReadFile(filepath.Join(installPath, "bin", "urnetwork"))

	return installerHarnessResult{
		exitCode:    exitCode,
		stdout:      string(out),
		stderr:      string(out),
		tarCalls:    tarCalls,
		finalBin:    string(finalBinBytes),
		svcCalls:    readInstallerLines(svcLog),
		installDir:  installPath,
		verifyCalls: readInstallerLines(verifyLog),
	}
}

func runLinuxInstaller(t *testing.T, opts installerHarnessOpts) installerHarnessResult {
	t.Helper()
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_Linux.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")

	tempDir := t.TempDir()
	installPath := filepath.Join(tempDir, "install")
	tarballPath := filepath.Join(tempDir, "provider.tar.gz")
	releaseJSONPath := filepath.Join(tempDir, "release.json")
	logDir := filepath.Join(tempDir, "logs")
	_ = os.MkdirAll(logDir, 0o755)

	if err := os.WriteFile(tarballPath, opts.tarballData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releaseJSONPath, []byte(opts.releaseJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if opts.existingBin != "" {
		_ = os.MkdirAll(filepath.Join(installPath, "bin"), 0o755)
		_ = os.WriteFile(filepath.Join(installPath, "bin", "urnetwork"), []byte(opts.existingBin), 0o755)
	}
	if opts.existingVersion != "" {
		_ = os.MkdirAll(installPath, 0o755)
		_ = os.WriteFile(filepath.Join(installPath, ".version"), []byte(opts.existingVersion+"\n"), 0o644)
	}
	if opts.existingDate != "" {
		_ = os.MkdirAll(installPath, 0o755)
		_ = os.WriteFile(filepath.Join(installPath, ".date"), []byte(opts.existingDate+"\n"), 0o644)
	}

	fnGetVersion := extractShellFunc(t, script, "get_version_from_api_response")
	fnGetDate := extractShellFunc(t, script, "get_release_date_from_api_response")
	fnDigest := extractShellFunc(t, script, "get_asset_digest_from_api_response")
	fnCompute := extractShellFunc(t, script, "compute_sha256_file")
	verifyHarness, fnVerify := verifyLoggingHarness(extractShellFunc(t, script, "verify_sha256_file"))
	fnLists := extractShellFunc(t, script, "release_lists_asset")
	verifyLog := filepath.Join(tempDir, "verify.log")
	svcLog := filepath.Join(tempDir, "svc.log")
	firstStartFlag := filepath.Join(tempDir, "first-start-done")
	fnSanitize := extractShellFunc(t, script, "sanitize_restart_dropins")
	fnInstallSystemd := extractShellFunc(t, script, "install_systemd_units")
	fnDoInstall := extractShellFunc(t, script, "do_install")
	if fnDoInstall == "" {
		t.Fatal("could not extract do_install from Linux script")
	}

	tag := opts.tag
	if tag == "" {
		tag = "v2026.10.27"
	}
	op := opts.operation
	if op == "" {
		op = "install"
	}
	failRestartVal := "0"
	if opts.failRestart {
		failRestartVal = "1"
	}
	failCpVal := "0"
	if opts.failCpToDst {
		failCpVal = "1"
	}
	failMvVal := "0"
	if opts.failMv {
		failMvVal = "1"
	}
	failFirstStartVal := "0"
	if opts.restartFailFirst {
		failFirstStartVal = "1"
	}
	failRestoreVal := "0"
	if opts.failRestore {
		failRestoreVal = "1"
	}

	harness := fmt.Sprintf(`
pr_err() { printf "[ERROR] %%s\n" "$*" >&2; }
pr_warn() { printf "[WARN] %%s\n" "$*" >&2; }
pr_info() { printf "[INFO] %%s\n" "$*"; }
sudo() { "$@"; }`+verifyHarness+`
install_downloader() { return 0; }
install_json_parser() { return 0; }
openrc_maybe_redirect_install_path() { return 0; }
stop_systemd_units() { return 0; }
stop_openrc_units() { return 0; }
ensure_tools_on_path() { return 0; }
add_path_blocks() { return 0; }
func_root_guard() { return 0; }
network_fetch() {
    if [ "${METADATA_FAILS:-0}" = "1" ]; then
        return 1
    fi
    cat %q
}
download_asset() {
    _src="$1"
    _dst="$2"
    cp %q "$_dst"
}
cp() {
    if [ "${FAIL_CP:-0}" = "1" ]; then
        case "$*" in
            *"/bin/.urnetwork"*|*"/bin/urnetwork"*)
                echo "simulated cp failure" >&2
                return 1
                ;;
        esac
    fi
    command cp "$@"
}
mv() {
    if [ "${FAIL_MV:-0}" = "1" ]; then
        case "$*" in
            *"-f "*"$install_path/bin/urnetwork")
                echo "simulated mv failure" >&2
                return 1
                ;;
        esac
    fi
    command mv "$@"
}
tar() {
    echo "tar $*" >> %q
    if [ -d "$workdir" ]; then
        mkdir -p "$workdir/linux/amd64"
        echo "extracted-new-binary" > "$workdir/linux/amd64/provider"
        chmod 755 "$workdir/linux/amd64/provider"
    fi
    return 0
}
cp() {
    if [ "${FAIL_CP:-0}" = "1" ]; then
        case "$*" in
            *"/bin/.urnetwork"*|*"/bin/urnetwork"*)
                echo "simulated cp failure" >&2
                return 1
                ;;
        esac
    fi
    if [ "${FAIL_BACKUP:-0}" = "1" ]; then
        case "$*" in
            *"/bin/urnetwork.old"*)
                echo "simulated backup failure" >&2
                return 1
                ;;
        esac
    fi
    command cp "$@"
}
mv() {
    if [ "${FAIL_MV:-0}" = "1" ]; then
        case "$*" in
            *"-f "*"$install_path/bin/urnetwork")
                echo "simulated mv failure" >&2
                return 1
                ;;
        esac
    fi
    if [ "${FAIL_RESTORE:-0}" = "1" ]; then
        case "$*" in
            *"urnetwork.old"*)
                echo "simulated restore failure" >&2
                return 1
                ;;
        esac
    fi
    command mv "$@"
}
systemctl() {
    if [ "$1" = "--user" ] && [ "$2" = "start" ]; then
        printf 'start live=%%s\n' "$(cat "$install_path/bin/urnetwork" 2>/dev/null)" >> "$SVC_LOG"
        if [ "${FAIL_RESTART:-0}" = "1" ]; then
            return 1
        fi
        if [ "${FAIL_RESTART_FIRST:-0}" = "1" ] && [ ! -e "$FIRST_START_FLAG" ]; then
            : > "$FIRST_START_FLAG"
            return 1
        fi
    fi
    return 0
}
`, releaseJSONPath, tarballPath, filepath.Join(logDir, "tar.log"))

	driver := fmt.Sprintf(`
tag=%q
operation=%q
install_path=%q
script_rundir=%q
api_base="http://mock"
urnet_github_dl_base="http://mock"
urnet_dl_base="http://mock"
api_url="http://mock/releases/tags/$tag"
arch="amd64"
has_systemd=1
has_openrc=0
FAIL_RESTART=%q
FAIL_RESTART_FIRST=%q
FAIL_RESTORE=%q
FAIL_CP=%q
FAIL_MV=%q
METADATA_FAILS=%q
VERIFY_LOG=%q
SVC_LOG=%q
FIRST_START_FLAG=%q
systemd_service="%s/urnetwork.service"
systemd_update_service="%s/urnetwork-update.service"
systemd_update_timer="%s/urnetwork-update.timer"
systemd_userdir="%s"
systemd_units_stopped=1
do_install
`, tag, op, installPath, tempDir, failRestartVal, failFirstStartVal, failRestoreVal, failCpVal, failMvVal, installerFlag(opts.metadataFails), verifyLog, svcLog, firstStartFlag, tempDir, tempDir, tempDir, tempDir)

	fullScript := harness + "\n" + fnGetVersion + "\n" + fnGetDate + "\n" + fnDigest + "\n" + fnCompute + "\n" + fnVerify + "\n" + fnLists + "\n" + fnSanitize + "\n" + fnInstallSystemd + "\n" + fnDoInstall + "\n" + driver
	cmd := exec.Command("/bin/sh", "-c", fullScript)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if opts.failBackup {
		cmd.Env = append(cmd.Env, "FAIL_BACKUP=1")
	}
	out, err := cmd.CombinedOutput()

	var exitCode int
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	var tarCalls []string
	if tarLogBytes, rerr := os.ReadFile(filepath.Join(logDir, "tar.log")); rerr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(tarLogBytes)), "\n") {
			if strings.TrimSpace(line) != "" {
				tarCalls = append(tarCalls, line)
			}
		}
	}

	finalBinBytes, _ := os.ReadFile(filepath.Join(installPath, "bin", "urnetwork"))

	return installerHarnessResult{
		exitCode:    exitCode,
		stdout:      string(out),
		stderr:      string(out),
		tarCalls:    tarCalls,
		finalBin:    string(finalBinBytes),
		installDir:  installPath,
		verifyCalls: readInstallerLines(verifyLog),
		svcCalls:    readInstallerLines(svcLog),
	}
}

// 1. A provider tarball whose digest does not match: the install aborts before
// extraction and the error names the asset and digests.
func TestInstallerProviderTarballDigestMismatchAborts(t *testing.T) {
	tarballContent := []byte("actual-tarball-payload-bytes")
	sum := sha256.Sum256(tarballContent)
	actualDigest := hex.EncodeToString(sum[:])
	expectedDigest := strings.Repeat("d", 64)

	tag := "v2026.10.27"
	assetName := "urnetwork-provider-" + tag + ".tar.gz"
	releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": %q, "digest": "sha256:%s"}
		]
	}`, tag, assetName, expectedDigest)

	for _, tc := range []struct {
		name string
		run  func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", runFreeBSDInstaller},
		{"Linux", runLinuxInstaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
			})
			if res.exitCode == 0 {
				t.Fatalf("expected install to abort on digest mismatch, got exit 0 (out:\n%s)", res.stdout)
			}
			if len(res.tarCalls) != 0 {
				t.Fatalf("tar was invoked (%v); must abort before extraction", res.tarCalls)
			}
			combinedOut := res.stdout + "\n" + res.stderr
			if !strings.Contains(combinedOut, assetName) {
				t.Errorf("error output missing asset name %q:\n%s", assetName, combinedOut)
			}
			if !strings.Contains(combinedOut, expectedDigest) {
				t.Errorf("error output missing expected digest %q:\n%s", expectedDigest, combinedOut)
			}
			if !strings.Contains(combinedOut, actualDigest) {
				t.Errorf("error output missing actual digest %q:\n%s", actualDigest, combinedOut)
			}
		})
	}
}

// 2. A matching digest: extraction proceeds.
func TestInstallerProviderTarballDigestMatchProceeds(t *testing.T) {
	tarballContent := []byte("valid-matching-tarball-bytes")
	sum := sha256.Sum256(tarballContent)
	actualDigest := hex.EncodeToString(sum[:])

	tag := "v2026.10.27"
	assetName := "urnetwork-provider-" + tag + ".tar.gz"
	releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": %q, "digest": "sha256:%s"}
		]
	}`, tag, assetName, actualDigest)

	for _, tc := range []struct {
		name string
		run  func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", runFreeBSDInstaller},
		{"Linux", runLinuxInstaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
			})
			if res.exitCode != 0 {
				t.Fatalf("expected matching digest install to succeed, got exit %d (out:\n%s)", res.exitCode, res.stdout)
			}
			if len(res.tarCalls) == 0 {
				t.Fatal("expected tar extraction to proceed, but tar was never invoked")
			}
			if strings.Contains(res.stdout+res.stderr, "mismatch") {
				t.Errorf("unexpected mismatch message in output:\n%s", res.stdout)
			}
			// Proceeding is not proof of verification: a skipped digest check
			// also proceeds, and a printed "verified" line can outlive a skipped
			// check. So the comparison itself must have run, and every comparison
			// must have used the release digest. The count is not pinned to one:
			// the Linux do_install body currently runs its download block twice,
			// so a Linux install compares the tarball more than once.
			want := "verify " + actualDigest
			if len(res.verifyCalls) == 0 {
				t.Errorf("no digest comparison ran, so the install proceeded unverified (out:\n%s)", res.stdout)
			}
			for _, call := range res.verifyCalls {
				if call != want {
					t.Errorf("digest comparison %q, want %q: verification ran against the wrong digest\n(out:\n%s)",
						call, want, res.stdout)
				}
			}
		})
	}
}

// 3. A release with no digest: one warning, install proceeds.
func TestInstallerProviderTarballNoDigestWarnsAndProceeds(t *testing.T) {
	tarballContent := []byte("tarball-without-release-digest")
	tag := "v2026.10.27"
	assetName := "urnetwork-provider-" + tag + ".tar.gz"
	releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": %q}
		]
	}`, tag, assetName)

	for _, tc := range []struct {
		name string
		run  func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", runFreeBSDInstaller},
		{"Linux", runLinuxInstaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
			})
			if res.exitCode != 0 {
				t.Fatalf("expected missing digest install to proceed, got exit %d (out:\n%s)", res.exitCode, res.stdout)
			}
			if len(res.tarCalls) == 0 {
				t.Fatal("expected tar extraction to proceed for undigested release")
			}
			combinedOut := res.stdout + "\n" + res.stderr
			if strings.Contains(combinedOut, "mismatch") {
				t.Errorf("missing digest must not be treated as a mismatch:\n%s", combinedOut)
			}
			hasProviderWarn := false
			for _, line := range strings.Split(combinedOut, "\n") {
				if strings.Contains(line, "[WARN]") && strings.Contains(line, assetName) {
					hasProviderWarn = true
					break
				}
			}
			if !hasProviderWarn {
				t.Errorf("expected warning specifically naming unverified asset %q:\n%s", assetName, combinedOut)
			}
		})
	}
}

// 4. An interrupted replacement (stub mv/cp to fail at the right moment)
// leaves the configured ExecPath present or restores the backup — never absent.
func TestInstallerInterruptedReplacementLeavesBinaryIntact(t *testing.T) {
	existingPayload := "pre-existing-working-binary"
	tarballContent := []byte("new-tarball-payload")
	tag := "v2026.10.27"
	assetName := "urnetwork-provider-" + tag + ".tar.gz"
	sum := sha256.Sum256(tarballContent)
	actualDigest := hex.EncodeToString(sum[:])
	releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": %q, "digest": "sha256:%s"}
		]
	}`, tag, assetName, actualDigest)

	for _, tc := range []struct {
		name string
		run  func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", runFreeBSDInstaller},
		{"Linux", runLinuxInstaller},
	} {
		t.Run(tc.name+"/cp_failure", func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				operation:   "update",
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
				existingBin: existingPayload,
				failCpToDst: true,
			})
			if res.finalBin == "" {
				t.Fatalf("interrupted replacement left executable absent! out:\n%s", res.stdout)
			}
			if res.finalBin != existingPayload {
				t.Errorf("interrupted replacement changed binary, want %q, got %q", existingPayload, res.finalBin)
			}
		})
		t.Run(tc.name+"/mv_failure", func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				operation:   "update",
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
				existingBin: existingPayload,
				failMv:      true,
			})
			if res.finalBin == "" {
				t.Fatalf("interrupted replacement left executable absent! out:\n%s", res.stdout)
			}
			if res.finalBin != existingPayload {
				t.Errorf("interrupted replacement changed binary, want %q, got %q", existingPayload, res.finalBin)
			}
		})
	}
}

// 5. A failed restart after install restores the previous binary.
func TestInstallerFailedRestartRestoresBackup(t *testing.T) {
	existingPayload := "pre-existing-working-binary"
	tarballContent := []byte("new-tarball-payload")
	tag := "v2026.10.27"
	assetName := "urnetwork-provider-" + tag + ".tar.gz"
	sum := sha256.Sum256(tarballContent)
	actualDigest := hex.EncodeToString(sum[:])
	releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": %q, "digest": "sha256:%s"}
		]
	}`, tag, assetName, actualDigest)

	for _, tc := range []struct {
		name string
		run  func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", runFreeBSDInstaller},
		{"Linux", runLinuxInstaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				operation:   "update",
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
				existingBin: existingPayload,
				failRestart: true,
			})
			if res.exitCode == 0 {
				t.Fatalf("expected install to fail when restart fails, got exit 0 (out:\n%s)", res.stdout)
			}
			if res.finalBin != existingPayload {
				t.Errorf("failed restart did not restore previous binary: got %q, want %q (out:\n%s)", res.finalBin, existingPayload, res.stdout)
			}
			combined := res.stdout + "\n" + res.stderr
			if !strings.Contains(combined, "Restored") && !strings.Contains(combined, "restored") {
				t.Errorf("expected message indicating rollback/restoration of previous binary:\n%s", combined)
			}
		})
	}
}

// D1. The provider digest comes from the release metadata. When that metadata
// cannot be fetched, nothing can be verified, so the install must abort before
// the tarball is extracted. The old behaviour warned and installed anyway, so
// anyone who could block the API call could defeat verification.
func TestInstallerReleaseMetadataUnavailableAborts(t *testing.T) {
	tarballContent := []byte("unverifiable-tarball-bytes")
	tag := "v2026.10.27"

	for _, tc := range []struct {
		name string
		run  func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", runFreeBSDInstaller},
		{"Linux", runLinuxInstaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:           tag,
				releaseJSON:   "",
				tarballData:   tarballContent,
				metadataFails: true,
			})
			combinedOut := res.stdout + "\n" + res.stderr
			if res.exitCode == 0 {
				t.Fatalf("metadata fetch failed but install succeeded; it must fail closed (out:\n%s)", combinedOut)
			}
			if len(res.tarCalls) != 0 {
				t.Fatalf("tar was invoked (%v) with no release metadata; must abort before extraction", res.tarCalls)
			}
			if !strings.Contains(combinedOut, "Refusing to install an unverified") {
				t.Errorf("expected a message saying the unverified install was refused:\n%s", combinedOut)
			}
			if strings.Contains(combinedOut, "continuing without verification") {
				t.Errorf("installer warned and continued unverified:\n%s", combinedOut)
			}
		})
	}
}

// D1. Metadata that loads but does not list the provider asset cannot verify
// the tarball either, so the install aborts. This is the case where the
// release is present and the asset is genuinely missing.
func TestInstallerProviderAssetMissingFromReleaseAborts(t *testing.T) {
	tarballContent := []byte("tarball-with-no-listed-asset")
	tag := "v2026.10.27"
	releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": "urnetwork-provider-other.tar.gz", "digest": "sha256:%s"}
		]
	}`, tag, strings.Repeat("e", 64))

	for _, tc := range []struct {
		name string
		run  func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", runFreeBSDInstaller},
		{"Linux", runLinuxInstaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				releaseJSON: releaseJSON,
				tarballData: tarballContent,
			})
			combinedOut := res.stdout + "\n" + res.stderr
			if res.exitCode == 0 {
				t.Fatalf("provider asset is not listed but install succeeded (out:\n%s)", combinedOut)
			}
			if len(res.tarCalls) != 0 {
				t.Fatalf("tar was invoked (%v) for an unlisted asset; must abort before extraction", res.tarCalls)
			}
			if !strings.Contains(combinedOut, "does not list") {
				t.Errorf("expected a message saying the release does not list the asset:\n%s", combinedOut)
			}
		})
	}
}

// D2. A failed restart must roll back AND restart the service on the restored
// binary. The old rollback only restarted when the service was still running
// after the failure, which a failed restart never is, so the box was left
// running the old binary with the service stopped.
func TestFreeBSDRollbackRestartsRestoredBinary(t *testing.T) {
	existingPayload := "pre-existing-working-binary"
	tarballContent := []byte("new-tarball-payload")
	tag := "v2026.10.27"
	assetName := "urnetwork-provider-" + tag + ".tar.gz"
	sum := sha256.Sum256(tarballContent)
	releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": %q, "digest": "sha256:%s"}
		]
	}`, tag, assetName, hex.EncodeToString(sum[:]))

	t.Run("restored binary restarts", func(t *testing.T) {
		res := runFreeBSDInstaller(t, installerHarnessOpts{
			tag:              tag,
			operation:        "update",
			releaseJSON:      releaseJSON,
			tarballData:      tarballContent,
			existingBin:      existingPayload,
			restartFailFirst: true,
		})
		combined := res.stdout + "\n" + res.stderr
		if res.exitCode == 0 {
			t.Fatalf("the new binary failed to start, so the install must exit non-zero (out:\n%s)", combined)
		}
		if res.finalBin != existingPayload {
			t.Errorf("rollback did not restore the previous binary: got %q (out:\n%s)", res.finalBin, combined)
		}
		want := []string{
			"restart #1 live=extracted-new-binary",
			"restart #2 live=" + existingPayload,
		}
		if strings.Join(res.svcCalls, "\n") != strings.Join(want, "\n") {
			t.Errorf("service calls = %q, want the restart to run again on the restored binary %q\n(out:\n%s)", res.svcCalls, want, combined)
		}
		if !strings.Contains(combined, "restarted") {
			t.Errorf("expected the operator to be told the service was restarted on the previous binary:\n%s", combined)
		}
	})

	t.Run("restored binary restart also fails", func(t *testing.T) {
		res := runFreeBSDInstaller(t, installerHarnessOpts{
			tag:         tag,
			operation:   "update",
			releaseJSON: releaseJSON,
			tarballData: tarballContent,
			existingBin: existingPayload,
			failRestart: true,
		})
		combined := res.stdout + "\n" + res.stderr
		if res.exitCode == 0 {
			t.Fatalf("expected a non-zero exit when the service cannot start (out:\n%s)", combined)
		}
		if len(res.svcCalls) != 2 {
			t.Errorf("expected a restart on the new and on the restored binary, got %q", res.svcCalls)
		}
		if !strings.Contains(combined, "did NOT restart") {
			t.Errorf("a failed restart of the restored binary must be reported explicitly:\n%s", combined)
		}
	})

	t.Run("stopped service is not started by rollback", func(t *testing.T) {
		// A service that was stopped before the install has no restart to
		// undo: the failed load must roll back without starting it.
		res := runFreeBSDInstaller(t, installerHarnessOpts{
			tag:            tag,
			operation:      "update",
			releaseJSON:    releaseJSON,
			tarballData:    tarballContent,
			existingBin:    existingPayload,
			failRestart:    true,
			serviceStopped: true,
		})
		if len(res.svcCalls) != 1 {
			t.Errorf("a service that was stopped must not be started during rollback; calls = %q", res.svcCalls)
		}
		if res.finalBin != existingPayload {
			t.Errorf("rollback did not restore the previous binary: got %q", res.finalBin)
		}
	})
}

// D3. The version file must describe the binary that is running. Writing it
// before the service answers left it naming the failed release after the
// rollback restored the old binary. Both installers must keep it on the old
// version when the new one does not start.
func TestInstallerVersionFileTracksRunningBinary(t *testing.T) {
	existingPayload := "pre-existing-working-binary"
	tarballContent := []byte("new-tarball-payload")
	oldVersion := "v2026.10.20"
	// Epoch seconds, the form get_release_date_from_api_response writes.
	oldDate := "1792454400" // 2026-10-20T00:00:00Z
	newDate := "1793059200" // 2026-10-27T00:00:00Z
	tag := "v2026.10.27"
	assetName := "urnetwork-provider-" + tag + ".tar.gz"
	sum := sha256.Sum256(tarballContent)
	releaseJSON := fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": %q, "digest": "sha256:%s"}
		]
	}`, tag, assetName, hex.EncodeToString(sum[:]))

	for _, tc := range []struct {
		name        string
		run         func(*testing.T, installerHarnessOpts) installerHarnessResult
		versionFile string
		// dateFile is empty for an installer that keeps no date file.
		dateFile string
	}{
		{"FreeBSD", runFreeBSDInstaller, "version", ""},
		{"Linux", runLinuxInstaller, ".version", ".date"},
	} {
		t.Run(tc.name+"/restart fails", func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:             tag,
				operation:       "update",
				releaseJSON:     releaseJSON,
				tarballData:     tarballContent,
				existingBin:     existingPayload,
				existingVersion: oldVersion,
				existingDate:    oldDate,
				failRestart:     true,
			})
			if res.exitCode == 0 {
				t.Fatalf("expected the failed restart to fail the install (out:\n%s)", res.stdout)
			}
			got, err := os.ReadFile(filepath.Join(res.installDir, tc.versionFile))
			if err != nil {
				t.Fatalf("version file missing: %v", err)
			}
			if strings.TrimSpace(string(got)) != oldVersion {
				t.Errorf("after rollback the version file says %q, want %q: it describes the failed release, not the binary that runs",
					strings.TrimSpace(string(got)), oldVersion)
			}
			if tc.dateFile == "" {
				return
			}
			// .date is written after .version, so a write placed before the
			// service start would pass the version check above and leave the
			// failed release's date behind.
			gotDate, err := os.ReadFile(filepath.Join(res.installDir, tc.dateFile))
			if err != nil {
				t.Fatalf("date file missing: %v", err)
			}
			if strings.TrimSpace(string(gotDate)) != oldDate {
				t.Errorf("after rollback the date file says %q, want %q: it describes the failed release, not the binary that runs",
					strings.TrimSpace(string(gotDate)), oldDate)
			}
		})
		t.Run(tc.name+"/restart succeeds", func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:             tag,
				operation:       "update",
				releaseJSON:     releaseJSON,
				tarballData:     tarballContent,
				existingBin:     existingPayload,
				existingVersion: oldVersion,
				existingDate:    oldDate,
			})
			if res.exitCode != 0 {
				t.Fatalf("expected a clean update, got exit %d (out:\n%s)", res.exitCode, res.stdout)
			}
			got, err := os.ReadFile(filepath.Join(res.installDir, tc.versionFile))
			if err != nil {
				t.Fatalf("version file missing: %v", err)
			}
			if strings.TrimSpace(string(got)) != tag {
				t.Errorf("after a successful update the version file says %q, want %q", strings.TrimSpace(string(got)), tag)
			}
			if tc.dateFile == "" {
				return
			}
			gotDate, err := os.ReadFile(filepath.Join(res.installDir, tc.dateFile))
			if err != nil {
				t.Fatalf("date file missing: %v", err)
			}
			if strings.TrimSpace(string(gotDate)) != newDate {
				t.Errorf("after a successful update the date file says %q, want %q", strings.TrimSpace(string(gotDate)), newDate)
			}
		})
	}
}

// installerRollbackRelease is a release whose provider digest matches tarball,
// for the Linux rollback tests below.
func installerRollbackRelease(tag string, tarball []byte) string {
	sum := sha256.Sum256(tarball)
	return fmt.Sprintf(`{
		"tag_name": %q,
		"published_at": "2026-10-27T00:00:00Z",
		"assets": [
			{"name": %q, "digest": "sha256:%s"}
		]
	}`, tag, "urnetwork-provider-"+tag+".tar.gz", hex.EncodeToString(sum[:]))
}

// The systemd rollback restores the previous binary and starts it. The
// operator must be told which outcome happened, as the FreeBSD and OpenRC paths
// tell them.
func TestLinuxRollbackReportsRestartedRestoredBinary(t *testing.T) {
	existingPayload := "pre-existing-working-binary"
	tarball := []byte("new-tarball-payload")
	tag := "v2026.10.27"
	res := runLinuxInstaller(t, installerHarnessOpts{
		tag:              tag,
		operation:        "update",
		releaseJSON:      installerRollbackRelease(tag, tarball),
		tarballData:      tarball,
		existingBin:      existingPayload,
		restartFailFirst: true,
	})
	if res.exitCode == 0 {
		t.Fatalf("a failed start of the new binary must fail the install (out:\n%s)", res.stdout)
	}
	if res.finalBin != existingPayload {
		t.Errorf("rollback did not restore the previous binary: got %q (out:\n%s)", res.finalBin, res.stdout)
	}
	if len(res.svcCalls) != 2 || !strings.HasSuffix(res.svcCalls[1], "live="+existingPayload) {
		t.Errorf("service calls %q: want a start on the new binary, then one on the restored binary %q", res.svcCalls, existingPayload)
	}
	if !strings.Contains(res.stdout, "Restored the previous provider binary") {
		t.Errorf("expected the restore to be reported (out:\n%s)", res.stdout)
	}
	if !strings.Contains(res.stdout, "Restarted the service on the previous binary") {
		t.Errorf("expected the restarted restored binary to be reported (out:\n%s)", res.stdout)
	}
	if strings.Contains(res.stdout, "did NOT restart") {
		t.Errorf("a restored binary that started must not be reported as a failed restart (out:\n%s)", res.stdout)
	}
}

// When the restored binary does not start either, the operator must be told
// plainly that the service is down. Reporting a rollback as a success hides it.
func TestLinuxRollbackReportsFailedRestoredRestart(t *testing.T) {
	existingPayload := "pre-existing-working-binary"
	tarball := []byte("new-tarball-payload")
	tag := "v2026.10.27"
	res := runLinuxInstaller(t, installerHarnessOpts{
		tag:         tag,
		operation:   "update",
		releaseJSON: installerRollbackRelease(tag, tarball),
		tarballData: tarball,
		existingBin: existingPayload,
		failRestart: true,
	})
	if res.exitCode == 0 {
		t.Fatalf("a failed restart must fail the install (out:\n%s)", res.stdout)
	}
	if len(res.svcCalls) != 2 {
		t.Errorf("service calls %q: want the restored binary started after the failed start", res.svcCalls)
	}
	if !strings.Contains(res.stdout, "did NOT restart on the previous binary") {
		t.Errorf("a restored binary that did not start must be reported as a failed restart (out:\n%s)", res.stdout)
	}
	if strings.Contains(res.stdout, "Restarted the service on the previous binary") {
		t.Errorf("reported a successful restart of the previous binary that did not start (out:\n%s)", res.stdout)
	}
}

// If the previous binary cannot be moved back, there is nothing to restart.
// The installer must say so and must not try to start the new binary again.
func TestLinuxRollbackRestoreFailureSkipsRestart(t *testing.T) {
	existingPayload := "pre-existing-working-binary"
	tarball := []byte("new-tarball-payload")
	tag := "v2026.10.27"
	res := runLinuxInstaller(t, installerHarnessOpts{
		tag:              tag,
		operation:        "update",
		releaseJSON:      installerRollbackRelease(tag, tarball),
		tarballData:      tarball,
		existingBin:      existingPayload,
		restartFailFirst: true,
		failRestore:      true,
	})
	if res.exitCode == 0 {
		t.Fatalf("a failed restore must fail the install (out:\n%s)", res.stdout)
	}
	if len(res.svcCalls) != 1 {
		t.Errorf("service calls %q: the service must not be restarted when the previous binary was not restored", res.svcCalls)
	}
	if !strings.Contains(res.stdout, "Could not restore the previous provider binary") {
		t.Errorf("a failed restore must be reported (out:\n%s)", res.stdout)
	}
	if strings.Contains(res.stdout, "Restored the previous provider binary") || strings.Contains(res.stdout, "Restarted the service") {
		t.Errorf("reported a restore or restart that did not happen (out:\n%s)", res.stdout)
	}
}

// The rollback copy is the only way back to the previous binary. When it cannot
// be created, even through the fallback copies, the swap must not run: a
// replacement with no backup leaves nothing to restore if the new binary fails.
func TestInstallerBackupFailureAbortsSwap(t *testing.T) {
	existingPayload := "pre-existing-working-binary"
	tarball := []byte("new-tarball-payload")
	tag := "v2026.10.27"
	for _, tc := range []struct {
		name string
		run  func(*testing.T, installerHarnessOpts) installerHarnessResult
	}{
		{"FreeBSD", runFreeBSDInstaller},
		{"Linux", runLinuxInstaller},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.run(t, installerHarnessOpts{
				tag:         tag,
				operation:   "update",
				releaseJSON: installerRollbackRelease(tag, tarball),
				tarballData: tarball,
				existingBin: existingPayload,
				failBackup:  true,
			})
			// The binary is checked first: the Linux harness can reach a second
			// flow that exits 0 on an up-to-date version, which hides the swap.
			if res.finalBin != existingPayload {
				t.Errorf("swap proceeded without a rollback copy: live binary is %q, want %q (out:\n%s)", res.finalBin, existingPayload, res.stdout)
			}
			if res.exitCode == 0 {
				t.Errorf("a failed backup must abort the install (out:\n%s)", res.stdout)
			}
			if !strings.Contains(res.stdout, "Could not back up") {
				t.Errorf("a failed backup must be reported (out:\n%s)", res.stdout)
			}
		})
	}
}
