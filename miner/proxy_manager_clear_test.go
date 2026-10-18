package miner

import (
	"strings"
	"testing"
	"time"
)

// proxy_manager_clear_test.go covers the `proxy clear` surface and the
// `proxy remove --all` reset it shares (v33's clearProxyStores semantics):
// internal config, proxy.state and the URL cache/source list, with the
// selected workflow (proxy.state.Source) deliberately preserved.

// seedClearableStores writes one entry into every store a clear resets.
func seedClearableStores(t *testing.T) {
	t.Helper()
	writeProxyConfig(&ProxyConfig{Servers: map[string]string{"1.1.1.1:1080": "", "2.2.2.2:1080": "key"}})
	if err := writeProxyState(&ProxyState{
		Source:  "/some/proxies.txt",
		NextID:  7,
		Proxies: map[string]ProxyEntry{"1.1.1.1:1080": {ID: 3, Health: "up"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{
		Sources:         []string{"https://example.com/list.txt"},
		Cache:           map[string]ProxyURLEntry{"3.3.3.3:1080": {ProbeOK: true}},
		Blacklist:       map[string]time.Time{"4.4.4.4:1080": time.Now()},
		ExcludePatterns: []string{"bad"},
	}); err != nil {
		t.Fatal(err)
	}
}

// assertStoresCleared checks the reset contract: config servers, proxy.state
// Proxies and the URL cache/sources are empty; proxy.state.Source survives as
// the selected workflow; proxy_url.json's non-cache configuration survives.
func assertStoresCleared(t *testing.T) {
	t.Helper()
	if cfg := readProxyConfig(); len(cfg.Servers) != 0 {
		t.Fatalf("internal config not cleared: %v", cfg.Servers)
	}
	state, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Proxies) != 0 {
		t.Fatalf("proxy.state.Proxies not cleared: %v", state.Proxies)
	}
	if state.NextID != 0 {
		t.Fatalf("proxy.state.NextID = %d, want 0 (v33 resets ID assignment)", state.NextID)
	}
	if state.Source != "/some/proxies.txt" {
		t.Fatalf("proxy.state.Source = %q, want the selected workflow preserved", state.Source)
	}
	urlState, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if len(urlState.Cache) != 0 {
		t.Fatalf("URL cache not cleared: %v", urlState.Cache)
	}
	if len(urlState.Sources) != 0 {
		t.Fatalf("URL sources not cleared: %v", urlState.Sources)
	}
}

func TestClearProxyStoresResetsAllStores(t *testing.T) {
	withTempHome(t)
	seedClearableStores(t)

	if err := clearProxyStores(); err != nil {
		t.Fatal(err)
	}
	assertStoresCleared(t)
}

// TestProxyRemoveAllRoutesThroughClearProxyStores is the wiring proof that
// `proxy remove --all` performs the full v33 reset (not just the internal
// config, which left proxy.state and the URL cache to revive the removed
// proxies).
func TestProxyRemoveAllRoutesThroughClearProxyStores(t *testing.T) {
	withTempHome(t)
	seedClearableStores(t)

	proxyRemove(parseArgsForTest(t, []string{"proxy", "remove", "--all"}))
	assertStoresCleared(t)
}

// TestProxyClearUsageDispatches pins the docopt wiring: `proxy clear` parses,
// and adding the verb does not shadow `proxy remove --all`.
func TestProxyClearUsageDispatches(t *testing.T) {
	opts := parseArgsForTest(t, []string{"proxy", "clear"})
	if clearCmd, _ := opts.Bool("clear"); !clearCmd {
		t.Fatal("proxy clear did not set the clear command")
	}

	opts = parseArgsForTest(t, []string{"proxy", "remove", "--all"})
	if remove, _ := opts.Bool("remove"); !remove {
		t.Fatal("proxy remove --all lost the remove command")
	}
	if all, _ := opts.Bool("--all"); !all {
		t.Fatal("proxy remove --all lost the --all flag")
	}
}

// TestProxyClearTriggersReloadWhenRunning: a clear on a provider whose state
// says it is running writes the reload trigger so the reset applies without a
// restart, and reports what it removed.
func TestProxyClearTriggersReloadWhenRunning(t *testing.T) {
	withTempHome(t)
	seedClearableStores(t)

	if err := writeProxyState(&ProxyState{
		Source:    "/some/proxies.txt",
		StartedAt: time.Now(),
		NextID:    7,
		Proxies:   map[string]ProxyEntry{"1.1.1.1:1080": {ID: 3, Health: "up"}},
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		proxyClear(parseArgsForTest(t, []string{"proxy", "clear"}))
	})
	if !strings.Contains(out, "proxy clear: removed 2 internal server(s)") {
		t.Fatalf("clear summary missing or wrong:\n%s", out)
	}
	assertStoresCleared(t)

	reloadPath, err := proxyReloadPath()
	if err != nil {
		t.Fatal(err)
	}
	seq, err := readReloadSeq(reloadPath)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("reload trigger seq = %d, want 1", seq)
	}
}
