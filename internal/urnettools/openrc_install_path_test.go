package urnettools

// Execution coverage for the OpenRC install-path validation in the Linux
// installer. The check the reviewer flagged was TEXTUAL, so a path that merely
// began with the root prefix resolved somewhere else entirely and every
// downstream step — the binary copy, and openrc_finalize_root_paths which
// recursively deletes symlinks and chmods — then acted on the resolved
// location. These cases execute the real shell function from the installer.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// resolveParentCases are the lexical-canonicalization inputs and what they must
// produce. `..` at or above the root collapses to the root rather than
// escaping it.
var resolveParentCases = []struct {
	path string
	want string
}{
	{"/usr/local/lib/urnetwork-provider", "/usr/local/lib"},
	{"/usr/local/lib/urnetwork-provider/./bin", "/usr/local/lib/urnetwork-provider"},
	{"/usr/local/lib/urnetwork-provider/sub/../x", "/usr/local/lib/urnetwork-provider"},
	{"/usr/local/lib/urnetwork-provider/a/b", "/usr/local/lib/urnetwork-provider/a"},
	{"/usr/local/lib/urnetwork-provider/a/b/c/../../../..", "/usr/local/lib/urnetwork-provider"},
	{"/usr/local/lib/urnetwork-provider/../../../../etc", "/"},
	{"/usr/local/lib/urnetwork-provider/../../../../../..", "/"},
	{"/x", "/"},
	{"relative/path", "relative"},
}

// TestOpenRCResolveParentCanonicalizesLexically runs the REAL function out of
// the installer. It is lexical by design: the install tree usually does not
// exist yet, and readlink -f on the parent could be steered by a symlink the
// service user owns.
func TestOpenRCResolveParentCanonicalizesLexically(t *testing.T) {
	skipWithoutPOSIXShell(t)
	fn := extractShellFunc(t, readInstaller(t), "openrc_resolve_parent")
	if fn == "" {
		t.Fatal("could not extract openrc_resolve_parent from the Linux installer")
	}

	for _, tc := range resolveParentCases {
		t.Run(tc.path, func(t *testing.T) {
			out, err := runShellScript(t, fn+"\nopenrc_resolve_parent '"+tc.path+"'")
			if err != nil {
				t.Fatalf("openrc_resolve_parent failed: %v (%s)", err, out)
			}
			if got := strings.TrimSpace(out); got != tc.want {
				t.Errorf("openrc_resolve_parent(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestOpenRCInstallPathRejectsTraversal is the security assertion: a path that
// passes the installer's textual prefix test but resolves outside the
// root-owned tree must be refused. Without the resolved-path check,
// `.../urnetwork-provider/../../../../etc` was accepted and openrc_finalize_root_paths
// would then recursively chmod and unlink under /etc.
func TestOpenRCInstallPathRejectsTraversal(t *testing.T) {
	skipWithoutPOSIXShell(t)
	source := readInstaller(t)
	fn := extractShellFunc(t, source, "openrc_resolve_parent")
	if fn == "" {
		t.Fatal("could not extract openrc_resolve_parent from the Linux installer")
	}

	const root = "/usr/local/lib/urnetwork-provider"

	// Reuse the REAL case statement from the installer rather than restating
	// it. A test that re-implements the check it is verifying proves nothing:
	// with the installer's own validation neutered, a local copy would still
	// reject the traversal and the test would pass.
	start := strings.Index(source, "openrc_resolved_parent=")
	if start < 0 {
		t.Fatal("could not find the resolved-path validation in the installer")
	}
	// The validation runs in a subshell assignment followed by a case; take
	// through to the closing esac.
	rest := source[start:]
	endIdx := strings.Index(rest, "\n    esac\n")
	if endIdx < 0 {
		t.Fatal("could not find the end of the resolved-path validation")
	}
	validation := rest[:endIdx+len("\n    esac\n")]

	for _, tc := range []struct {
		path string
		want string // "" means the installer must refuse it
	}{
		// Traversal that escapes the tree — the reported attack. The installer
		// exits non-zero on these, so the resolved value never reaches stdout.
		{root + "/../../../../etc", ""},
		{root + "/../evil", ""},
		{root + "/../../../../../../etc/shadow", ""},
		{root + "/..", ""},
		{root + "/sub/../..", ""},
		// Legitimate paths must keep working.
		{root, root},
		{root + "/sub", root + "/sub"},
		{root + "/sub/.", root + "/sub"},
		{root + "/bin/urnetwork", root + "/bin/urnetwork"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			out, err := runShellScript(t, fn+`
pr_err() { printf '%s\n' "$*" >&2; }
openrc_install_root="`+root+`"
install_path="`+tc.path+`"
`+validation+`
printf 'RESOLVED=%s\n' "$openrc_resolved"
`)
			got := strings.TrimSpace(out)
			if tc.want == "" {
				if err == nil {
					t.Errorf("install path %q resolved to %q and was ACCEPTED; it escapes the root-owned tree",
						tc.path, strings.TrimPrefix(got, "RESOLVED="))
				}
				return
			}
			if err != nil {
				t.Errorf("install path %q was refused but must be accepted: %v (%s)", tc.path, err, out)
			}
			if want := "RESOLVED=" + tc.want; got != want {
				t.Errorf("install path %q: %q, want %q", tc.path, got, want)
			}
		})
	}
}

// TestOpenRCInstallTreeValidationRunsBeforeWrites pins the ORDER, which is the
// rest of the finding: the check has to happen before the installer copies a
// binary or hands the path to openrc_finalize_root_paths. Checking the whole
// file would pass even if the validation were deleted from the flow, because
// the function is still defined somewhere in the file.
func TestOpenRCInstallTreeValidationRunsBeforeWrites(t *testing.T) {
	source := readInstaller(t)
	// Extract by the DEFINITION, not by name: a comment above another
	// function mentions install_openrc_units, and the shared extractor's
	// earliest-match rule would then slice the wrong span.
	body := extractShellFunc(t, source, "install_openrc_units")
	if body == "" {
		t.Fatal("could not extract install_openrc_units from the Linux installer")
	}
	if !strings.HasPrefix(body, "install_openrc_units") {
		t.Fatal("extracted the wrong span for install_openrc_units: " +
			strings.SplitN(body, "\n", 2)[0])
	}

	resolvedIdx := strings.Index(body, "openrc_resolve_parent")
	if resolvedIdx < 0 {
		t.Fatal("install_openrc_units never validates the RESOLVED install path; " +
			"a textual prefix test accepts /usr/local/lib/urnetwork-provider/../../../../etc")
	}
	// The point of the finding is that validation happens BEFORE any write to
	// the tree. Compare CALLS, not occurrences: the explanatory comment above
	// the check names openrc_finalize_root_paths, so a naive substring search
	// finds it before the validation and reports a false failure. A call is
	// the name followed by whitespace, not inside a comment line.
	lines := strings.Split(body, "\n")
	resolvedLine, finalizeLine := -1, -1
	for i, l := range lines {
		code := strings.TrimSpace(l)
		if strings.HasPrefix(code, "#") {
			continue
		}
		if resolvedLine < 0 && strings.Contains(code, "openrc_resolve_parent") {
			resolvedLine = i
		}
		if strings.Contains(code, "openrc_finalize_root_paths") {
			if finalizeLine < 0 {
				finalizeLine = i
			}
		}
	}
	if finalizeLine >= 0 && resolvedLine >= 0 && finalizeLine < resolvedLine {
		t.Errorf("openrc_finalize_root_paths is called on line %d, before the resolved-path "+
			"validation on line %d; it recursively unlinks symlinks and chmods, so a "+
			"traversing path would rewrite arbitrary host files",
			finalizeLine+1, resolvedLine+1)
	}
}

// runShellScriptWithPath runs script with stubDir prepended to PATH, so a
// `command -v <name>` guard inside the installer finds the stub.
func runShellScriptWithPath(t *testing.T, stubDir, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=" + stubDir + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// readInstaller returns the Linux installer's source with CRLF normalized.
func readInstaller(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../scripts/Provider_Install_Linux.sh")
	if err != nil {
		t.Fatalf("read the Linux installer: %v", err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// runShellScript executes script with /bin/sh and returns its combined output.
func runShellScript(t *testing.T, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
