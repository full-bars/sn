package urnettools

import (
	"bytes"
	"errors"
	"testing"
)

func TestHSN4RequiresConfirmationBeforeMutation(t *testing.T) {
	testExtenderSetupRequiresConfirmationBeforeMutation(t)
}

func TestExtenderSetupRequiresConfirmationBeforeMutation(t *testing.T) {
	testExtenderSetupRequiresConfirmationBeforeMutation(t)
}

func testExtenderSetupRequiresConfirmationBeforeMutation(t *testing.T) {
	old := setupDiscoverFn
	setupDiscoverFn = func() []Provider { return []Provider{{Binary: "/provider", User: "test"}} }
	defer func() { setupDiscoverFn = old }()
	cfg := &mockPlatformConfigurer{platform: "linux"}
	fwWrites := 0
	fw := &FirewallManager{Platform: "linux", LookPath: func(name string) (string, error) {
		if name == "ufw" {
			return name, nil
		}
		return "", errors.New("not found")
	}, ExecCmd: func(name string, args ...string) ([]byte, error) {
		if name == "ufw" && len(args) > 0 && args[0] == "status" {
			return []byte("Status: active\n"), nil
		}
		fwWrites++
		return nil, nil
	}}
	err := RunExtenderSetup(ExtenderSetupOptions{Platform: "linux", PlatformCfg: cfg, FirewallMgr: fw,
		PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }},
		IsRoot:      func() bool { return true }, NonInteractive: true, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	if err == nil || cfg.applyCalled || fwWrites != 0 {
		t.Fatalf("unconfirmed setup mutated host: err=%v platformApplied=%v firewallWrites=%d", err, cfg.applyCalled, fwWrites)
	}
}
