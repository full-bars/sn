package miner

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// TestTrimPreviewDoesNotWriteStateButReloadDoes pins the cause of the CI
// "trim-preview-no-write" flake on Alpine. `proxy add` leaves new entries in
// the proxy file and the running provider's reloader (not the add command)
// assigns their proxy.state identities on the next watcher tick. So:
//   - the preview is read-only: proxy.state is byte-identical across it, and
//   - the reload is the writer: it grows proxy.state after the add.
//
// A preview that runs before the reload has applied therefore overlaps the
// reload's write, and a byte-identity check around it is only sound once the
// reload has been observed to finish.
func TestTrimPreviewDoesNotWriteStateButReloadDoes(t *testing.T) {
	withTempHome(t)
	statePath, err := providerStatePath("proxy.state")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{Version: 2, StartedAt: time.Now(), Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	cfg := readProxyConfig()
	if !applyProxyAdd(cfg, []string{"192.0.2.10:1080", "192.0.2.11:1080"}, false, func(string, string, string) bool { return true }) {
		t.Fatal("applyProxyAdd refused")
	}
	writeProxyConfig(cfg)

	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	// The preview alone.
	if _, err := buildMinerTrimPreview(1); err != nil {
		t.Fatalf("buildMinerTrimPreview: %v", err)
	}
	afterPreview, _ := os.ReadFile(statePath)
	if !bytes.Equal(before, afterPreview) {
		t.Fatalf("preview wrote proxy.state:\nbefore=%s\nafter=%s", before, afterPreview)
	}

	// The reload that `proxy add` triggers.
	m := newProxyManager(defaultProxyManagerConfig())
	if _, _, _, err := m.desiredForReload(); err != nil {
		t.Fatal(err)
	}
	afterReload, _ := os.ReadFile(statePath)
	if bytes.Equal(before, afterReload) {
		t.Fatalf("reload left proxy.state unchanged (%d bytes); the race cannot occur", len(before))
	}
	t.Logf("proxy.state: %d bytes -> %d bytes across the reload, unchanged across the preview", len(before), len(afterReload))
}
