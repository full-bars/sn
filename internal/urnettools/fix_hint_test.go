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

// The hint has to be copy-pasteable on its own, and it must never write over
// the running tool: a download that dies halfway would replace a working
// binary with a truncated one.
func TestToolInstallHint(t *testing.T) {
	self := "/home/u/bin/urnet-tools"
	hint := toolInstallHint("full-bars/sn", "v2026.10.26-1793016775", self, "linux", "amd64")
	for _, want := range []string{
		"https://github.com/full-bars/sn/releases/download/v2026.10.26-1793016775/urnet-tools-linux-amd64",
		"-o " + self + ".new",
		"chmod +x " + self + ".new",
		"mv " + self + ".new " + self,
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q:\n%s", want, hint)
		}
	}
	if strings.Contains(hint, "-o "+self+" ") {
		t.Errorf("hint writes straight over the running tool:\n%s", hint)
	}

	// A path that already ends in .exe must not gain a second one, and the
	// move has to wait until the tool exits.
	winSelf := `C:	ools\urnet-tools.exe`
	win := toolInstallHint("full-bars/sn", "v2026.10.26-1793016775", winSelf, "windows", "amd64")
	if strings.Contains(win, ".exe.exe") {
		t.Errorf("windows hint doubles the suffix:\n%s", win)
	}
	for _, want := range []string{winSelf + ".new", "move /Y " + winSelf + ".new " + winSelf, "once this tool has exited"} {
		if !strings.Contains(win, want) {
			t.Errorf("windows hint missing %q:\n%s", want, win)
		}
	}
}
