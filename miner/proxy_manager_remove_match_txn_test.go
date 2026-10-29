package miner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A URL fetch that lands right after the matches are removed must not bring
// them back: the exclusion is already persisted and the proxy lock is still
// held, so the merge either skips the address or does not run at all.
func TestRemoveMatchHoldsOutAURLMergeBetweenRemovalAndExclusion(t *testing.T) {
	withTempHome(t)
	allowLoopbackForTest(t)

	addr, cleanup := listenSocks5Once(t)
	defer cleanup()
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{addr: {}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(addr + "\n"))
	}))
	defer srv.Close()

	afterMatchRemovalHook = func() {
		fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 0, "", 0)
	}
	defer func() { afterMatchRemovalHook = nil }()

	if _, err := removeMatchingProxies("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if _, back := got.Cache[addr]; back {
		t.Fatalf("a URL merge during remove --match restored %s", addr)
	}
	if len(got.ExcludePatterns) != 1 {
		t.Fatalf("exclude patterns = %v, want the pattern persisted", got.ExcludePatterns)
	}
}

// If the removal fails part way, the exclusion is already on disk so no fetch
// can re-add the matches before the operator reruns the command.
func TestRemoveMatchPersistsTheExclusionBeforeRemoving(t *testing.T) {
	withTempHome(t)

	missing := filepath.Join(t.TempDir(), "gone", "proxies.txt")
	if err := writeProxyState(&ProxyState{
		Source:  missing,
		Proxies: map[string]ProxyEntry{"203.0.113.7:1080": {Source: "file"}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := removeMatchingProxies("203.0.113.7"); err == nil {
		t.Fatal("expected the unreadable file source to fail the removal")
	}
	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ExcludePatterns) != 1 || got.ExcludePatterns[0] != "203.0.113.7" {
		t.Fatalf("exclude patterns = %v, want the pattern persisted before the failed removal", got.ExcludePatterns)
	}
}

// A proxy_url.json that cannot be read cannot record the exclusion, so the
// command fails and removes nothing instead of reporting a removal that a
// later URL fetch would undo.
func TestRemoveMatchFailsWhenURLStateIsCorrupt(t *testing.T) {
	withTempHome(t)

	source := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(source, []byte("203.0.113.7:1080:u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Source:  source,
		Proxies: map[string]ProxyEntry{"203.0.113.7:1080": {Source: "file"}},
	}); err != nil {
		t.Fatal(err)
	}
	path, err := proxyURLStatePath()
	if err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("{not json")
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}

	removed, err := removeMatchingProxies("203.0.113.7")
	if err == nil {
		t.Fatalf("removed %d with an unreadable URL state, want an error", removed)
	}
	if removed != 0 {
		t.Fatalf("removed = %d alongside the error, want 0", removed)
	}
	if b, _ := os.ReadFile(path); string(b) != string(corrupt) {
		t.Fatalf("proxy_url.json was rewritten: %q", b)
	}
	if b, _ := os.ReadFile(source); string(b) != "203.0.113.7:1080:u:p\n" {
		t.Fatalf("source was modified: %q", b)
	}
	state, _ := readProxyState()
	if _, ok := state.Proxies["203.0.113.7:1080"]; !ok {
		t.Fatalf("proxy.state lost the proxy: %v", state.Proxies)
	}
}
