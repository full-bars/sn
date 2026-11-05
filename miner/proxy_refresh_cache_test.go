package miner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
	if err := writeProxyState(&ProxyState{Source: file, StartedAt: time.Now().Add(-9 * time.Hour), Proxies: map[string]ProxyEntry{
		authSettingsFor("198.51.100.10:1080", "u").Key(): {ID: 1},
		"198.51.100.99:1080":                             {ID: 2},
	}}); err != nil {
		t.Fatal(err)
	}
	feedStdin(t, "y\ny\n")
	out := captureStdout(t, func() { proxyRefresh(docopt.Opts{}) })
	if !strings.Contains(out, "1 proxies will be removed, 0 will be added.") || !strings.Contains(out, "198.51.100.99") {
		t.Fatalf("file must be the curated authority (internal config ignored): %s", out)
	}
}

// TestProxyRefreshChild is the body of runProxyRefreshChild: proxyCommandFatal
// ends the command with os.Exit, so a fatal preview needs its own process.
func TestProxyRefreshChild(t *testing.T) {
	if os.Getenv("GO_WANT_REFRESH_CHILD") != "1" {
		t.Skip("helper process")
	}
	proxyRefresh(docopt.Opts{})
}

func runProxyRefreshChild(t *testing.T) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProxyRefreshChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "GO_WANT_REFRESH_CHILD=1")
	cmd.Stdin = strings.NewReader("n\nn\n")
	b, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, string(b)
}

// previewFixture runs a real reloader over desiredForReload, exactly as the
// provider does, with the running set seeded from `seed` settings.
type previewFixture struct {
	r       *minerProxyReloader
	running map[string]bool
}

func newPreviewFixture(t *testing.T) *previewFixture {
	t.Helper()
	pm := newProxyManager(defaultProxyManagerConfig())
	f := &previewFixture{running: map[string]bool{}}
	f.r = newMinerProxyReloader(func() ([]*connect.ProxySettings, int, error) {
		launch, _, _, err := pm.desiredForReload()
		return launch, pm.Skipped(), err
	}, func(s *connect.ProxySettings) (context.CancelFunc, error) {
		f.running[s.Key()] = true
		return func() { delete(f.running, s.Key()) }, nil
	})
	return f
}

// stateFromRunning records the running set as proxy.state, as the provider does.
func (f *previewFixture) writeState(t *testing.T, source string) {
	t.Helper()
	proxies := map[string]ProxyEntry{}
	id := 1
	for key := range f.running {
		proxies[key] = ProxyEntry{ID: id}
		id++
	}
	if err := writeProxyState(&ProxyState{Source: source, StartedAt: time.Now().Add(-9 * time.Hour), Proxies: proxies}); err != nil {
		t.Fatal(err)
	}
}

func writeCurated(t *testing.T, n int) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "curated.txt")
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "198.51.100.%d:1080:u:p\n", 10+i)
	}
	if err := os.WriteFile(file, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

// The reload removes every running proxy outside the trimmed launch set, so the
// preview must count them. A preview that diffed the untrimmed set would say
// "up to date" while the reload shrinks the pool.
func TestProxyRefreshPreviewHonorsTrimCap(t *testing.T) {
	oomCapTestEnv(t, "off")
	file := writeCurated(t, 4)
	if err := writeProxyState(&ProxyState{Source: file, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	f := newPreviewFixture(t)
	if _, err := f.r.reload(); err != nil || len(f.running) != 4 {
		t.Fatalf("seed reload: err=%v running=%v", err, f.running)
	}
	f.writeState(t, file)
	if err := writeTrimTarget(2); err != nil {
		t.Fatal(err)
	}
	feedStdin(t, "y\ny\ny\n")
	out := captureStdout(t, func() { proxyRefresh(docopt.Opts{}) })
	summary, err := f.r.reload()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Removed != 2 {
		t.Fatalf("sanity: the trim cap must make the reload remove 2, got %+v", summary)
	}
	want := fmt.Sprintf("proxy refresh: %d proxies will be removed, %d will be added.", summary.Removed, summary.Added)
	if !strings.Contains(out, want) {
		t.Fatalf("preview must match the trimmed reload (%s): %s", want, out)
	}
}

// A URL proxy inside its give-up backoff window is dropped by the reload, so
// the preview must list it as removed, not keep it.
func TestProxyRefreshPreviewHonorsGiveUpBackoff(t *testing.T) {
	withTempHome(t)
	held := authSettingsFor("198.51.100.20:1080", "")
	kept := authSettingsFor("198.51.100.21:1080", "")
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{held.Address: {}, kept.Address: {}}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	f := newPreviewFixture(t)
	if _, err := f.r.reload(); err != nil || !f.running[held.Key()] || !f.running[kept.Key()] {
		t.Fatalf("seed reload: err=%v running=%v", err, f.running)
	}
	f.writeState(t, "")
	globalProxyFailureHistory.SetBackoffUntil(held.Key(), time.Now().Add(time.Hour))
	feedStdin(t, "y\ny\ny\n")
	out := captureStdout(t, func() { proxyRefresh(docopt.Opts{}) })
	summary, err := f.r.reload()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Removed != 1 || f.running[held.Key()] {
		t.Fatalf("sanity: the reload must drop the backed-off proxy, got %+v running=%v", summary, f.running)
	}
	want := fmt.Sprintf("proxy refresh: %d proxies will be removed, %d will be added.", summary.Removed, summary.Added)
	if !strings.Contains(out, want) {
		t.Fatalf("preview must match the backoff-aware reload (%s): %s", want, out)
	}
}

// The reload refuses an empty curated file; the preview must refuse it too,
// never offer to "remove ALL proxies".
func TestProxyRefreshPreviewRefusesEmptyCuratedFile(t *testing.T) {
	withTempHome(t)
	file := writeCurated(t, 0)
	running := authSettingsFor("198.51.100.10:1080", "u")
	if err := writeProxyState(&ProxyState{Source: file, StartedAt: time.Now().Add(-9 * time.Hour), Proxies: map[string]ProxyEntry{
		running.Key(): {ID: 1, Health: "up"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := newProxyManager(defaultProxyManagerConfig()).desiredForReload(); err == nil {
		t.Fatal("sanity: the reload read must refuse an empty curated file")
	}
	code, out := runProxyRefreshChild(t)
	if code != 54 || strings.Contains(out, "remove ALL") {
		t.Fatalf("preview must abort (exit 54) like the reload, code=%d out=%s", code, out)
	}
}

// A proxy_url.json that stays unreadable aborts the reload; the preview must
// abort too instead of reporting every URL proxy as removed.
func TestProxyRefreshPreviewRefusesUnreadableURLCache(t *testing.T) {
	dir := withTempHome(t)
	url := authSettingsFor("198.51.100.20:1080", "alice")
	if err := writeProxyState(&ProxyState{StartedAt: time.Now().Add(-9 * time.Hour), Proxies: map[string]ProxyEntry{
		url.Key(): {ID: 1, Health: "up"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "proxy_url.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := newProxyManager(defaultProxyManagerConfig()).desiredForReload(); err == nil {
		t.Fatal("sanity: the reload read must fail on a corrupt cache")
	}
	code, out := runProxyRefreshChild(t)
	if code != 54 || strings.Contains(out, "will be removed") {
		t.Fatalf("preview must abort (exit 54) like the reload, code=%d out=%s", code, out)
	}
}
