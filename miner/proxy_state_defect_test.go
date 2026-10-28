package miner

import (
	"context"
	"os"
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
		func() ([]*connect.ProxySettings, error) {
			launch, _, _, err := pm.desiredForReload()
			if err != nil {
				return nil, err
			}
			return launch, nil
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
