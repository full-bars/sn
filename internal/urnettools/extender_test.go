package urnettools

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfoundation/sn/internal/tui"
	"go.etcd.io/bbolt"
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

// 7. Status report JSON and human-readable format verification.
func TestExtender_StatusFormatting(t *testing.T) {
	report := ExtenderStatusReport{
		Platform:       "linux",
		CanBindTCP443:  true,
		RolePreference: "on",
		Bound:          true,
		Activated:      true,
		Healthy:        true,
		Ports: ExtenderPorts{
			TCP443:  PortHolder{Port: 443, Proto: "tcp", Held: true, Process: "urnetwork", PID: 1234},
			UDP443:  PortHolder{Port: 443, Proto: "udp", Held: false},
			UDP4053: PortHolder{Port: 4053, Proto: "udp", Held: false},
		},
		Firewall: &FirewallStatus{
			Kind:      FirewallFirewalld,
			Active:    true,
			PortsOpen: true,
			Message:   "firewalld active",
		},
	}

	// Test JSON encoding
	var jsonBuf bytes.Buffer
	if err := FormatExtenderStatus(&jsonBuf, report, true); err != nil {
		t.Fatalf("FormatExtenderStatus(json) error: %v", err)
	}

	var decoded ExtenderStatusReport
	if err := json.Unmarshal(jsonBuf.Bytes(), &decoded); err != nil {
		t.Fatalf("Unmarshal decoded json error: %v", err)
	}
	if decoded.Platform != "linux" || !decoded.CanBindTCP443 || !decoded.Bound || !decoded.Activated {
		t.Errorf("decoded JSON mismatch: %+v", decoded)
	}

	// Test text formatting
	var textBuf bytes.Buffer
	if err := FormatExtenderStatus(&textBuf, report, false); err != nil {
		t.Fatalf("FormatExtenderStatus(text) error: %v", err)
	}
	textStr := textBuf.String()
	if !strings.Contains(textStr, "Platform:") || !strings.Contains(textStr, "Can bind TCP 443:  yes") {
		t.Errorf("text output missing expected headers: %s", textStr)
	}
	if !strings.Contains(textStr, "TCP 443:   held by urnetwork (PID 1234)") {
		t.Errorf("text output missing TCP 443 holder: %s", textStr)
	}
}

type mockListener struct{}

func (m *mockListener) Accept() (net.Conn, error) { return nil, errors.New("closed") }
func (m *mockListener) Close() error              { return nil }
func (m *mockListener) Addr() net.Addr            { return &net.TCPAddr{Port: 443} }

// 8. Source-order fallback: Socket first, then Persisted State, then events.log, then Unknown.
func TestExtender_SourceFallbackOrder(t *testing.T) {
	origSocket, origState, origLog := socketExtenderFn, stateExtenderFn, logExtenderFn
	t.Cleanup(func() {
		socketExtenderFn, stateExtenderFn, logExtenderFn = origSocket, origState, origLog
	})

	dummyProvider := Provider{User: "user", StateDir: "/tmp/dummy", Running: true}

	// 1. Socket answers -> source must be "control socket"
	socketExtenderFn = func(p Provider) (*ExtenderReading, error) {
		return &ExtenderReading{
			Source:        SourceControlSocket,
			Preference:    "on",
			RoleState:     "active",
			Bound:         true,
			CarriersBound: "tcp:443",
			ActivatedV4:   true,
			Ipv4:          "198.51.100.1",
			LastRefusal:   "none",
			NextAttempt:   "immediate",
		}, nil
	}
	stateExtenderFn = func(p Provider) (*ExtenderReading, error) {
		t.Fatal("stateExtenderFn should not be called when socket answers")
		return nil, nil
	}
	logExtenderFn = func(p Provider) (*ExtenderReading, error) {
		t.Fatal("logExtenderFn should not be called when socket answers")
		return nil, nil
	}

	reading := ReadExtenderStatus(dummyProvider)
	if reading.Source != SourceControlSocket {
		t.Fatalf("expected source %q, got %q", SourceControlSocket, reading.Source)
	}
	var buf bytes.Buffer
	formatExtenderBlock(&buf, reading)
	out := buf.String()
	if !strings.Contains(out, "Source:            control socket") || !strings.Contains(out, "198.51.100.1") {
		t.Fatalf("expected socket output to be named in block, got:\n%s", out)
	}

	// 2. Socket returns nothing -> falls back to "persisted state"
	socketExtenderFn = func(p Provider) (*ExtenderReading, error) {
		return nil, errors.New("socket offline")
	}
	stateExtenderFn = func(p Provider) (*ExtenderReading, error) {
		return &ExtenderReading{
			Source:        SourcePersistedState,
			Preference:    "on",
			RoleState:     "stopped",
			Bound:         false,
			CarriersBound: "none",
			LastRefusal:   "2026-11-03 12:00:00 (1 refusals)",
			NextAttempt:   "immediate (backoff elapsed)",
		}, nil
	}
	logExtenderFn = func(p Provider) (*ExtenderReading, error) {
		t.Fatal("logExtenderFn should not be called when state answers")
		return nil, nil
	}

	reading = ReadExtenderStatus(dummyProvider)
	if reading.Source != SourcePersistedState {
		t.Fatalf("expected source %q, got %q", SourcePersistedState, reading.Source)
	}
	buf.Reset()
	formatExtenderBlock(&buf, reading)
	out = buf.String()
	if !strings.Contains(out, "Source:            persisted state") || !strings.Contains(out, "stopped") {
		t.Fatalf("expected persisted state to be named in block, got:\n%s", out)
	}

	// 3. Socket and state return nothing -> falls back to "events.log"
	stateExtenderFn = func(p Provider) (*ExtenderReading, error) {
		return nil, errors.New("state unavailable")
	}
	logExtenderFn = func(p Provider) (*ExtenderReading, error) {
		return &ExtenderReading{
			Source:        SourceLog,
			Preference:    "on",
			RoleState:     "historical (events.log)",
			Bound:         false,
			CarriersBound: "none",
			Ipv4:          "203.0.113.1",
			LastRefusal:   "v4: rate limit",
			NextAttempt:   "unknown",
		}, nil
	}

	reading = ReadExtenderStatus(dummyProvider)
	if reading.Source != SourceLog {
		t.Fatalf("expected source %q, got %q", SourceLog, reading.Source)
	}
	buf.Reset()
	formatExtenderBlock(&buf, reading)
	out = buf.String()
	if !strings.Contains(out, "Source:            events.log") || !strings.Contains(out, "historical (events.log)") {
		t.Fatalf("expected events.log to be named in block, got:\n%s", out)
	}

	// 4. All three disabled -> explicit "unknown" output (never blank)
	logExtenderFn = func(p Provider) (*ExtenderReading, error) {
		return nil, errors.New("log empty")
	}

	reading = ReadExtenderStatus(dummyProvider)
	if reading.Source != SourceUnknown {
		t.Fatalf("expected source %q, got %q", SourceUnknown, reading.Source)
	}
	buf.Reset()
	formatExtenderBlock(&buf, reading)
	out = buf.String()
	if !strings.Contains(out, "Source:            unknown") || !strings.Contains(out, "Role state:        unknown") {
		t.Fatalf("expected unknown source and role state in block, got:\n%s", out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("fallback output must not be blank when all three sources are disabled")
	}
}

// 9. Verify top renders a non-empty Extender block and handles all fallbacks including unknown.
func TestExtender_TopRenderingNonEmptyBlock(t *testing.T) {
	origSocket, origState, origLog := socketExtenderFn, stateExtenderFn, logExtenderFn
	t.Cleanup(func() {
		socketExtenderFn, stateExtenderFn, logExtenderFn = origSocket, origState, origLog
	})

	clock := &fakeClock{t: topBase}
	prov := Provider{User: "u", Network: "tornado", StateDir: "/state/tornado", Running: true}

	// 1. Socket source in top
	socketExtenderFn = func(p Provider) (*ExtenderReading, error) {
		return &ExtenderReading{
			Source:        SourceControlSocket,
			Preference:    "on",
			RoleState:     "active",
			Bound:         true,
			CarriersBound: "tcp:443",
			ActivatedV4:   true,
			Ipv4:          "198.51.100.1",
		}, nil
	}
	stateExtenderFn = func(p Provider) (*ExtenderReading, error) { return nil, errors.New("offline") }
	logExtenderFn = func(p Provider) (*ExtenderReading, error) { return nil, errors.New("empty") }

	m := newTestModel(tui.DefaultTheme(), []Provider{prov}, clock)
	m.showExtender = true
	m.refreshExtender()

	b := tui.New(120, 40)
	m.render(b)
	rendered := b.String()
	if !strings.Contains(rendered, "Extender") || !strings.Contains(rendered, "control socket") || !strings.Contains(rendered, "active") {
		t.Fatalf("top should render non-empty extender block from socket, got:\n%s", rendered)
	}

	// 2. Socket returns nothing -> falls back to persisted state in top
	socketExtenderFn = func(p Provider) (*ExtenderReading, error) { return nil, errors.New("socket offline") }
	stateExtenderFn = func(p Provider) (*ExtenderReading, error) {
		return &ExtenderReading{
			Source:      SourcePersistedState,
			Preference:  "on",
			RoleState:   "stopped",
			LastRefusal: "2026-11-03 10:00:00",
			NextAttempt: "immediate",
		}, nil
	}

	m = newTestModel(tui.DefaultTheme(), []Provider{prov}, clock)
	m.showExtender = true
	m.refreshExtender()

	b = tui.New(120, 40)
	m.render(b)
	rendered = b.String()
	if !strings.Contains(rendered, "Extender") || !strings.Contains(rendered, "persisted state") {
		t.Fatalf("top should render non-empty extender block from persisted state, got:\n%s", rendered)
	}

	// 3. Socket and state return nothing -> falls back to events.log in top
	stateExtenderFn = func(p Provider) (*ExtenderReading, error) { return nil, errors.New("no state") }
	logExtenderFn = func(p Provider) (*ExtenderReading, error) {
		return &ExtenderReading{
			Source:      SourceLog,
			Preference:  "on",
			RoleState:   "historical (events.log)",
			LastRefusal: "refused",
		}, nil
	}

	m = newTestModel(tui.DefaultTheme(), []Provider{prov}, clock)
	m.showExtender = true
	m.refreshExtender()

	b = tui.New(120, 40)
	m.render(b)
	rendered = b.String()
	if !strings.Contains(rendered, "Extender") || !strings.Contains(rendered, "events.log") {
		t.Fatalf("top should render non-empty extender block from log, got:\n%s", rendered)
	}

	// 4. All three disabled -> explicit "unknown" output instead of blank screen
	logExtenderFn = func(p Provider) (*ExtenderReading, error) { return nil, errors.New("no log") }

	m = newTestModel(tui.DefaultTheme(), []Provider{prov}, clock)
	m.showExtender = true
	m.refreshExtender()

	b = tui.New(120, 40)
	m.render(b)
	rendered = b.String()
	if !strings.Contains(rendered, "Extender") || !strings.Contains(rendered, "unknown") {
		t.Fatalf("top must render explicit unknown extender block instead of blank screen, got:\n%s", rendered)
	}
}

// 10. Integration test with real files on disk.
func TestExtender_RealFilesFallbackIntegration(t *testing.T) {
	dir := t.TempDir()
	p := Provider{StateDir: dir, Running: true}

	// 1. With empty dir, all three disabled -> unknown
	reading := ReadExtenderStatus(p)
	if reading.Source != SourceUnknown {
		t.Fatalf("expected unknown source for empty dir, got %q", reading.Source)
	}

	// 2. Add events.log -> source becomes events.log
	logData := `{"domain":"extender","event":"activation_failed","cause":"v4: rate limit","extender":{"listening":false,"activated_v4":false,"activated_v6":false,"activation_refused":true,"last_activation_error":"v4: rate limit","ipv4":"198.51.100.5"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "events.log"), []byte(logData), 0600); err != nil {
		t.Fatal(err)
	}
	reading = ReadExtenderStatus(p)
	if reading.Source != SourceLog {
		t.Fatalf("expected SourceLog, got %q", reading.Source)
	}
	if reading.Ipv4 != "198.51.100.5" {
		t.Fatalf("expected ipv4 198.51.100.5, got %q", reading.Ipv4)
	}

	// 3. Add provider_state.db (bbolt) -> state takes precedence over events.log
	dbPath := filepath.Join(dir, "provider_state.db")
	db, err := bbolt.Open(dbPath, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("state"))
		if err != nil {
			return err
		}
		rec := `{"last_refusal_unix":1700000000,"backoff_until_unix":1700000600,"refusals":1}`
		return b.Put([]byte("extender_activation"), []byte(rec))
	})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	reading = ReadExtenderStatus(p)
	if reading.Source != SourcePersistedState {
		t.Fatalf("expected SourcePersistedState over log, got %q", reading.Source)
	}
	if !strings.Contains(reading.LastRefusal, "1 refusals") {
		t.Fatalf("expected refusal detail in persisted state reading, got %q", reading.LastRefusal)
	}
}
