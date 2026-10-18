package miner

import (
	"testing"

	"github.com/docopt/docopt-go"
)

// The fleet path must receive the kill switch: a child never inherits the
// supervisor's flags any other way.
func TestOperatorChildArgsForwardNoLegacyImport(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		supervisor := &operatorSupervisor{settings: operatorSupervisorSettings{noLegacyImport: enabled}}
		args := supervisor.childArgs(operatorChildSpec{
			apiUrl:     "https://api.example.invalid",
			connectUrl: "wss://connect.example.invalid",
		})
		found := false
		for _, arg := range args {
			if arg == "--no-legacy-import" {
				found = true
			}
		}
		if found != enabled {
			t.Fatalf("noLegacyImport=%v forwarded=%v args=%v", enabled, found, args)
		}
	}
}

func TestOperatorSupervisorSettingsReadNoLegacyImport(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	for _, enabled := range []bool{false, true} {
		opts := docopt.Opts{"--port": "0", "--no-legacy-import": enabled}
		settings, _, err := newOperatorSupervisorSettings(opts)
		if err != nil {
			t.Fatalf("settings: %v", err)
		}
		if settings.noLegacyImport != enabled {
			t.Fatalf("noLegacyImport=%v, want %v", settings.noLegacyImport, enabled)
		}
	}
}
