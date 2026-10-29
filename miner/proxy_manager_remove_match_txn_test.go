package miner

import (
	"context"
	"net/http"
	"net/http/httptest"
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
