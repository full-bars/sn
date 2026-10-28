package urnettools

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMacInstallerREADMECommandInstalls(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	const command = "Provider_Install_Mac.sh | sh -s -- install"
	if !strings.Contains(string(b), command) {
		t.Fatalf("macOS install command must pass the install operation to sh")
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
