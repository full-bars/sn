package miner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// F2: an unreadable URL cache must leave the live set unchanged and report
// failure; a genuinely empty cache still removes them.
func TestProxyReloadUnreadableURLCacheLeavesLiveSetUnchanged(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	initialURLState := &ProxyURLState{
		Cache: map[string]ProxyURLEntry{
			"198.51.100.20:1080": {
				User:     "alice",
				Password: "secret",
			},
		},
	}
	if err := writeProxyURLState(initialURLState); err != nil {
		t.Fatalf("write initial proxy_url.json: %v", err)
	}

	pm := newProxyManager(defaultProxyManagerConfig())

	spawned := make(map[string]bool)
	reloader := newMinerProxyReloader(
		func() ([]*connect.ProxySettings, int, error) {
			launch, _, _, err := pm.desiredForReload()
			if err != nil {
				return nil, 0, err
			}
			return launch, pm.Skipped(), nil
		},
		func(settings *connect.ProxySettings) (context.CancelFunc, error) {
			key := settings.Key()
			spawned[key] = true
			return func() {
				delete(spawned, key)
			}, nil
		},
	)

	summary, err := reloader.reload()
	if err != nil {
		t.Fatalf("initial reload failed: %v", err)
	}
	if summary.Added != 1 {
		t.Fatalf("initial reload summary = %s, want added 1", summary)
	}
	urlKey := (&connect.ProxySettings{
		Network: "tcp",
		Address: "198.51.100.20:1080",
		Auth:    &proxy.Auth{User: "alice", Password: "secret"},
	}).Key()
	if !spawned[urlKey] {
		t.Fatalf("URL proxy %s was not spawned", urlKey)
	}

	// Corrupt proxy_url.json so it cannot be read.
	urlPath, err := proxyURLStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(urlPath, []byte("invalid-json{"), 0600); err != nil {
		t.Fatal(err)
	}

	// The reload must abort and leave the live set untouched.
	summary, err = reloader.reload()
	if err == nil {
		t.Fatal("reload succeeded on unreadable URL cache, want failure")
	}
	if !spawned[urlKey] {
		t.Fatalf("live set was mutated on unreadable URL cache; %s was cancelled", urlKey)
	}
	if keys := reloader.runningKeys(); len(keys) != 1 || keys[0] != urlKey {
		t.Fatalf("reloader running keys = %v, want [%s]", keys, urlKey)
	}

	// A genuinely empty cache must successfully remove the proxies.
	emptyURLState := &ProxyURLState{
		Cache: map[string]ProxyURLEntry{},
	}
	if err := writeProxyURLState(emptyURLState); err != nil {
		t.Fatal(err)
	}

	summary, err = reloader.reload()
	if err != nil {
		t.Fatalf("reload failed on genuinely empty cache: %v", err)
	}
	if summary.Removed != 1 {
		t.Fatalf("summary = %s, want removed 1", summary)
	}
	if spawned[urlKey] {
		t.Fatalf("URL proxy %s was not removed on genuinely empty cache", urlKey)
	}
	if keys := reloader.runningKeys(); len(keys) != 0 {
		t.Fatalf("reloader running keys = %v, want empty", keys)
	}
}

// F3: a file with only malformed lines, and an empty file, both leave the
// live set unchanged on reload; a file with one bad line and two good ones
// loads the two and reports one skipped.
func TestProxyReloadFileMalformedOrEmptyLeavesLiveSetUnchanged(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	proxyFile := filepath.Join(stateDir, "curated_proxies.txt")
	initialContent := "198.51.100.10:1080:alice:pass1\n198.51.100.11:1080:bob:pass2\n"
	if err := os.WriteFile(proxyFile, []byte(initialContent), 0600); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{
		Source: proxyFile,
	}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	pm := newProxyManager(defaultProxyManagerConfig())

	spawned := make(map[string]bool)
	reloader := newMinerProxyReloader(
		func() ([]*connect.ProxySettings, int, error) {
			launch, _, _, err := pm.desiredForReload()
			if err != nil {
				return nil, 0, err
			}
			return launch, pm.Skipped(), nil
		},
		func(settings *connect.ProxySettings) (context.CancelFunc, error) {
			key := settings.Key()
			spawned[key] = true
			return func() {
				delete(spawned, key)
			}, nil
		},
	)

	summary, err := reloader.reload()
	if err != nil {
		t.Fatalf("initial reload failed: %v", err)
	}
	if summary.Added != 2 {
		t.Fatalf("initial reload summary = %s, want added 2", summary)
	}
	if len(spawned) != 2 {
		t.Fatalf("spawned count = %d, want 2", len(spawned))
	}

	// Case 1: Empty file on reload must leave live set unchanged and report error.
	if err := os.WriteFile(proxyFile, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	summary, err = reloader.reload()
	if err == nil {
		t.Fatal("reload succeeded on empty proxy file, want error")
	}
	if len(spawned) != 2 {
		t.Fatalf("live set was mutated on empty file reload: spawned count = %d, want 2", len(spawned))
	}
	if keys := reloader.runningKeys(); len(keys) != 2 {
		t.Fatalf("running keys count = %d, want 2", len(keys))
	}

	// Case 2: Only malformed lines on reload must leave live set unchanged and report error.
	malformedContent := "# comment line\n\nnot-valid-proxy\n198.51.100.99:1080::\n"
	if err := os.WriteFile(proxyFile, []byte(malformedContent), 0600); err != nil {
		t.Fatal(err)
	}
	summary, err = reloader.reload()
	if err == nil {
		t.Fatal("reload succeeded on malformed proxy file, want error")
	}
	if len(spawned) != 2 {
		t.Fatalf("live set was mutated on malformed file reload: spawned count = %d, want 2", len(spawned))
	}
	if keys := reloader.runningKeys(); len(keys) != 2 {
		t.Fatalf("running keys count = %d, want 2", len(keys))
	}

	// Case 3: A file with one bad line and two good ones loads the two and reports one skipped.
	partialContent := "198.51.100.20:1080:carol:pass3\nbad-proxy-line\n198.51.100.21:1080:dave:pass4\n"
	if err := os.WriteFile(proxyFile, []byte(partialContent), 0600); err != nil {
		t.Fatal(err)
	}
	summary, err = reloader.reload()
	if err != nil {
		t.Fatalf("reload failed with partial content: %v", err)
	}
	if summary.Added != 2 || summary.Removed != 2 || summary.Skipped != 1 {
		t.Fatalf("summary = %s, want added 2, removed 2, skipped 1", summary)
	}
	if len(spawned) != 2 {
		t.Fatalf("spawned count = %d, want 2", len(spawned))
	}
}
