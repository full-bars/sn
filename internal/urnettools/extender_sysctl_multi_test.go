package urnettools

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeSysctlInfo struct{}

func (fakeSysctlInfo) Name() string       { return "99-urnetwork-extender.conf" }
func (fakeSysctlInfo) Size() int64        { return 0 }
func (fakeSysctlInfo) Mode() os.FileMode  { return 0o644 }
func (fakeSysctlInfo) ModTime() time.Time { return time.Time{} }
func (fakeSysctlInfo) IsDir() bool        { return false }
func (fakeSysctlInfo) Sys() any           { return nil }

// The sysctl route is host-wide, so several providers share one write. A
// second apply would back up the drop-in setup itself just wrote, and the
// printed recovery would then restore setup's own file instead of the
// original.
func TestExtenderSetupSysctlRouteAppliesOnceForSeveralProviders(t *testing.T) {
	oldDiscover := setupDiscoverFn
	setupDiscoverFn = func() []Provider {
		return []Provider{
			{Unit: "a.service", StateDir: "/state/a", User: "alice", Binary: "/opt/a/miner"},
			{Unit: "b.service", StateDir: "/state/b", User: "bob", Binary: "/opt/b/miner"},
			{Unit: "c.service", StateDir: "/state/c", User: "root", Binary: "/opt/c/miner"},
		}
	}
	defer func() { setupDiscoverFn = oldDiscover }()

	for _, tc := range []struct {
		name         string
		existing     bool
		wantBackups  int
		wantRecovery string
	}{
		{name: "no prior drop-in", wantRecovery: "sudo rm " + extenderSysctlDropin + " && sudo sysctl -w " + extenderSysctlKey + "=1024"},
		{name: "prior drop-in kept once", existing: true, wantBackups: 1, wantRecovery: "sudo mv " + extenderSysctlBackup + " " + extenderSysctlDropin + " && sudo sysctl -w " + extenderSysctlKey + "=1024"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			live := "1024\n"
			dropin, backup := tc.existing, false
			writes, backups, applies, setcaps := 0, 0, 0, 0
			execFake := func(name string, args ...string) ([]byte, error) {
				switch name {
				case "ss":
					return []byte("State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n"), nil
				case "cp":
					backups++
					backup = true
				case "sysctl":
					if len(args) == 1 && args[0] == "--system" {
						applies++
						live = "443\n"
					}
				case "setcap":
					setcaps++
				case "getcap":
					return nil, nil
				case "sudo":
					return []byte("extender-bind-probe: proven\n"), nil
				}
				return nil, nil
			}
			cfg := &SystemPlatformConfigurer{
				PlatformName: "linux",
				ExecCmd:      execFake,
				ReadFile: func(path string) ([]byte, error) {
					if path == extenderSysctlPortStartPath {
						return []byte(live), nil
					}
					return nil, os.ErrNotExist
				},
				WriteFile: func(name string, data []byte, perm os.FileMode) error {
					if name == extenderSysctlDropin {
						writes++
						dropin = true
					}
					return nil
				},
				Lstat: func(name string) (os.FileInfo, error) {
					if (name == extenderSysctlDropin && dropin) || (name == extenderSysctlBackup && backup) {
						return fakeSysctlInfo{}, nil
					}
					return nil, os.ErrNotExist
				},
				LookupPath: func(string) (string, error) { return "/usr/bin/sudo", nil },
			}
			err := RunExtenderSetup(ExtenderSetupOptions{
				Platform:    "linux",
				Route:       "sysctl",
				Yes:         true,
				IsRoot:      func() bool { return true },
				Stdout:      &out,
				Stderr:      &errOut,
				PortChecker: &PortChecker{Platform: "linux", ExecCmd: execFake},
				PlatformCfg: cfg,
				FirewallMgr: &FirewallManager{Platform: "linux", LookPath: func(string) (string, error) { return "", os.ErrNotExist }, ExecCmd: execFake},
			})
			if err != nil {
				t.Fatalf("RunExtenderSetup failed: %v\n%s", err, out.String())
			}
			if writes != 1 || backups != tc.wantBackups || applies != 1 {
				t.Fatalf("writes=%d backups=%d (want %d) applies=%d, want one write and one apply", writes, backups, tc.wantBackups, applies)
			}
			if setcaps != 0 {
				t.Fatalf("the sysctl route must not run setcap, ran %d", setcaps)
			}
			if got := strings.Count(out.String(), "Recovery command: "+tc.wantRecovery); got != 1 {
				t.Fatalf("want the recovery command once (%q), got %d:\n%s", tc.wantRecovery, got, out.String())
			}
			if strings.Count(out.String(), "applied sysctl drop-in") != 1 {
				t.Fatalf("the route must be reported applied exactly once:\n%s", out.String())
			}
		})
	}
}

// Root providers need no low-port route: with a route chosen for another
// provider they are neither planned nor configured.
func TestExtenderRouteLeavesRootProviderAlone(t *testing.T) {
	for _, route := range []string{"sysctl", "capability", "skip"} {
		cfg := &SystemPlatformConfigurer{PlatformName: "linux", Route: route}
		ok, desc, err := cfg.CheckRequirement("/opt/c/miner", "root")
		if err != nil || !ok || !strings.Contains(desc, "root") {
			t.Fatalf("%s: check ok=%v desc=%q err=%v", route, ok, desc, err)
		}
		plan, err := cfg.PlanRequirement("/opt/c/miner", "root")
		if err != nil || len(plan) != 0 {
			t.Fatalf("%s: plan=%v err=%v", route, plan, err)
		}
		applied, _, err := cfg.ApplyRequirement("/opt/c/miner", "root")
		if err != nil || applied {
			t.Fatalf("%s: applied=%v err=%v", route, applied, err)
		}
	}
}
