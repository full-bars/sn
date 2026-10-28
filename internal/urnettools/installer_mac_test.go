package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMacInstallerREADMECommandInstalls(t *testing.T) {
	// Every page that publishes the piped one-liner must pass the operation,
	// because `curl | sh` alone now exits non-zero without installing.
	const command = "Provider_Install_Mac.sh | sh -s -- install"
	for _, page := range []string{"../../README.md", "../../docs/Installation.md"} {
		b, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		if !strings.Contains(string(b), command) {
			t.Errorf("%s: macOS install command must pass the install operation to sh, want %q", page, command)
		}
	}
}

func TestMacInstallerArgumentHandling(t *testing.T) {
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skipf("requires /bin/sh: %v", err)
	}
	b, err := os.ReadFile("../../scripts/Provider_Install_Mac.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	start := strings.Index(script, `operation="${1:-}"`)
	end := strings.LastIndex(script, "\nesac")
	if start < 0 || end <= start {
		t.Fatal("could not extract the macOS installer's operation dispatch")
	}
	dispatch := script[start : end+len("\nesac")]
	prelude := `
me=Provider_Install_Mac.sh
show_help() { echo help; }
pr_err() { printf '%s\n' "$*" >&2; }
do_install() { echo install-invoked; }
`

	for _, tc := range []struct {
		name       string
		args       []string
		wantStatus int
		wantOutput string
	}{
		{name: "missing operation fails", wantStatus: 1, wantOutput: "Missing command"},
		{name: "install dispatches", args: []string{"install"}, wantOutput: "install-invoked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-c", prelude + dispatch, "mac-installer"}, tc.args...)
			cmd := exec.Command("/bin/sh", args...)
			out, err := cmd.CombinedOutput()
			status := 0
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					status = exitErr.ExitCode()
				} else {
					t.Fatalf("run dispatch: %v", err)
				}
			}
			if status != tc.wantStatus {
				t.Errorf("dispatch exit = %d, want %d; output: %s", status, tc.wantStatus, out)
			}
			if !strings.Contains(string(out), tc.wantOutput) {
				t.Errorf("dispatch output = %q, want it to contain %q", out, tc.wantOutput)
			}
		})
	}
}

func TestMacInstallerRequiresLaunchdBeforeInstallationSuccess(t *testing.T) {
	b, err := os.ReadFile("../../scripts/Provider_Install_Mac.sh")
	if err != nil {
		t.Fatal(err)
	}
	doInstall := extractMacShellFunc(t, strings.ReplaceAll(string(b), "\r\n", "\n"), "do_install")
	loadCheck := strings.Index(doInstall, "if ! install_service")
	successMessage := strings.Index(doInstall, `pr_info "URnetwork provider %s installed"`)
	if loadCheck < 0 || successMessage < loadCheck {
		t.Fatal("do_install must stop on launchd failure before reporting install success")
	}
}

func TestMacLaunchdFailuresAreNotReportedAsSuccess(t *testing.T) {
	for _, tc := range []struct {
		name            string
		operation       string
		guiStatus       int
		serviceStatus   int
		bootstrapStatus int
		bootoutStatus   int
		wantText        string
		wantNoText      string
	}{
		{name: "start bootstrap failure", operation: "start", guiStatus: 0, serviceStatus: 1, bootstrapStatus: 1, wantText: "Next:", wantNoText: "Provider started"},
		{name: "stop failure", operation: "stop", guiStatus: 0, serviceStatus: 0, bootoutStatus: 1, wantText: "Next:", wantNoText: "Provider stopped"},
		{name: "restart bootstrap failure", operation: "restart", guiStatus: 0, serviceStatus: 0, bootstrapStatus: 1, bootoutStatus: 0, wantText: "Next:", wantNoText: "Provider restarted"},
		{name: "stop without a GUI session", operation: "stop", guiStatus: 1, serviceStatus: 0, wantText: "graphical session", wantNoText: "Provider stopped"},
		{name: "install bootstrap failure", operation: "install-service", guiStatus: 0, serviceStatus: 1, bootstrapStatus: 1, wantText: "Next:", wantNoText: "installed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, status := runMacLaunchdHarness(t, tc.operation, tc.guiStatus, tc.serviceStatus, tc.bootstrapStatus, tc.bootoutStatus)
			if status == 0 {
				t.Errorf("operation exited successfully after launchctl failure; output: %s", out)
			}
			if !strings.Contains(out, tc.wantText) {
				t.Errorf("output = %q, want it to contain %q", out, tc.wantText)
			}
			if strings.Contains(out, tc.wantNoText) {
				t.Errorf("output falsely reports success %q: %s", tc.wantNoText, out)
			}
		})
	}
}

func TestMacLaunchdSuccessfulLifecycleReportsSuccess(t *testing.T) {
	for _, tc := range []struct {
		operation string
		service   int
		wantText  string
	}{
		{operation: "start", service: 1, wantText: "Provider started"},
		{operation: "stop", service: 0, wantText: "Provider stopped"},
		{operation: "restart", service: 0, wantText: "Provider restarted"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			out, status := runMacLaunchdHarness(t, tc.operation, 0, tc.service, 0, 0)
			if status != 0 || !strings.Contains(out, tc.wantText) {
				t.Fatalf("successful %s: exit=%d output=%q, want success message %q", tc.operation, status, out, tc.wantText)
			}
		})
	}
}

func runMacLaunchdHarness(t *testing.T, operation string, guiStatus, serviceStatus, bootstrapStatus, bootoutStatus int) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skipf("requires /bin/sh: %v", err)
	}
	b, err := os.ReadFile("../../scripts/Provider_Install_Mac.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(b), "\r\n", "\n")
	functions := []string{"pr_err", "pr_info", "load_service", "unload_service", "restart_service", "service_running", "launchd_domain_error", "install_service", "do_start", "do_stop", "do_restart"}
	var extracted strings.Builder
	for _, name := range functions {
		fn := extractMacShellFunc(t, script, name)
		if fn != "" {
			extracted.WriteString(fn)
			extracted.WriteByte('\n')
		}
	}

	tempDir := t.TempDir()
	binDir := filepath.Join(tempDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	launchctl := `#!/bin/sh
case "$1" in
    print)
        case "$2" in
            gui/*/*) exit "$SERVICE_STATUS" ;;
            gui/*) exit "$GUI_STATUS" ;;
        esac
        exit 2
        ;;
    bootstrap) exit "$BOOTSTRAP_STATUS" ;;
    bootout) exit "$BOOTOUT_STATUS" ;;
esac
exit 2
`
	launchctlPath := filepath.Join(binDir, "launchctl")
	if err := os.WriteFile(launchctlPath, []byte(launchctl), 0o755); err != nil {
		t.Fatal(err)
	}

	prelude := `
me=urnetwork-installer
launchd_domain=gui/501
plist_path=/tmp/com.urnetwork.provider.plist
label=com.urnetwork.provider
install_path=/tmp/urnetwork-provider
FORCE=1
sleep() { :; }
`
	driver := `
case "$1" in
    start) do_start ;;
    stop) do_stop ;;
    restart) do_restart ;;
    install-service) install_service ;;
esac
`
	cmd := exec.Command("/bin/sh", "-c", prelude+extracted.String()+driver, "mac-launchd", operation)
	cmd.Env = []string{
		"PATH=" + binDir + ":/usr/bin:/bin",
		fmt.Sprintf("GUI_STATUS=%d", guiStatus),
		fmt.Sprintf("SERVICE_STATUS=%d", serviceStatus),
		fmt.Sprintf("BOOTSTRAP_STATUS=%d", bootstrapStatus),
		fmt.Sprintf("BOOTOUT_STATUS=%d", bootoutStatus),
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return string(out), exitErr.ExitCode()
	}
	t.Fatalf("run launchd harness: %v", err)
	return string(out), 1
}

func extractMacShellFunc(t *testing.T, script, name string) string {
	t.Helper()
	start := -1
	for _, declaration := range []string{name + "()", name + " ()"} {
		if i := strings.Index(script, declaration); i >= 0 && (start < 0 || i < start) {
			start = i
		}
	}
	if start < 0 {
		return ""
	}
	rest := script[start:]
	lines := strings.Split(rest, "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i] == "}" {
			return strings.Join(lines[:i+1], "\n")
		}
	}
	t.Fatalf("could not extract function %s from the Mac installer", name)
	return ""
}
