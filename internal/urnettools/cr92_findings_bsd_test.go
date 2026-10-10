//go:build freebsd

package urnettools

// FreeBSD-only coverage for the CodeRabbit findings addressed on this branch.
// Split from cr92_findings_test.go because lifecycle_freebsd.go is itself
// freebsd-gated, so its symbols (bsdServiceRunFn, bsdRequireRcScript,
// renderBSDStatus, setAutoStart) do not exist when the package builds for
// linux/darwin — the shared file must not reference them.

import (
	"os"
	"strings"
	"testing"
)

// --- CR2: rc.d actions must be the one* variants -----------------------------

// TestBsdServiceControlUsesOneshotActions pins that lifecycle control does not
// depend on <name>_enable. The generated rc.d script sets rcvar but neither
// procname nor command, so rc_pid is always empty and rc.subr refuses
// start/stop/restart/status unless the rc.conf flag is YES — printing
// "Cannot 'start' <name>" and exiting 0. The old code then reported success
// while nothing had started.
func TestBsdServiceControlUsesOneshotActions(t *testing.T) {
	oldRun := bsdServiceRunFn
	t.Cleanup(func() { bsdServiceRunFn = oldRun })

	var got []string
	bsdServiceRunFn = func(name string, args ...string) ([]byte, error) {
		got = append([]string{name}, args...)
		return nil, nil
	}

	p := Provider{Binary: "/usr/local/libexec/urnetwork-provider/bin/urnetwork"}
	service := bsdRcServiceName(p.Binary)
	if err := stubRcScriptForTest(t, service); err != nil {
		t.Skipf("cannot stub an rc.d script for %s: %v", service, err)
	}
	for _, action := range []string{"start", "stop", "restart"} {
		got = nil
		if err := bsdServiceControl(p, action); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if len(got) < 2 {
			t.Fatalf("%s: no command captured: %v", action, got)
		}
		if last := got[len(got)-1]; last != "one"+action {
			t.Errorf("%s ran %q, want %q (plain %q is gated on %s_enable=YES)",
				action, last, "one"+action, action, service)
		}
	}
}

// stubRcScriptForTest points bsdRequireRcScript at a real temporary rc.d file
// so bsdServiceControl passes its existence check without a live FreeBSD host.
func stubRcScriptForTest(t *testing.T, service string) error {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/" + service
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		return err
	}
	old := bsdRcScriptExists
	bsdRcScriptExists = func(string) bool { return true }
	t.Cleanup(func() { bsdRcScriptExists = old })
	return nil
}

// TestRenderBSDStatusUsesOnestatus pins the status path the same way: plain
// `status` is rc.conf-gated and exits 0 silently when the flag is not YES,
// which reads as a clean status for a service that is not running.
func TestRenderBSDStatusUsesOnestatus(t *testing.T) {
	src := readFuncSource(t, "lifecycle_freebsd.go", "func renderBSDStatus(")
	if !strings.Contains(src, `"onestatus"`) {
		t.Errorf("renderBSDStatus must call service with onestatus, got:\n%s", src)
	}
	if strings.Contains(src, `"status"`) {
		t.Errorf("renderBSDStatus still passes plain status:\n%s", src)
	}
}

// --- CR3: setAutoStart must escalate through sudo ---------------------------

// TestSetAutoStartEscalatesLikeServiceControl pins that auto-start uses the
// same root/sudo choice as lifecycle control. sysrc writes the root-owned
// /etc/rc.conf and the installer installs the provider as a normal user, so
// without escalation `auto-start on|off` failed with a permission error from
// exactly the account it was installed for.
func TestSetAutoStartEscalatesLikeServiceControl(t *testing.T) {
	src := readFuncSource(t, "lifecycle_freebsd.go", "func setAutoStart(")
	for _, want := range []string{
		"os.Geteuid() == 0",
		`exec.LookPath("sudo")`,
		`"sudo", []string{"sysrc"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("setAutoStart is missing %q:\n%s", want, src)
		}
	}
}

// readFuncSource returns the text of a function body, read from disk. Used for
// the two fixes whose commands are built inline with exec.Command rather than
// behind a seam, so the assertion is on what is actually invoked.
func readFuncSource(t *testing.T, file, sig string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	s := string(b)
	i := strings.Index(s, sig)
	if i < 0 {
		t.Fatalf("%s not found in %s", sig, file)
	}
	rest := s[i:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j]
	}
	return rest
}
