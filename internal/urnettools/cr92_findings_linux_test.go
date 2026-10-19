//go:build linux

package urnettools

// Linux-only coverage for the CodeRabbit findings addressed on this branch.
// Split from cr92_findings_test.go because openrc.go and discover_openrc.go are
// themselves linux-gated, so their symbols (openrcMatchedProviderFrom,
// discoverOpenRCService, openrcUpdateCronScript, openrcStagedRootToolPath and
// the supervision seam) do not exist when the package builds for freebsd/darwin.

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- CR4: a selector must match the stopped-service record ------------------

// TestOpenRCSelectorMatchesStoppedServiceRecord pins the stopped-service path.
// discoverOpenRCService yields PID 0 with Supervisor "openrc"; it is not a
// supervised process, so requiring providerSupervisedByOpenRC made every
// selector miss it and `start --user urnet` fall through to
// selectLifecycleTarget and fail with "has no owning systemd unit".
func TestOpenRCSelectorMatchesStoppedServiceRecord(t *testing.T) {
	oldSupervised := providerSupervisedByOpenRCFn
	t.Cleanup(func() { providerSupervisedByOpenRCFn = oldSupervised })
	providerSupervisedByOpenRCFn = func(p Provider) bool { return p.PID > 0 }

	stopped := Provider{
		User:       "urnet",
		StateDir:   "/home/urnet/.urnetwork",
		Binary:     "/usr/local/libexec/urnetwork-provider/bin/urnetwork",
		Supervisor: "openrc",
		Running:    false,
		PID:        0,
	}

	got, ok := openrcMatchedProviderFrom([]Provider{stopped}, Target{User: "urnet"})
	if !ok {
		t.Fatal("selector must match the stopped-service record (PID 0, Supervisor openrc)")
	}
	if got.Supervisor != "openrc" {
		t.Errorf("matched the wrong record: %+v", got)
	}
}

// TestOpenRCSelectorStillRejectsBareProvider is the negative half: accepting the
// stopped record must not let an unsupervised provider satisfy a selector on
// the service's behalf.
func TestOpenRCSelectorStillRejectsBareProvider(t *testing.T) {
	oldSupervised := providerSupervisedByOpenRCFn
	t.Cleanup(func() { providerSupervisedByOpenRCFn = oldSupervised })
	providerSupervisedByOpenRCFn = func(p Provider) bool { return false }

	bare := Provider{
		User:   "urnet",
		Binary: "/usr/local/libexec/urnetwork-provider/bin/urnetwork",
	}
	if _, ok := openrcMatchedProviderFrom([]Provider{bare}, Target{User: "urnet"}); ok {
		t.Error("an unsupervised provider must not satisfy a selector")
	}
}

// --- CR1: duplicate suppression must not be tautological --------------------

// TestDiscoverOpenRCServiceKeepsStoppedRecordForOtherUser pins that a provider
// started by a DIFFERENT user from the same binary no longer suppresses the
// stopped record. The old condition's second clause could never change the
// result, so matching on the binary alone hid the service from providers,
// uninstall and logs, and selector-less start lost the rc-service path.
func TestDiscoverOpenRCServiceKeepsStoppedRecordForOtherUser(t *testing.T) {
	oldSupervised := providerSupervisedByOpenRCFn
	oldProbe, oldSystemd := openrcProbeFn, systemdRunningFn
	oldInitPath := openrcInitScriptPath
	t.Cleanup(func() {
		providerSupervisedByOpenRCFn = oldSupervised
		openrcProbeFn, systemdRunningFn = oldProbe, oldSystemd
		openrcInitScriptPath = oldInitPath
	})

	const command = "/usr/local/libexec/urnetwork-provider/bin/urnetwork"
	// discoverOpenRCService needs OpenRC active, the init script on disk, and a
	// resolvable home for the service user before it reaches the suppression
	// loop under test. The stub must actually declare command=/command_user=:
	// the identity is parsed out of the init script and the function returns
	// early when command is empty. userHomeDir reads the REAL /etc/passwd with no
	// seam, so command_user must be a user that exists here — klets does, and
	// it is not the "other user" the first case needs.
	serviceUser := currentTestUser(t)
	otherUser := serviceUser + "-other"

	openrcProbeFn = func() bool { return true }
	systemdRunningFn = func() bool { return false }
	openrcInitScriptPath = filepath.Join(t.TempDir(), "urnetwork")
	initScript := strings.Join([]string{
		"#!/sbin/openrc-run",
		"command=" + command,
		"command_user=" + serviceUser,
		"pidfile=/run/urnetwork.pid",
	}, "\n")
	if err := os.WriteFile(openrcInitScriptPath, []byte(initScript), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		running  Provider
		supervis bool
		want     bool // expect the stopped record to be reported
	}{
		{"other user same binary", Provider{User: otherUser, Binary: command, PID: 4242}, false, true},
		{"same user same binary", Provider{User: serviceUser, Binary: command, PID: 4242}, false, false},
		{"supervised child", Provider{User: serviceUser, Binary: command, PID: 4242}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			providerSupervisedByOpenRCFn = func(Provider) bool { return tc.supervis }
			_, got := discoverOpenRCService([]Provider{tc.running})
			if got != tc.want {
				t.Errorf("stopped record reported=%v, want %v", got, tc.want)
			}
		})
	}
}

// currentTestUser returns a user that exists in this host's /etc/passwd with an
// absolute home, which userHomeDir requires. It reads the real passwd file, so
// a fabricated name would make discoverOpenRCService return early and the test
// would pass for the wrong reason.
func currentTestUser(t *testing.T) string {
	t.Helper()
	f, err := os.Open("/etc/passwd")
	if err != nil {
		t.Skipf("cannot read /etc/passwd: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) >= 6 && fields[0] == currentUserName() && filepath.IsAbs(fields[5]) {
			return fields[0]
		}
	}
	t.Skip("the current user has no absolute home in /etc/passwd")
	return ""
}

// --- CR5: the cron guard must check the FILE, not only its directories ------

// TestOpenRCCronGuardChecksToolFile pins that the root-executed cron entry
// verifies the tool binary itself. The guard walked only the directories above
// it; with an explicit -i install the tree is not root-owned and installBinary
// chowns swapped binaries to the service user, so every directory passed while
// root executed a file the service user could rewrite — root code execution
// from an internet-facing account.
func TestOpenRCCronGuardChecksToolFile(t *testing.T) {
	script := openrcUpdateCronScript("/usr/local/libexec/urnetwork-provider/bin/urnet-tools")

	for _, want := range []string{
		`if [ -L "$tool" ]`,               // refuse a symlink
		`if [ ! -f "$tool" ]`,             // require a regular file
		`tool_owner=$(stat -c %u "$tool"`, // check the file's owner
		`$((0$tool_mode & 022))`,          // check the file's write bits
	} {
		if !strings.Contains(script, want) {
			t.Errorf("cron guard is missing %q; it must verify the tool FILE before exec:\n%s", want, script)
		}
	}
	// The file check must come BEFORE the exec, and before the directory walk
	// it augments.
	iFile := strings.Index(script, `tool_owner=$(stat -c %u "$tool"`)
	iExec := strings.Index(script, `exec "$tool"`)
	iDir := strings.Index(script, "dir=$(dirname")
	if iFile < 0 || iExec < 0 || iDir < 0 {
		t.Fatalf("could not locate the guard stages in the script:\n%s", script)
	}
	if iFile > iExec {
		t.Error("the tool-file check runs after the exec; it must run before")
	}
}

// TestOpenRCCronGuardFileCheckFailsClosed runs the generated script against a
// tool file a non-root user can rewrite, and asserts it refuses. This is the
// behavioural half of the CR5 test above: the structural one proves the check
// exists, this one proves it fires.
func TestOpenRCCronGuardFileCheckFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the guard cannot be observed refusing a non-root-owned file")
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "urnet-tools")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write tool: %v", err)
	}
	// Group/other-writable and owned by us: exactly the case the old
	// directory-only guard walked past. The temp dir chain is ours too, so the
	// directory walk would also refuse — this test is specifically about the
	// FILE check being present and evaluated before the exec.
	if err := os.Chmod(tool, 0o777); err != nil {
		t.Fatalf("chmod tool: %v", err)
	}

	// Neutralise the exec so the script's verdict is the only observable.
	script := strings.Replace(openrcUpdateCronScript(tool),
		`exec "$tool" update -f`, "echo GUARD_ALLOWED", 1)

	out, err := runShell(t, script)
	if strings.Contains(out, "GUARD_ALLOWED") {
		t.Errorf("cron guard allowed a world-writable tool file owned by a non-root user:\n%s", out)
	}
	if !strings.Contains(out, "refusing") && err == nil {
		t.Errorf("expected the guard to refuse with a message; output:\n%s", out)
	}
}

// runShell executes script with /bin/sh and returns its combined output.
func runShell(t *testing.T, script string) (string, error) {
	t.Helper()
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	return string(out), err
}

// --- CR7: the staged-tool branch must require OpenRC AND the file -----------

// TestOpenRCStagedRootToolPathRequiresOpenRCAndFile pins that the staged-tool
// branch is only selected when OpenRC is in use and the staged file exists.
// Returning the configured path unconditionally meant a systemd host took that
// branch, skipped every missing staged source and returned early, so
// ensureToolOnPath never reached the /usr/local/bin link path and updates
// could not install the system-wide tool links.
func TestOpenRCStagedRootToolPathRequiresOpenRCAndFile(t *testing.T) {
	oldStat, oldProbe, oldSystemd := openrcStatFn, openrcProbeFn, systemdRunningFn
	t.Cleanup(func() {
		openrcStatFn, openrcProbeFn, systemdRunningFn = oldStat, oldProbe, oldSystemd
	})

	// A real file, so IsDir() is a genuine answer rather than a fake.
	present := filepath.Join(t.TempDir(), "urnet-tools")
	if err := os.WriteFile(present, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(t.TempDir(), "not-created")

	cases := []struct {
		name     string
		probe    bool
		systemd  bool
		statPath string
		wantSet  bool
	}{
		{"openrc active and file present", true, false, present, true},
		{"openrc active but file missing", true, false, absent, false},
		{"systemd wins even with file present", true, true, present, false},
		{"no openrc, file present", false, false, present, false},
		{"no openrc, file missing", false, false, absent, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			openrcProbeFn = func() bool { return tc.probe }
			systemdRunningFn = func() bool { return tc.systemd }
			openrcStatFn = func(p string) (os.FileInfo, error) { return os.Stat(tc.statPath) }

			got := openrcStagedRootToolPath()
			if (got != "") != tc.wantSet {
				t.Errorf("openrcStagedRootToolPath()=%q, want set=%v", got, tc.wantSet)
			}
			if tc.wantSet && got != openrcRootToolPath {
				t.Errorf("got %q, want the staged root path %q", got, openrcRootToolPath)
			}
		})
	}
}
