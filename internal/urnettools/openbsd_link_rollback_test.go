package urnettools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenBSDToolLinkFailureRestoresProvider(t *testing.T) {
	skipWithoutPOSIXShell(t)
	data, err := os.ReadFile("../../scripts/Provider_Install_OpenBSD.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	start := strings.Index(script, "    if [ ! -e /usr/local/bin/urnet-tools ]")
	if start < 0 {
		t.Fatal("cannot locate tool link block")
	}
	end := strings.Index(script[start:], "    # The install tree stays ROOT-OWNED")
	if end < 0 {
		t.Fatal("cannot locate end of tool link block")
	}
	block := script[start : start+end]
	// Force the link branch without accessing the real /usr/local/bin.
	block = strings.ReplaceAll(block, "/usr/local/bin/urnet-tools", "$link")
	for _, old := range []bool{true, false} {
		t.Run(map[bool]string{true: "update", false: "fresh"}[old], func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "provider")
			backup := filepath.Join(dir, "backup")
			if err := os.WriteFile(bin, []byte("new"), 0755); err != nil {
				t.Fatal(err)
			}
			if old {
				if err := os.WriteFile(backup, []byte("old"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			setup := `priv() { if [ "$1" = ln ]; then return 1; fi; "$@"; }
pr_err() { :; }
pr_warn() { :; }
`
			cmd := exec.Command("/bin/sh", "-c", setup+block)
			hadOld := "0"
			if old {
				hadOld = "1"
			}
			cmd.Env = append(os.Environ(), "link="+filepath.Join(dir, "link"), "install_path="+dir, "tmpdir="+filepath.Join(dir, "temp"), "provider_bin="+bin, "provider_backup="+backup, "had_old_binary="+hadOld)
			if err := cmd.Run(); err == nil {
				t.Fatal("expected failure")
			}
			got, err := os.ReadFile(bin)
			if old && (err != nil || string(got) != "old") {
				t.Fatalf("old binary not restored: %q, %v", got, err)
			}
			if !old && !os.IsNotExist(err) {
				t.Fatalf("new binary remains: %q, %v", got, err)
			}
		})
	}
}
