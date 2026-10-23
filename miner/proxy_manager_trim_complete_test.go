package miner

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A failed proxy_url.json read leaves every URL-sourced proxy out of the
// desired set, so that count is not the configured pool: reporting it would
// understate the pool and clamp the cap below the count the operator
// configured. Only a complete pool may replace the last decision.

// trimPoolFixture builds a manager whose desired set is 2 file entries plus
// `cached` URL-cache entries, in an isolated state directory with a 5 proxy
// trim cap set.
func trimPoolFixture(t *testing.T, cached int) *proxyManager {
	t.Helper()
	withTempHome(t)

	sourcePath := filepath.Join(t.TempDir(), "proxies.txt")
	writeProxyFile(t, sourcePath, "1.1.1.1:1080:alice:secret", "2.2.2.2:1080:bob:secret")
	cache := make(map[string]ProxyURLEntry, cached)
	for i := 0; i < cached; i++ {
		cache[fmt.Sprintf("10.0.0.%d:8080", i+1)] = ProxyURLEntry{}
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: cache}); err != nil {
		t.Fatal(err)
	}
	if err := writeTrimTarget(5); err != nil {
		t.Fatal(err)
	}

	cfg := defaultProxyManagerConfig()
	cfg.ProxyFile = sourcePath
	return newProxyManager(cfg)
}

// breakProxyURLCache makes proxy_url.json unreadable for a reason other than
// "not set": a directory at the file path.
func breakProxyURLCache(t *testing.T) {
	t.Helper()
	path, err := proxyURLStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestTrimSelectionRecordedFromCompletePool(t *testing.T) {
	m := trimPoolFixture(t, 5)

	if _, _, _, err := m.desired(true); err != nil {
		t.Fatal(err)
	}
	desired, cap, source, ok := currentTrimSelection()
	if !ok {
		t.Fatal("selection not recorded from a complete pool")
	}
	if desired != 7 || cap != 5 || source != trimCapOperator {
		t.Fatalf("got desired=%d cap=%d source=%q, want 7/5/%q", desired, cap, source, trimCapOperator)
	}
}

// The reload-time bug the relay fixed: an unreadable URL cache must keep the
// last complete decision, not replace it with the URL-less subset.
func TestTrimSelectionKeptWhenURLCacheUnreadable(t *testing.T) {
	m := trimPoolFixture(t, 5)
	if _, _, _, err := m.desired(true); err != nil {
		t.Fatal(err)
	}
	breakProxyURLCache(t)

	if _, _, _, err := m.desired(true); err != nil {
		t.Fatal(err)
	}
	desired, cap, _, ok := currentTrimSelection()
	if !ok {
		t.Fatal("selection dropped when the URL cache became unreadable")
	}
	if desired != 7 || cap != 5 {
		t.Fatalf("got desired=%d cap=%d, want the last complete 7/5", desired, cap)
	}
}

// A first start with an unreadable URL cache has no complete decision to keep:
// it must report nothing and let urnet-tools read the operator file, rather
// than report the URL-less subset as the configured pool.
func TestTrimSelectionNotRecordedFromFirstIncompletePool(t *testing.T) {
	m := trimPoolFixture(t, 5)
	breakProxyURLCache(t)

	if _, _, _, err := m.desired(true); err != nil {
		t.Fatal(err)
	}
	if desired, cap, source, ok := currentTrimSelection(); ok {
		t.Fatalf("recorded %d/%d/%q from an unreadable URL cache; want no selection", desired, cap, source)
	}
}
