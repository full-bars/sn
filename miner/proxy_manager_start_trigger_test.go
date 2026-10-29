package miner

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Startup captures its launch set, a clear then empties the stores and writes
// its trigger, and only after that does the watcher start. The watcher must
// apply that trigger instead of adopting it as its baseline.
func TestWatcherAppliesAClearThatLandedBeforeItStarted(t *testing.T) {
	withTempHome(t)
	writeProxyConfig(&ProxyConfig{Servers: map[string]string{"198.51.100.10:1080": ""}})

	m := newProxyManager(defaultProxyManagerConfig())
	launch, _, err := m.start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer m.stop()
	if len(launch) != 1 {
		t.Fatalf("startup launch set = %d proxies, want 1", len(launch))
	}

	proxyClear(parseArgsForTest(t, []string{"proxy", "clear"}))

	cancelled := make(chan struct{})
	r := newMinerProxyReloader(
		func() ([]*connect.ProxySettings, int, error) {
			launch, _, _, err := newProxyManager(defaultProxyManagerConfig()).desiredForReload()
			return launch, 0, err
		},
		func(*connect.ProxySettings) (context.CancelFunc, error) { return func() {}, nil },
	)
	r.seed(launch[0], func() { close(cancelled) })

	path, err := proxyReloadPath()
	if err != nil {
		t.Fatal(err)
	}
	for proxyReloadWatcherActive.Load() {
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.watch(ctx, path, 10*time.Millisecond)

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher swallowed the clear's trigger: the removed proxy is still running")
	}
}
