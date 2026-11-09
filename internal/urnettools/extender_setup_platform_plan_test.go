package urnettools

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Linux provider running as root whose platform requirement is not satisfied
// must have its setcap command included in the preview and confirmed plan.
func TestExtenderSetupLinuxRootProviderIncludesSetcapInPlan(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "urnetwork")
	if err := os.WriteFile(bin, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldDiscover := setupDiscoverFn
	setupDiscoverFn = func() []Provider {
		return []Provider{{Binary: bin, User: "root"}}
	}
	defer func() { setupDiscoverFn = oldDiscover }()

	sp := &SystemPlatformConfigurer{
		PlatformName: "linux",
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			if name == "getcap" {
				return []byte(""), nil
			}
			return nil, nil
		},
		Lstat: os.Lstat,
	}

	fwMgr := &FirewallManager{
		Platform: "linux",
		LookPath: func(string) (string, error) { return "", errors.New("no firewall") },
	}

	var out bytes.Buffer
	err := RunExtenderSetup(ExtenderSetupOptions{
		Platform:    "linux",
		PlatformCfg: sp,
		FirewallMgr: fwMgr,
		PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }},
		IsRoot:      func() bool { return true },
		DryRun:      true,
		Stdout:      &out,
		Stderr:      &out,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "setcap cap_net_bind_service=+ep") {
		t.Fatalf("dry run plan omitted setcap for root provider: output=%q", output)
	}
}

// A nonempty aggregate plan (e.g. firewall only) must not count as approval
// for an omitted platform action.
type omittedPlatformConfigurer struct {
	applyCalled bool
}

func (o *omittedPlatformConfigurer) Platform() string { return "linux" }

func (o *omittedPlatformConfigurer) CheckRequirement(binary, user string) (bool, string, error) {
	return false, "needs platform configuration", nil
}

func (o *omittedPlatformConfigurer) PlanRequirement(binary, user string) ([]setupCommand, error) {
	// Returns no commands for this provider
	return nil, nil
}

func (o *omittedPlatformConfigurer) ApplyRequirement(binary, user string) (bool, string, error) {
	o.applyCalled = true
	return true, "applied platform action", nil
}

func (o *omittedPlatformConfigurer) VerifyBind(binary, user string) (bool, error) {
	return false, nil
}

func TestExtenderSetupOmittedPlatformActionNotAppliedByFirewallConfirmation(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "urnetwork")
	if err := os.WriteFile(bin, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldDiscover := setupDiscoverFn
	setupDiscoverFn = func() []Provider {
		return []Provider{{Binary: bin, User: "provider"}}
	}
	defer func() { setupDiscoverFn = oldDiscover }()

	mockCfg := &omittedPlatformConfigurer{}

	fwMutated := false
	fwMgr := &FirewallManager{
		Platform: "linux",
		LookPath: func(name string) (string, error) {
			if name == "ufw" {
				return "/usr/sbin/ufw", nil
			}
			return "", errors.New("not found")
		},
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			if name == "ufw" {
				if len(args) == 1 && args[0] == "status" {
					return []byte("Status: active\n"), nil
				}
				fwMutated = true
				return nil, nil
			}
			return nil, nil
		},
	}

	var out bytes.Buffer
	err := RunExtenderSetup(ExtenderSetupOptions{
		Route:       "capability",
		Platform:    "linux",
		PlatformCfg: mockCfg,
		FirewallMgr: fwMgr,
		PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }},
		IsRoot:      func() bool { return true },
		Yes:         true,
		Stdout:      &out,
		Stderr:      &out,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fwMutated {
		t.Fatal("expected firewall rules to be applied")
	}
	if mockCfg.applyCalled {
		t.Fatal("omitted platform action was applied because aggregate plan was nonempty")
	}
}

// When platform planning fails on a platformPlanner, setup must fail and report
// the failure; it must not show the fallback as an exact command.
type failingPlatformPlanner struct {
}

func (f *failingPlatformPlanner) Platform() string { return "linux" }

func (f *failingPlatformPlanner) CheckRequirement(binary, user string) (bool, string, error) {
	return false, "needs capability", nil
}

func (f *failingPlatformPlanner) PlanRequirement(binary, user string) ([]setupCommand, error) {
	return nil, errors.New("getcap failed: permission denied")
}

func (f *failingPlatformPlanner) ApplyRequirement(binary, user string) (bool, string, error) {
	return false, "", errors.New("should not be called")
}

func (f *failingPlatformPlanner) VerifyBind(binary, user string) (bool, error) {
	return false, nil
}

func TestExtenderSetupPlatformPlanningFailureReportsError(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "urnetwork")
	if err := os.WriteFile(bin, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldDiscover := setupDiscoverFn
	setupDiscoverFn = func() []Provider {
		return []Provider{{Binary: bin, User: "provider"}}
	}
	defer func() { setupDiscoverFn = oldDiscover }()

	cfg := &failingPlatformPlanner{}
	fwMgr := &FirewallManager{
		Platform: "linux",
		LookPath: func(string) (string, error) { return "", errors.New("no firewall") },
	}

	var out bytes.Buffer
	err := RunExtenderSetup(ExtenderSetupOptions{
		Route:       "capability",
		Platform:    "linux",
		PlatformCfg: cfg,
		FirewallMgr: fwMgr,
		PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }},
		IsRoot:      func() bool { return true },
		Yes:         true,
		Stdout:      &out,
		Stderr:      &out,
	})
	if err == nil {
		t.Fatal("expected error when platform planning fails, got nil")
	}
	if !strings.Contains(err.Error(), "planning platform requirement") || !strings.Contains(err.Error(), "getcap failed") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
