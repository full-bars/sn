package urnettools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenBSDInstallServicePreservesAccount(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_OpenBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	fn := extractShellFunc(t, string(b), "do_install_service")
	for _, tc := range []struct{ existing, want string }{{"alice", "alice"}, {"", "bob"}} {
		t.Run(tc.want, func(t *testing.T) {
			script := `id() { printf '0\n'; }
resolve_service_user() { printf 'bob\n'; }
write_rc_script() { printf '%s\n' "$1"; }
rcctl() { :; }
pr_info() { :; }
pr_err() { :; }
` + fn + "\nservice_user='" + tc.existing + "'\nrc_script='" + filepath.Join(t.TempDir(), "urnetwork") + "'\nservice_name=urnetwork\ndo_install_service\n"
			out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != tc.want {
				t.Fatalf("got %q, err %v; want %q", out, err, tc.want)
			}
		})
	}
}

func TestOpenBSDFlagStateDirectoryOwnership(t *testing.T) {
	skipWithoutPOSIXShell(t)
	b, err := os.ReadFile("../../scripts/Provider_Install_OpenBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	fn := extractShellFunc(t, string(b), "write_state_flag")
	for _, tc := range []struct {
		uid, user string
		wantChown bool
	}{{"0", "alice", true}, {"0", "root", false}, {"1000", "alice", false}} {
		t.Run(tc.uid+"-"+tc.user, func(t *testing.T) {
			dir := t.TempDir()
			script := `id() { printf '%s\n' "$mock_uid"; }
chown() { printf '%s\n' "$*" > "$log"; }
pr_err() { :; }
` + fn + "\nwrite_state_flag flag on\n"
			cmd := exec.Command("/bin/sh", "-c", script)
			cmd.Env = append(os.Environ(), "mock_uid="+tc.uid, "service_user="+tc.user, "state_dir="+filepath.Join(dir, "state"), "log="+filepath.Join(dir, "chown"))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("write failed: %v: %s", err, out)
			}
			if data, err := os.ReadFile(filepath.Join(dir, "state", "flag")); err != nil || string(data) != "on\n" {
				t.Fatalf("flag %q: %v", data, err)
			}
			_, err = os.Stat(filepath.Join(dir, "chown"))
			if (err == nil) != tc.wantChown {
				t.Fatalf("chown called: %v; want %v", err == nil, tc.wantChown)
			}
		})
	}
}
