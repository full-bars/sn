package urnettools

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestFreeBSDSetupPrintOnly(t *testing.T) {
	old := setupDiscoverFn
	setupDiscoverFn = func() []Provider { return nil }
	defer func() { setupDiscoverFn = old }()
	for _, root := range []bool{false, true} {
		for _, dry := range []bool{false, true} {
			var out bytes.Buffer
			calls := 0
			runner := func(string, ...string) ([]byte, error) { calls++; return nil, nil }
			cfg := &SystemPlatformConfigurer{PlatformName: "freebsd", ExecCmd: runner, ReadFile: func(string) ([]byte, error) { calls++; return nil, nil }, WriteFile: func(string, []byte, os.FileMode) error { calls++; return nil }}
			err := RunExtenderSetup(ExtenderSetupOptions{Platform: "freebsd", Stdout: &out, Stderr: &out, PlatformCfg: cfg, PortChecker: &PortChecker{Platform: "freebsd", ExecCmd: runner}, FirewallMgr: &FirewallManager{Platform: "freebsd", ExecCmd: runner, LookPath: func(s string) (string, error) { calls++; return s, nil }}, IsRoot: func() bool { return root }, DryRun: dry, ElevateFunc: func([]string) error { calls++; return nil }})
			if err != nil || calls != 0 {
				t.Errorf("root=%v dry=%v err=%v calls=%d", root, dry, err, calls)
			}
			for _, want := range []string{"untested this release", "previous=$(sysctl -n net.inet.ip.portrange.reservedhigh)", "sudo sysctl net.inet.ip.portrange.reservedhigh=0", "sudo sysctl net.inet.ip.portrange.reservedhigh=\"$previous\"", "Undo", "UDP 53"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("missing %q", want)
				}
			}
		}
	}
}

func TestFreeBSDApplyPrintOnly(t *testing.T) {
	calls := 0
	cfg := &SystemPlatformConfigurer{PlatformName: "freebsd", ExecCmd: func(string, ...string) ([]byte, error) { calls++; return nil, nil }, ReadFile: func(string) ([]byte, error) { calls++; return nil, nil }, WriteFile: func(string, []byte, os.FileMode) error { calls++; return nil }}
	applied, _, err := cfg.ApplyRequirement("", "provider")
	if err != nil || applied || calls != 0 {
		t.Fatalf("applied=%v err=%v calls=%d", applied, err, calls)
	}
}
