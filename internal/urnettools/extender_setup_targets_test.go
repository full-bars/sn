package urnettools

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// A host with several providers cannot be narrowed (setup takes no flags), so
// every distinct provider is configured and nothing is refused with an
// inventory that names flags the command does not have.
func TestExtenderSetupConfiguresEveryProviderWithoutFlags(t *testing.T) {
	old := setupDiscoverFn
	defer func() { setupDiscoverFn = old }()
	setupDiscoverFn = func() []Provider {
		return []Provider{
			{Unit: "a.service", User: "alice", Binary: "/opt/a/miner"},
			{Unit: "b.service", User: "bob", Binary: "/opt/b/miner"},
			{Unit: "c.service", User: "bob", Binary: "/opt/b/miner"}, // same binary and user as b
		}
	}
	// The unprivileged auto-narrow must not apply: act as root.
	cfg := &recordingConfigurer{mockPlatformConfigurer: mockPlatformConfigurer{platform: "linux"}}
	opts := setupRegressionOptions()
	opts.IsRoot = func() bool { return true }
	opts.Yes = true
	opts.PlatformCfg = cfg
	opts.FirewallMgr = &FirewallManager{Platform: "linux", LookPath: func(string) (string, error) { return "", errors.New("missing") }}
	err := RunExtenderSetup(opts)
	if err != nil && strings.Contains(err.Error(), "resolving target") {
		t.Fatalf("multi-provider host refused: %v", err)
	}
	if want := []string{"/opt/a/miner|alice", "/opt/b/miner|bob"}; !reflect.DeepEqual(cfg.applied, want) {
		t.Fatalf("applied to %v, want %v (err=%v)", cfg.applied, want, err)
	}
	// The route is a flag setup was given; the command must not add any
	// target flags of its own.
	opts.Route = ""
	if cmd := formatSetupSudoCommand(opts); strings.Contains(cmd, "--") {
		t.Fatalf("sudo command names flags the command does not have: %s", cmd)
	}
}

type recordingConfigurer struct {
	mockPlatformConfigurer
	applied []string
}

func (r *recordingConfigurer) ApplyRequirement(binary, user string) (bool, string, error) {
	r.applied = append(r.applied, binary+"|"+user)
	return true, "applied", nil
}
