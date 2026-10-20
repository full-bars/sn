package urnettools

import (
	"runtime"
	"strings"
	"testing"
)

// TestBSDRunningPIDLookupCannotMatchItself pins the process-table fallback in
// the rc.d template, in BOTH copies (the Go renderer and the installer's
// heredoc). `ps -U <service user>` lists the pipeline's own grep when the
// caller is that user, and a `grep -F '<binary> provide'` carries the search
// string in its argv — so the grep matches itself and a stopped provider
// reports as running (`onestatus` green, `start` skips the real start, and the
// shell wrapper's `urnet-tools status` shows the same false green).
//
// The match therefore has to live in awk, on the argv fields, where the program
// text cannot match its own command line.
func TestBSDRunningPIDLookupCannotMatchItself(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the rc.d template is not a Windows artifact")
	}

	scripts := map[string]string{
		"go": renderBSDServiceScript("urnetwork", "/usr/local/bin/urnetwork", "tester", "/home/tester"),
	}
	shScript, err := renderInstallerRcdTemplate(t, "urnetwork", "/usr/local/bin/urnetwork", "tester")
	if err != nil {
		t.Fatalf("render the installer copy: %v", err)
	}
	scripts["installer"] = shScript

	for name, script := range scripts {
		code := codeWithoutComments(script)
		// -u selects the output format on FreeBSD; -U selects by username.
		if strings.Contains(code, "ps -u ") {
			t.Errorf("%s copy: ps -u selects an output format, not a user, so the fallback cannot find the provider", name)
		}
		if !strings.Contains(code, `ps -U "${urnetwork_user}" -o pid=,args=`) {
			t.Errorf("%s copy: the fallback does not select processes by username with ps -U", name)
		}
		// The self-matching shape this test exists to prevent. Comments are
		// dropped first: both copies explain the hazard by naming it.
		if strings.Contains(code, "grep -F") {
			t.Errorf("%s copy: the pid lookup still pipes through grep, which matches its own argv", name)
		}
		const want = `awk -v b='/usr/local/bin/urnetwork' '$2 == b && $3 == "provide" {print $1; exit}'`
		if !strings.Contains(code, want) {
			t.Errorf("%s copy: the pid lookup does not match the binary and the provide subcommand in awk; want\n  %s", name, want)
		}
	}
}

// codeWithoutComments drops whole-line shell comments so a test can assert on
// behaviour without tripping over the commentary that describes it.
func codeWithoutComments(script string) string {
	var b strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// TestBsdRcConfUserArgsScopeToService pins sysrc's -s flag. Without it the
// query reads only the rc_conf_files (/etc/rc.conf, /etc/rc.conf.local) and
// misses a service-specific /etc/rc.conf.d/<service> override that
// load_rc_config honours, so stopped-provider discovery would assign the wrong
// user and, through it, the wrong state directory.
func TestBsdRcConfUserArgsScopeToService(t *testing.T) {
	got := strings.Join(bsdRcConfUserArgs("urnetwork"), " ")
	const want = "-s urnetwork -n urnetwork_user"
	if got != want {
		t.Errorf("sysrc query is %q, want %q; without -s rc.conf.d/<service> is ignored", got, want)
	}
}
