package urnettools

import (
	"strings"
	"testing"
)

func TestReleaseRepoForProvider(t *testing.T) {
	cases := []struct {
		name    string
		module  string
		version string
		want    string
		ok      bool
	}{
		{"sn", "github.com/urfoundation/sn", "v2026.10.26-1793016775", "full-bars/sn", true},
		{"3.23-fix", "github.com/urnetwork/connect", "v3.23.0-fix.34.3", "full-bars/urnetwork-3.23-fix", true},
		{"meso-miner", "github.com/urnetwork/connect", "v2026.10.24-1080497440-meso", "full-bars/meso-miner", true},
		{"unknown", "github.com/someone/else", "v1.0.0", "", false},
	}
	for _, c := range cases {
		got, ok := releaseRepoForProvider(c.module, c.version)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: releaseRepoForProvider(%q, %q) = (%q, %v), want (%q, %v)",
				c.name, c.module, c.version, got, ok, c.want, c.ok)
		}
	}
}

// The hint has to be copy-pasteable on its own: repo, tag, asset and the path
// the shell already resolves, all in one line.
func TestToolInstallHint(t *testing.T) {
	hint := toolInstallHint("full-bars/sn", "v2026.10.26-1793016775", "/home/u/bin/urnet-tools", "linux", "amd64")
	for _, want := range []string{
		"https://github.com/full-bars/sn/releases/download/v2026.10.26-1793016775/urnet-tools-linux-amd64",
		"/home/u/bin/urnet-tools",
		"chmod +x",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q:\n%s", want, hint)
		}
	}
	win := toolInstallHint("full-bars/sn", "v2026.10.26-1793016775", `C:\tools\urnet-tools`, "windows", "amd64")
	if !strings.Contains(win, ".exe") || !strings.Contains(win, "cannot be replaced") {
		t.Errorf("windows hint must stage and warn, got:\n%s", win)
	}
}
