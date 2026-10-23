package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The miner port shipped operator-facing text promising a hot reload the
// process did not perform. These tests pin the promise to the mechanism: the
// message is decided by what the running provider reports, so it cannot drift
// from the mechanism again.

// withProviderReloadState answers the reload probe as a provider would.
func withProviderReloadState(t *testing.T, live, running bool) {
	t.Helper()
	prev := proxyReloadWatcherProbe
	t.Cleanup(func() { proxyReloadWatcherProbe = prev })
	proxyReloadWatcherProbe = func() (bool, bool) { return live, running }
}

func TestProxyChangeNoticeFollowsTheRunningProvider(t *testing.T) {
	cases := []struct {
		name     string
		live     bool
		running  bool
		mustHave string
		mustNot  string
	}{
		{"not running", false, false, "when it starts", "next restart"},
		{"running without a watcher", false, true, "next restart", "on its own"},
		{"running with a watcher", true, true, "on its own", "next restart"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProviderReloadState(t, tc.live, tc.running)
			notice := proxyChangeEffectNotice()
			if !strings.Contains(notice, tc.mustHave) {
				t.Fatalf("notice = %q, want it to contain %q", notice, tc.mustHave)
			}
			if strings.Contains(notice, tc.mustNot) {
				t.Fatalf("notice = %q, want it NOT to contain %q", notice, tc.mustNot)
			}
		})
	}
}

// A refresh rewrites the desired set, so the claim must also follow the running
// provider: "state updated" is only true when a watcher applies it.
func TestProxyRefreshNoticeDoesNotOverstate(t *testing.T) {
	cases := []struct {
		name    string
		live    bool
		running bool
		want    string
	}{
		{"not running", false, false, "Proxy refresh requested"},
		{"running without a watcher", false, true, "Proxy refresh requested"},
		{"running with a watcher", true, true, "Proxy state updated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProviderReloadState(t, tc.live, tc.running)
			if notice := proxyRefreshEffectNotice(); !strings.Contains(notice, tc.want) {
				t.Fatalf("notice = %q, want it to contain %q", notice, tc.want)
			}
		})
	}
}

// No miner source file may carry the old promises again.
func TestNoStaleHotReloadPromisesInMinerSources(t *testing.T) {
	forbidden := []string{
		"hot reload (no restart)",
		"apply changes within 2 seconds",
		"Reload triggered.",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, phrase := range forbidden {
			if strings.Contains(string(data), phrase) {
				t.Errorf("%s still contains the stale promise %q", name, phrase)
			}
		}
	}
}
