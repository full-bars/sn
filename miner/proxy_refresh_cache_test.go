package miner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

func TestProxyRefreshCachePreview(t *testing.T) {
	withTempHome(t)
	url := authSettingsFor("198.51.100.20:1080", "alice")
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		url.Address: {User: "alice", Password: "p"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{StartedAt: time.Now().Add(-9 * time.Hour), Proxies: map[string]ProxyEntry{
		url.Key(): {ID: 1, Health: "up"},
	}}); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { proxyRefresh(docopt.Opts{}) })
	if !strings.Contains(out, "proxy list is already up to date") {
		t.Fatalf("cached live proxy must not be reported as removed: %s", out)
	}
}

func TestProxyRefreshAddOnlyCacheMatchesReload(t *testing.T) {
	dir := withTempHome(t)
	file := filepath.Join(dir, "curated.txt")
	if err := os.WriteFile(file, []byte("198.51.100.10:1080:u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	url := authSettingsFor("198.51.100.20:1080", "alice")
	curated := authSettingsFor("198.51.100.10:1080", "u")
	added := authSettingsFor("198.51.100.11:1080", "u")
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{url.Address: {User: "alice", Password: "p"}}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{Source: file, StartedAt: time.Now().Add(-9 * time.Hour), Proxies: map[string]ProxyEntry{
		url.Key(): {ID: 1, Health: "up"}, curated.Key(): {ID: 2, Health: "up"},
	}}); err != nil {
		t.Fatal(err)
	}
	pm := newProxyManager(defaultProxyManagerConfig())
	running := map[string]bool{}
	r := newMinerProxyReloader(func() ([]*connect.ProxySettings, int, error) {
		launch, _, _, err := pm.desiredForReload()
		return launch, pm.Skipped(), err
	}, func(s *connect.ProxySettings) (context.CancelFunc, error) {
		running[s.Key()] = true
		return func() { delete(running, s.Key()) }, nil
	})
	if _, err := r.reload(); err != nil {
		t.Fatal(err)
	}
	if !running[url.Key()] || !running[curated.Key()] {
		t.Fatalf("initial running set: %v", running)
	}
	if err := os.WriteFile(file, []byte("198.51.100.10:1080:u:p\n198.51.100.11:1080:u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	feedStdin(t, "y\n")
	path, err := proxyReloadPath()
	if err != nil {
		t.Fatal(err)
	}
	before, _ := readReloadSeq(path)
	out := captureStdout(t, func() { proxyRefresh(docopt.Opts{}) })
	after, _ := readReloadSeq(path)
	if after != before+1 {
		t.Fatalf("refresh did not trigger reload: %s", out)
	}
	summary, err := r.reload()
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("proxy refresh: %d proxies will be removed, %d will be added.", summary.Removed, summary.Added)
	if !strings.Contains(out, want) {
		t.Errorf("preview does not match reload (%s): %s", want, out)
	}
	if summary.Removed != 0 || summary.Added != 1 || !running[url.Key()] || !running[curated.Key()] || !running[added.Key()] {
		t.Fatalf("add-only reload summary=%+v running=%v", summary, running)
	}
}

func TestProxyRefreshCuratedFileAuthority(t *testing.T) {
	dir := withTempHome(t)
	file := filepath.Join(dir, "curated.txt")
	if err := os.WriteFile(file, []byte("198.51.100.10:1080:u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	writeProxyConfig(&ProxyConfig{Servers: map[string]string{"198.51.100.99:1080": ""}})
	state := &ProxyState{Source: file, Proxies: map[string]ProxyEntry{
		authSettingsFor("198.51.100.10:1080", "u").Key(): {ID: 1},
		"198.51.100.99:1080":                             {ID: 2},
	}}
	desired, err := proxyRefreshDesired(state)
	if err != nil {
		t.Fatal(err)
	}
	added, removed := planProxyRefresh(desired, state.Proxies)
	if len(added) != 0 || len(removed) != 1 || removed[0].key != "198.51.100.99:1080" {
		t.Fatalf("file must be the curated authority: added=%v removed=%v", added, removed)
	}
}
