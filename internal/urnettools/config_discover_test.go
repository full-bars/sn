package urnettools

import (
	"bytes"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestConfigUsesDiscoveredStateDir: config must reach the provider's socket
// in the state dir discovery reports, not ~/.urnetwork under the caller's
// HOME. Running as root against a provider owned by another user (the
// shakedown's setup) is exactly that case: HOME is /root, the provider's
// state dir is under the provider user's home.
func TestConfigUsesDiscoveredStateDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix domain sockets not supported on Windows CI")
	}
	t.Setenv("HOME", t.TempDir())
	stateDir := t.TempDir()
	stubDiscovery(t, []Provider{{StateDir: stateDir, Running: true}}, nil)

	cleanup := startMockStatusServer(t, filepath.Join(stateDir, "provider.sock"), controlResponse{
		OK:       true,
		Settings: map[string]SettingInfo{"node_name": {Value: "discovered-node", Source: "socket"}},
	})
	defer cleanup()

	var buf bytes.Buffer
	if err := runConfig(&buf, nil); err != nil {
		t.Fatalf("runConfig: %v", err)
	}
	if !strings.Contains(buf.String(), "discovered-node") {
		t.Fatalf("config did not read the discovered provider's socket:\n%s", buf.String())
	}
}
