package urnettools

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOpenBSDProxyWrapperRoutesSummary(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_OpenBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	fn := extractShellFunc(t, string(b), "do_proxy")
	for _, tc := range []struct{ args, want string }{
		{"summary --network test", "summary\n--network\ntest\n"},
		{"refresh --force", "proxy\nrefresh\n--force\n"},
	} {
		t.Run(tc.args, func(t *testing.T) {
			dir := t.TempDir()
			tool := filepath.Join(dir, "tool")
			if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("/bin/sh", "-c", "require_tool() { :; }\n"+fn+"\ndo_proxy "+tc.args)
			cmd.Env = append(os.Environ(), "tool_bin="+tool)
			out, err := cmd.CombinedOutput()
			if err != nil || string(out) != tc.want {
				t.Fatalf("output %q, err %v, want %q", out, err, tc.want)
			}
		})
	}
}
