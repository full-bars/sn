package urnettools

import (
	"bytes"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
)

// mockPlatformConfigurer records calls and simulates platform checks.
type mockPlatformConfigurer struct {
	platform       string
	configured     bool
	checkDesc      string
	applyCalled    bool
	applyDesc      string
	applyErr       error
	verifyCalled   bool
	verifyBindErr  error
	checkCallCount int
}

func (m *mockPlatformConfigurer) Platform() string {
	return m.platform
}

func (m *mockPlatformConfigurer) CheckRequirement(binary, user string) (bool, string, error) {
	m.checkCallCount++
	return m.configured, m.checkDesc, nil
}

func (m *mockPlatformConfigurer) ApplyRequirement(binary, user string) (bool, string, error) {
	m.applyCalled = true
	if m.applyErr != nil {
		return false, "", m.applyErr
	}
	return true, m.applyDesc, nil
}

func (m *mockPlatformConfigurer) VerifyBind(user string) (bool, error) {
	m.verifyCalled = true
	if m.verifyBindErr != nil {
		return false, m.verifyBindErr
	}
	return true, nil
}

// 1. TCP 443 busy: stops, names the holder, changes nothing, exit non-zero. Never kill anything.
func TestExtenderSetup_TCP443Busy(t *testing.T) {
	checker := &PortChecker{
		Platform: "linux",
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			if name == "ss" && len(args) > 0 && args[0] == "-tlnp" {
				return []byte("LISTEN 0 4096 0.0.0.0:443 0.0.0.0:* users:((\"caddy\",pid=4567,fd=5))\n"), nil
			}
			return nil, nil
		},
	}

	platformCfg := &mockPlatformConfigurer{
		platform:   "linux",
		configured: false,
		applyDesc:  "applied cap_net_bind_service",
	}

	var stdout, stderr bytes.Buffer
	opts := ExtenderSetupOptions{
		Platform:       "linux",
		PortChecker:    checker,
		PlatformCfg:    platformCfg,
		IsRoot:         func() bool { return true },
		Stdout:         &stdout,
		Stderr:         &stderr,
		NonInteractive: true,
	}

	err := RunExtenderSetup(opts)
	if err == nil {
		t.Fatal("expected error when TCP 443 is busy, got nil")
	}

	errStr := err.Error()
	if !strings.Contains(errStr, "caddy") {
		t.Errorf("error = %q; expected to name the process 'caddy'", errStr)
	}
	if !strings.Contains(errStr, "4567") {
		t.Errorf("error = %q; expected to name PID 4567", errStr)
	}
	if !strings.Contains(errStr, "cannot run without it") {
		t.Errorf("error = %q; expected explanation that role cannot run without TCP 443", errStr)
	}

	if platformCfg.applyCalled {
		t.Fatal("ApplyRequirement was called; must change nothing when port 443 is held")
	}
}

// 2. UDP busy only: warns and continues.
func TestExtenderSetup_UDPBusyOnly(t *testing.T) {
	checker := &PortChecker{
		Platform: "linux",
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			if name == "ss" && len(args) > 0 && args[0] == "-tlnp" {
				return []byte("LISTEN 0 4096 127.0.0.1:8080 0.0.0.0:*\n"), nil // TCP 443 free
			}
			if name == "ss" && len(args) > 0 && args[0] == "-ulnp" {
				return []byte("UNCONN 0 0 0.0.0.0:443 0.0.0.0:* users:((\"quic-server\",pid=8910,fd=7))\n" +
					"UNCONN 0 0 0.0.0.0:4053 0.0.0.0:* users:((\"dns-server\",pid=8911,fd=8))\n"), nil
			}
			return nil, nil
		},
	}

	platformCfg := &mockPlatformConfigurer{
		platform:   "linux",
		configured: false,
		applyDesc:  "applied cap_net_bind_service",
	}

	fwMgr := &FirewallManager{
		Platform: "linux",
		LookPath: func(file string) (string, error) { return "", errors.New("not found") },
	}

	var stdout, stderr bytes.Buffer
	opts := ExtenderSetupOptions{
		Platform:       "linux",
		PortChecker:    checker,
		PlatformCfg:    platformCfg,
		FirewallMgr:    fwMgr,
		IsRoot:         func() bool { return true },
		Stdout:         &stdout,
		Stderr:         &stderr,
		NonInteractive: true,
	}

	err := RunExtenderSetup(opts)
	if err != nil {
		t.Fatalf("expected success with UDP busy, got err: %v", err)
	}

	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "warn:") || !strings.Contains(stderrStr, "UDP 443") {
		t.Errorf("stderr = %q; expected warning for UDP 443", stderrStr)
	}
	if !strings.Contains(stderrStr, "UDP 4053") {
		t.Errorf("stderr = %q; expected warning for UDP 4053", stderrStr)
	}

	if !platformCfg.applyCalled {
		t.Fatal("ApplyRequirement was not called; setup must continue when only UDP is busy")
	}
	if !platformCfg.verifyCalled {
		t.Fatal("VerifyBind was not called; setup must verify bind")
	}
}

// 3. Already configured: reports it and makes no change.
func TestExtenderSetup_AlreadyConfigured(t *testing.T) {
	checker := &PortChecker{
		Platform: "linux",
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			return nil, nil
		},
	}

	platformCfg := &mockPlatformConfigurer{
		platform:   "linux",
		configured: true,
		checkDesc:  "cap_net_bind_service=+ep already set",
	}

	fwMgr := &FirewallManager{
		Platform: "linux",
		LookPath: func(file string) (string, error) { return "", errors.New("none") },
	}

	var stdout, stderr bytes.Buffer
	opts := ExtenderSetupOptions{
		Platform:       "linux",
		PortChecker:    checker,
		PlatformCfg:    platformCfg,
		FirewallMgr:    fwMgr,
		IsRoot:         func() bool { return false }, // even non-root should succeed without elevation if already configured
		Stdout:         &stdout,
		Stderr:         &stderr,
		NonInteractive: true,
	}

	err := RunExtenderSetup(opts)
	if err != nil {
		t.Fatalf("unexpected error on already configured host: %v", err)
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, "already configured") {
		t.Errorf("stdout = %q; expected 'already configured' message", outStr)
	}

	if platformCfg.applyCalled {
		t.Fatal("ApplyRequirement was called on already configured host; must make no change")
	}
}

// 4. Non-root or declined sudo: prints the exact command, changes nothing, exit non-zero.
func TestExtenderSetup_NonRootOrDeclinedSudo(t *testing.T) {
	checker := &PortChecker{
		Platform: "linux",
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			return nil, nil
		},
	}

	platformCfg := &mockPlatformConfigurer{
		platform:   "linux",
		configured: false,
	}

	fwMgr := &FirewallManager{
		Platform: "linux",
		LookPath: func(file string) (string, error) { return "", errors.New("none") },
	}

	var stdout, stderr bytes.Buffer
	opts := ExtenderSetupOptions{
		Platform:       "linux",
		PortChecker:    checker,
		PlatformCfg:    platformCfg,
		FirewallMgr:    fwMgr,
		IsRoot:         func() bool { return false },
		LookPath:       func(file string) (string, error) { return "/usr/bin/sudo", nil },
		NonInteractive: true,
		Stdout:         &stdout,
		Stderr:         &stderr,
	}

	err := RunExtenderSetup(opts)
	if err == nil {
		t.Fatal("expected non-zero exit/error for unprivileged non-interactive setup, got nil")
	}

	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "sudo") || !strings.Contains(stderrStr, "extender setup") {
		t.Errorf("stderr = %q; expected to print exact command with sudo", stderrStr)
	}

	if platformCfg.applyCalled {
		t.Fatal("ApplyRequirement was called without root privileges; must change nothing")
	}
}

// 5. Platform dispatch: each platform selects its own path (inject the platform, do not hardcode linux).
func TestExtender_PlatformDispatch(t *testing.T) {
	platforms := []struct {
		goos            string
		expectedCommand string
	}{
		{"linux", "setcap"},
		{"freebsd", "sysctl"},
		{"windows", "netsh"},
		{"darwin", "launchctl"},
	}

	for _, p := range platforms {
		t.Run(p.goos, func(t *testing.T) {
			commandsExecuted := []string{}
			configurer := &SystemPlatformConfigurer{
				PlatformName: p.goos,
				ExecCmd: func(name string, args ...string) ([]byte, error) {
					commandsExecuted = append(commandsExecuted, name)
					if name == "getcap" {
						return []byte("/path/urnetwork cap_net_bind_service=ep"), nil
					}
					if name == "sysctl" {
						return []byte("0"), nil
					}
					if name == "netsh" {
						return []byte("URnetwork Extender"), nil
					}
					return nil, nil
				},
				ReadFile: func(name string) ([]byte, error) {
					return []byte("net.inet.ip.portrange.reservedhigh=0\n"), nil
				},
				WriteFile: func(name string, data []byte, perm os.FileMode) error {
					return nil
				},
				ListenTCP: func(network, address string) (net.Listener, error) {
					return &mockListener{}, nil
				},
				IsRoot: func() bool {
					// FreeBSD tests non-root to verify sysctl invocation
					if p.goos == "freebsd" {
						return false
					}
					return true
				},
			}

			if configurer.Platform() != p.goos {
				t.Errorf("configurer.Platform() = %q, want %q", configurer.Platform(), p.goos)
			}

			// Check requirement dispatch
			configured, _, err := configurer.CheckRequirement("/path/urnetwork", "urnet")
			if err != nil {
				t.Fatalf("CheckRequirement on %s returned err: %v", p.goos, err)
			}
			if !configured {
				t.Errorf("CheckRequirement on %s returned false, want true", p.goos)
			}

			// Verify that the platform's specific tool was queried
			found := false
			for _, cmd := range commandsExecuted {
				if cmd == p.expectedCommand || (p.goos == "linux" && cmd == "getcap") || (p.goos == "darwin" && cmd == "launchctl") {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("platform %s did not invoke %s, invoked: %v", p.goos, p.expectedCommand, commandsExecuted)
			}
		})
	}
}

// 6. Firewall detection: unknown firewall prints the rule rather than claiming success.
func TestExtender_FirewallDetection(t *testing.T) {
	t.Run("UnknownFirewallPrintsRules", func(t *testing.T) {
		fwMgr := &FirewallManager{
			Platform: "linux",
			LookPath: func(file string) (string, error) {
				return "", errors.New("command not found")
			},
			ExecCmd: func(name string, args ...string) ([]byte, error) {
				return nil, errors.New("not available")
			},
		}

		status := fwMgr.Detect()
		if status.Active {
			t.Errorf("expected inactive firewall for unknown, got active=true")
		}
		if status.PortsOpen {
			t.Errorf("expected ports_open=false, got true")
		}
		if len(status.SuggestedRules) == 0 {
			t.Errorf("expected suggested rules to be populated for unknown firewall")
		}

		// Apply should NOT claim success or silently succeed
		applied, err := fwMgr.Apply(status)
		if err != nil {
			t.Errorf("Apply on unknown firewall returned error: %v", err)
		}
		if applied {
			t.Errorf("Apply claimed success for unknown firewall; must not claim success")
		}
	})

	t.Run("FirewalldDetectedWhenActive", func(t *testing.T) {
		fwMgr := &FirewallManager{
			Platform: "linux",
			LookPath: func(file string) (string, error) {
				if file == "firewall-cmd" {
					return "/usr/bin/firewall-cmd", nil
				}
				return "", errors.New("not found")
			},
			ExecCmd: func(name string, args ...string) ([]byte, error) {
				if name == "firewall-cmd" && len(args) > 0 && args[0] == "--state" {
					return []byte("running\n"), nil
				}
				if name == "firewall-cmd" && len(args) > 0 && strings.HasPrefix(args[0], "--query-port=") {
					return []byte("no\n"), nil
				}
				return nil, nil
			},
		}

		status := fwMgr.Detect()
		if status.Kind != FirewallFirewalld {
			t.Errorf("status.Kind = %q, want %q", status.Kind, FirewallFirewalld)
		}
		if !status.Active {
			t.Errorf("expected active firewalld")
		}
		if status.PortsOpen {
			t.Errorf("expected ports_open=false")
		}
	})

	t.Run("UFWDetectedWhenActive", func(t *testing.T) {
		fwMgr := &FirewallManager{
			Platform: "linux",
			LookPath: func(file string) (string, error) {
				if file == "ufw" {
					return "/usr/sbin/ufw", nil
				}
				return "", errors.New("not found")
			},
			ExecCmd: func(name string, args ...string) ([]byte, error) {
				if name == "ufw" && len(args) > 0 && args[0] == "status" {
					return []byte("Status: active\n\nTo                         Action      From\n--                         ------      ----\n22/tcp                     ALLOW       Anywhere\n"), nil
				}
				return nil, nil
			},
		}

		status := fwMgr.Detect()
		if status.Kind != FirewallUFW {
			t.Errorf("status.Kind = %q, want %q", status.Kind, FirewallUFW)
		}
		if !status.Active {
			t.Errorf("expected active ufw")
		}
		if status.PortsOpen {
			t.Errorf("expected ports_open=false (443 not in rule list)")
		}
	})

	t.Run("FreeBSDPFDetectedWhenActive", func(t *testing.T) {
		fwMgr := &FirewallManager{
			Platform: "freebsd",
			LookPath: func(file string) (string, error) {
				if file == "pfctl" {
					return "/sbin/pfctl", nil
				}
				return "", errors.New("not found")
			},
			ExecCmd: func(name string, args ...string) ([]byte, error) {
				if name == "pfctl" && len(args) > 0 && args[0] == "-s" {
					return []byte("Status: Enabled for 2 days\n"), nil
				}
				return nil, nil
			},
		}

		status := fwMgr.Detect()
		if status.Kind != FirewallPF {
			t.Errorf("status.Kind = %q, want %q", status.Kind, FirewallPF)
		}
		if !status.Active {
			t.Errorf("expected active pf")
		}
		if len(status.SuggestedRules) == 0 {
			t.Errorf("expected pf suggested rules")
		}
	})

	t.Run("WindowsAdvFirewallDetected", func(t *testing.T) {
		fwMgr := &FirewallManager{
			Platform: "windows",
			ExecCmd: func(name string, args ...string) ([]byte, error) {
				if name == "netsh" && len(args) > 0 && args[0] == "advfirewall" {
					return []byte("Rule Name:                            URnetwork Extender\nEnabled:                              Yes\nDirection:                            In\nProfiles:                             Domain,Private,Public\nGrouping:                             \nLocalIP:                              Any\nRemoteIP:                             Any\nProtocol:                             TCP\nLocalPort:                            443\n"), nil
				}
				return nil, nil
			},
		}

		status := fwMgr.Detect()
		if status.Kind != FirewallWindowsAdv {
			t.Errorf("status.Kind = %q, want %q", status.Kind, FirewallWindowsAdv)
		}
		if !status.Active {
			t.Errorf("expected active Windows Firewall")
		}
		if !status.PortsOpen {
			t.Errorf("expected ports_open=true when rule exists")
		}
	})
}

type mockListener struct{}

func (m *mockListener) Accept() (net.Conn, error) { return nil, errors.New("closed") }
func (m *mockListener) Close() error              { return nil }
func (m *mockListener) Addr() net.Addr            { return &net.TCPAddr{Port: 443} }
