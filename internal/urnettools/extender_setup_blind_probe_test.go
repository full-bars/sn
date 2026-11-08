package urnettools

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// `ss -p` hides root-owned sockets from an unprivileged caller, so a held port
// with no visible holder must not abort setup before it can elevate.
func TestExtenderSetupHiddenPortHolderDefersToElevation(t *testing.T) {
	ssOut := "State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\nLISTEN 0 4096 *:443 *:*\n"
	run := func(root bool) (struct{}, error) {
		cfg := &mockPlatformConfigurer{platform: "linux", configured: true}
		err := RunExtenderSetup(ExtenderSetupOptions{
			Platform: "linux", PlatformCfg: cfg, IsRoot: func() bool { return root },
			Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Force: true,
			LookPath: func(string) (string, error) { return "/usr/bin/sudo", nil },
			PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(name string, args ...string) ([]byte, error) {
				if args[0] == "-tlnp" {
					return []byte(ssOut), nil
				}
				return nil, nil
			}},
			FirewallMgr: &FirewallManager{Platform: "linux", LookPath: func(string) (string, error) { return "", errors.New("missing") }},
		})
		return struct{}{}, err
	}
	// Non-root sees nothing about the holder: must reach elevation.
	// Without a TTY it stops at the "run sudo" instruction, which is the
	// elevation step; the failure it must not produce is a port conflict.
	if _, err := run(false); err == nil || strings.Contains(err.Error(), "port TCP 443 is held") || !strings.Contains(err.Error(), "requires root privileges") {
		t.Fatalf("hidden holder aborted setup before elevation: %v", err)
	}
	// Root sees everything: an unidentified holder is still a conflict.
	if _, err := run(true); err == nil || !strings.Contains(err.Error(), "port TCP 443 is held") {
		t.Fatalf("root run must still report the conflict: %v", err)
	}
}

func TestExtenderFirewallUFWUnreadableIsNotNoFirewall(t *testing.T) {
	fm := &FirewallManager{Platform: "linux",
		LookPath: func(name string) (string, error) {
			if name == "ufw" {
				return name, nil
			}
			return "", errors.New("missing")
		},
		ExecCmd: func(string, ...string) ([]byte, error) {
			return []byte("ERROR: You need to be root to run this script"), errors.New("exit 1")
		}}
	st := fm.Detect()
	if st.Kind != FirewallUFW || !st.Active || st.PortsOpen {
		t.Fatalf("unreadable ufw misclassified: %+v", st)
	}
}

type nnpConfigurer struct {
	mockPlatformConfigurer
	nnp bool
}

func (n *nnpConfigurer) VerifyBind(string, string) (bool, error) { return true, nil }
func (n *nnpConfigurer) UnitNoNewPrivileges(string) (bool, error) {
	return n.nnp, nil
}

// The bind probe runs outside the unit, so "verified" must not stand alone
// when the unit's NoNewPrivileges would make the service ignore the capability.
func TestReportExtenderBindWarnsWhenUnitSetsNoNewPrivileges(t *testing.T) {
	user := Provider{Unit: "urnetwork-provider.service", User: "provider", Binary: "/usr/local/bin/urnetwork"}
	cases := []struct {
		name string
		cfg  *nnpConfigurer
		p    Provider
		warn bool
	}{
		{"non-root with NoNewPrivileges", &nnpConfigurer{nnp: true}, user, true},
		{"non-root without it", &nnpConfigurer{nnp: false}, user, false},
		{"root needs no capability", &nnpConfigurer{nnp: true}, Provider{Unit: user.Unit, User: "root"}, false},
		{"no unit", &nnpConfigurer{nnp: true}, Provider{User: "provider"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := reportExtenderBind(&out, c.cfg, c.p, false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "verified TCP 443 listener") {
				t.Fatalf("missing verified line: %q", out.String())
			}
			if got := strings.Contains(out.String(), "NoNewPrivileges"); got != c.warn {
				t.Fatalf("warned=%v want %v: %q", got, c.warn, out.String())
			}
		})
	}
}

func TestUnitNoNewPrivilegesParsesSystemctl(t *testing.T) {
	for out, want := range map[string]bool{"yes\n": true, "no\n": false, "\n": false} {
		sp := &SystemPlatformConfigurer{PlatformName: "linux", ExecCmd: func(name string, args ...string) ([]byte, error) {
			if name != "systemctl" || args[0] != "show" {
				t.Fatalf("unexpected call %s %v", name, args)
			}
			return []byte(out), nil
		}}
		if got, err := sp.UnitNoNewPrivileges("u.service"); err != nil || got != want {
			t.Fatalf("%q: got %v err %v", out, got, err)
		}
	}
	sp := &SystemPlatformConfigurer{PlatformName: "linux", ExecCmd: func(string, ...string) ([]byte, error) { return nil, errors.New("no systemd") }}
	if _, err := sp.UnitNoNewPrivileges("u.service"); err == nil {
		t.Fatal("systemctl failure must surface")
	}
}

// Windows has no sudo: a non-elevated run must say so and name an elevated
// terminal, not tell the operator to run a command that does not exist there.
func TestExtenderSetupWindowsNonAdminDoesNotSuggestSudo(t *testing.T) {
	var errOut bytes.Buffer
	err := RunExtenderSetup(ExtenderSetupOptions{
		Platform: "windows", PlatformCfg: &mockPlatformConfigurer{platform: "windows"},
		IsRoot: func() bool { return false }, Stdout: &bytes.Buffer{}, Stderr: &errOut,
		LookPath:    func(string) (string, error) { return "C:\\sudo.exe", nil }, // even a sudo on PATH
		PortChecker: &PortChecker{Platform: "windows", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }},
		FirewallMgr: &FirewallManager{Platform: "windows", ExecCmd: func(string, ...string) ([]byte, error) { return nil, errors.New("no rule") }},
	})
	if err == nil {
		t.Fatal("a non-admin Windows run must not proceed")
	}
	for _, text := range []string{err.Error(), errOut.String()} {
		if strings.Contains(text, "sudo") || !strings.Contains(text, "Administrator") || !strings.Contains(text, "extender setup") {
			t.Fatalf("message = %q", text)
		}
	}
}
