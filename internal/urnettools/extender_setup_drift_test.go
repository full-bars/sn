package urnettools

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestExtenderSetupDriftRefusesFirewallChange(t *testing.T) {
	oldDiscover := setupDiscoverFn
	setupDiscoverFn = func() []Provider {
		return []Provider{{Binary: "/usr/local/bin/urnetwork", User: "provider"}}
	}
	defer func() { setupDiscoverFn = oldDiscover }()

	cfg := &mockPlatformConfigurer{platform: "linux", configured: false}

	detectCount := 0
	fwWrites := 0
	fwMgr := &FirewallManager{
		Platform: "linux",
		LookPath: func(name string) (string, error) {
			if name == "ufw" {
				return "/usr/sbin/ufw", nil
			}
			return "", errors.New("not found")
		},
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			if name == "ufw" && len(args) > 0 && args[0] == "status" {
				detectCount++
				if detectCount == 1 {
					// 1st Detect during initial plan: active with missing ports
					return []byte("Status: active\n"), nil
				}
				// 2nd Detect during drift guard check: firewall became inactive or open
				return []byte("Status: inactive\n"), nil
			}
			fwWrites++
			return nil, nil
		},
	}

	var out, errOut bytes.Buffer
	err := RunExtenderSetup(ExtenderSetupOptions{
		Platform:    "linux",
		PlatformCfg: cfg,
		FirewallMgr: fwMgr,
		PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }},
		IsRoot:      func() bool { return true },
		Yes:         true,
		Stdout:      &out,
		Stderr:      &errOut,
	})

	if err == nil {
		t.Fatal("expected setup to abort due to firewall drift, got nil error")
	}
	if !strings.Contains(err.Error(), "host configuration changed") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if cfg.applyCalled {
		t.Fatal("platform requirement was applied despite drift")
	}
	if fwWrites != 0 {
		t.Fatalf("firewall was mutated %d times despite drift", fwWrites)
	}
}

func TestExtenderSetupDriftRefusesPlatformChange(t *testing.T) {
	oldDiscover := setupDiscoverFn
	setupDiscoverFn = func() []Provider {
		return []Provider{{Binary: "/usr/local/bin/urnetwork", User: "provider"}}
	}
	defer func() { setupDiscoverFn = oldDiscover }()

	checkCount := 0
	cfg := &mockPlatformConfigurer{platform: "linux"}
	// Fake PlatformConfigurer whose CheckRequirement changes between calls
	fakeCfg := &driftPlatformConfigurer{
		PlatformConfigurer: cfg,
		checkFn: func(bin, user string) (bool, string, error) {
			checkCount++
			if checkCount == 1 {
				return false, "needs capability", nil
			}
			return true, "already configured", nil
		},
	}

	fwWrites := 0
	fwMgr := &FirewallManager{
		Platform: "linux",
		LookPath: func(string) (string, error) { return "", errors.New("none") },
	}

	var out, errOut bytes.Buffer
	err := RunExtenderSetup(ExtenderSetupOptions{
		Platform:    "linux",
		PlatformCfg: fakeCfg,
		FirewallMgr: fwMgr,
		PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }},
		IsRoot:      func() bool { return true },
		Yes:         true,
		Stdout:      &out,
		Stderr:      &errOut,
	})

	if err == nil {
		t.Fatal("expected setup to abort due to platform drift, got nil error")
	}
	if !strings.Contains(err.Error(), "host configuration changed") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if fakeCfg.applied {
		t.Fatal("platform requirement was applied despite drift")
	}
	if fwWrites != 0 {
		t.Fatalf("firewall was mutated %d times despite drift", fwWrites)
	}
}

type driftPlatformConfigurer struct {
	PlatformConfigurer
	checkFn func(bin, user string) (bool, string, error)
	applied bool
}

func (d *driftPlatformConfigurer) CheckRequirement(bin, user string) (bool, string, error) {
	if d.checkFn != nil {
		return d.checkFn(bin, user)
	}
	return d.PlatformConfigurer.CheckRequirement(bin, user)
}

func (d *driftPlatformConfigurer) ApplyRequirement(bin, user string) (bool, string, error) {
	d.applied = true
	return true, "applied", nil
}
