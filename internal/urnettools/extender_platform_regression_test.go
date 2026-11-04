package urnettools

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestExtenderFreeBSDProviderUserNotSetupCaller(t *testing.T) {
	calls := 0
	sp := &SystemPlatformConfigurer{
		IsRoot:  func() bool { return true },
		ExecCmd: func(string, ...string) ([]byte, error) { calls++; return []byte("1023"), nil },
	}
	configured, _, err := sp.checkFreeBSD("provider")
	if err != nil || configured || calls != 1 {
		t.Fatalf("configured=%v err=%v calls=%d", configured, err, calls)
	}
	configured, _, err = sp.checkFreeBSD("root")
	if err != nil || !configured || calls != 1 {
		t.Fatalf("root: configured=%v err=%v calls=%d", configured, err, calls)
	}
}

func TestExtenderFreeBSDSysctlReadFailureNeverWrites(t *testing.T) {
	failure := errors.New("permission denied")
	sp := &SystemPlatformConfigurer{
		ReadFile:  func(string) ([]byte, error) { return nil, failure },
		WriteFile: func(string, []byte, os.FileMode) error { t.Fatal("unexpected write"); return nil },
		ExecCmd:   func(string, ...string) ([]byte, error) { t.Fatal("unexpected sysctl"); return nil, nil },
	}
	_, _, err := sp.applyFreeBSD("provider")
	if !errors.Is(err, failure) {
		t.Fatalf("expected read failure, got %v", err)
	}
}

func TestExtenderPortInspectionFailure(t *testing.T) {
	for _, platform := range []string{"linux", "windows", "freebsd", "darwin"} {
		for _, failAt := range []int{1, 2} {
			t.Run(platform+string(rune('0'+failAt)), func(t *testing.T) {
				calls := 0
				pc := &PortChecker{Platform: platform, ExecCmd: func(string, ...string) ([]byte, error) {
					calls++
					if calls == failAt {
						return nil, errors.New("inspection failed")
					}
					return nil, nil
				}}
				_, err := pc.CheckPorts()
				if err == nil || !strings.Contains(err.Error(), "inspection failed") {
					t.Fatalf("got %v", err)
				}
			})
		}
	}
}

func TestExtenderMacOSRefusesUnverifiedDaemon(t *testing.T) {
	sp := &SystemPlatformConfigurer{PlatformName: "darwin"}
	for _, operation := range []func() (bool, string, error){sp.checkDarwin, sp.applyDarwin} {
		ok, _, err := operation()
		if ok || err == nil || !strings.Contains(err.Error(), "LaunchDaemon") {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
	}
}

func TestExtenderSetupUnknownFirewallIncomplete(t *testing.T) {
	var output bytes.Buffer
	cfg := &mockPlatformConfigurer{platform: "linux"}
	err := RunExtenderSetup(ExtenderSetupOptions{
		Platform: "linux", PlatformCfg: cfg, IsRoot: func() bool { return true }, Stdout: &output,
		PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }},
		FirewallMgr: &FirewallManager{Platform: "linux", LookPath: func(string) (string, error) { return "", errors.New("missing") }},
	})
	if err == nil || !strings.Contains(err.Error(), "firewall configuration unverified") || strings.Contains(output.String(), "setup complete") {
		t.Fatalf("err=%v output=%q", err, output.String())
	}
	if !strings.Contains(output.String(), "iptables") || cfg.verifyCalled {
		t.Fatalf("expected manual rules and no completion: %q", output.String())
	}
}

func TestExtenderFirewallUFWRuleActions(t *testing.T) {
	status := "Status: active\n22/tcp ALLOW Anywhere\n443/tcp DENY Anywhere\n443/udp ALLOW Anywhere\n4053/udp ALLOW Anywhere\n"
	fm := &FirewallManager{Platform: "linux", LookPath: func(name string) (string, error) {
		if name == "ufw" {
			return name, nil
		}
		return "", errors.New("missing")
	}, ExecCmd: func(string, ...string) ([]byte, error) { return []byte(status), nil }}
	if fm.Detect().PortsOpen {
		t.Fatal("denied TCP rule reported open")
	}
	status = strings.Replace(status, "443/tcp DENY", "443/tcp ALLOW", 1)
	if !fm.Detect().PortsOpen {
		t.Fatal("allowed rules not detected")
	}
}
