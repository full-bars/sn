package urnettools

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestExtenderFirewallWindowsPartialRepair(t *testing.T) {
	for _, missing := range []string{"URnetwork Extender", "URnetwork Extender UDP"} {
		t.Run(missing, func(t *testing.T) {
			rules := map[string]bool{"URnetwork Extender": true, "URnetwork Extender UDP": true}
			rules[missing] = false
			var adds []string
			run := func(_ string, args ...string) ([]byte, error) {
				name := strings.TrimPrefix(args[4], "name=")
				if args[2] == "show" {
					if rules[name] {
						return []byte("Rule Name: " + name), nil
					}
					return nil, errors.New("not found")
				}
				adds = append(adds, name)
				rules[name] = true
				return nil, nil
			}
			fm := &FirewallManager{Platform: "windows", ExecCmd: run}
			if fm.Detect().PortsOpen {
				t.Fatal("partial rules reported open")
			}
			applied, err := fm.Apply(fm.Detect())
			if err != nil || !applied || !reflect.DeepEqual(adds, []string{missing}) {
				t.Fatalf("applied=%v err=%v adds=%v", applied, err, adds)
			}
			if !fm.Detect().PortsOpen {
				t.Fatal("repaired rules not detected")
			}
			sp := &SystemPlatformConfigurer{ExecCmd: run}
			ok, _, err := sp.checkWindows()
			if !ok || err != nil {
				t.Fatalf("platform check: %v %v", ok, err)
			}
		})
	}
}

func TestExtenderBindOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		execErr   error
		want      bool
		failed    bool
	}{
		{"proven", "extender-bind-probe: proven", nil, true, false},
		{"failed", "extender-bind-probe: failed: permission denied", errors.New("exit 1"), false, true},
		{"unverified", "sudo denied", errors.New("exit 1"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := &SystemPlatformConfigurer{PlatformName: "linux", LookupPath: func(string) (string, error) { return "sudo", nil }, ExecCmd: func(name string, args ...string) ([]byte, error) {
				want := []string{"-n", "-u", "provider", "--", "/provider", "--extender-bind-probe"}
				if name != "sudo" || !reflect.DeepEqual(args, want) {
					t.Fatalf("command %s %v", name, args)
				}
				return []byte(tc.out), tc.execErr
			}}
			ok, err := sp.VerifyBind("/provider", "provider")
			if ok != tc.want || (err != nil) != tc.failed {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			var out bytes.Buffer
			err = reportExtenderBind(&out, sp, Provider{Binary: "/provider", User: "provider"}, false)
			if (err != nil) != tc.failed {
				t.Fatalf("report err=%v", err)
			}
			if !tc.failed && !strings.Contains(out.String(), tc.name) && !(tc.want && strings.Contains(out.String(), "verified")) {
				t.Fatalf("output %q", out.String())
			}
		})
	}
}

func setupRegressionOptions() ExtenderSetupOptions {
	return ExtenderSetupOptions{Platform: "linux", Route: "capability", PlatformCfg: &mockPlatformConfigurer{platform: "linux"}, IsRoot: func() bool { return false }, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }}, FirewallMgr: &FirewallManager{Platform: "linux", LookPath: func(string) (string, error) { return "", errors.New("missing") }}}
}
func TestExtenderSetupElevateDecision(t *testing.T) {
	opts := setupRegressionOptions()
	opts.NonInteractive = true
	called := false
	opts.ElevateFunc = func([]string) error { called = true; return nil }
	err := RunExtenderSetup(opts)
	if err == nil || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}
func TestExtenderSetupReexecArgv(t *testing.T) {
	old := stdinIsInteractiveOverride
	stdinIsInteractiveOverride = func() bool { return true }
	defer func() { stdinIsInteractiveOverride = old }()
	opts := setupRegressionOptions()
	opts.Force = true
	opts.LookPath = func(string) (string, error) { return "sudo", nil }
	opts.Argv = []string{"extender", "setup", "--enable", "--force"}
	var got []string
	opts.ElevateFunc = func(args []string) error { got = args; return nil }
	// The elevated run keeps the caller's arguments and carries the chosen route.
	want := []string{"extender", "setup", "--enable", "--force", "--route=capability"}
	if err := RunExtenderSetup(opts); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("argv=%v want=%v err=%v", got, want, err)
	}
}
func TestExtenderSetupTargetResolutionError(t *testing.T) {
	opts := setupRegressionOptions()
	opts.Target = Target{Unit: "nonexistent-extender-regression.service"}
	if err := RunExtenderSetup(opts); err == nil || !strings.Contains(err.Error(), "resolving target") {
		t.Fatalf("err=%v", err)
	}
}
func TestExtenderSetupFirewallApplyFailure(t *testing.T) {
	opts := setupRegressionOptions()
	opts.IsRoot = func() bool { return true }
	opts.Yes = true
	failure := errors.New("denied firewall")
	opts.FirewallMgr = &FirewallManager{Platform: "linux", LookPath: func(name string) (string, error) {
		if name == "firewall-cmd" {
			return name, nil
		}
		return "", errors.New("missing")
	}, ExecCmd: func(_ string, args ...string) ([]byte, error) {
		if args[0] == "--state" {
			return []byte("running"), nil
		}
		if args[0] == "--get-default-zone" {
			return []byte("public"), nil
		}
		for _, arg := range args {
			if strings.HasPrefix(arg, "--query-port=") {
				return []byte("no"), errors.New("not enabled")
			}
		}
		if args[0] == "--permanent" {
			return nil, failure
		}
		return []byte("ok"), nil
	}}
	err := RunExtenderSetup(opts)
	if !errors.Is(err, failure) {
		t.Fatalf("err=%v", err)
	}
}
