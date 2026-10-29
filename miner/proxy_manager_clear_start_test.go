package miner

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Clear takes the cross-process lock before it reads any store. While another
// process holds it (a startup mid-transaction), none of the three stores may
// change.
func TestClearWaitsForLockBeforeTouchingAnyStore(t *testing.T) {
	withTempHome(t)
	seedClearableStores(t)
	release, err := acquireProxyLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	finished := make(chan error, 1)
	go func() { finished <- clearProxyStores() }()
	select {
	case err := <-finished:
		t.Fatalf("clear finished while the proxy lock was held: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if n := len(readProxyConfig().Servers); n != 2 {
		t.Fatalf("clear emptied the internal config before it held the lock: %d servers left", n)
	}
	if urlState, err := readProxyURLState(); err != nil || len(urlState.Cache) != 1 {
		t.Fatalf("clear emptied the URL cache before it held the lock: %v %v", urlState, err)
	}
	release()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	assertStoresCleared(t)
}

// A startup that completes while clear waits for the lock marks the provider as
// running. Clear must read that after it holds the lock, or it skips the reload
// trigger and the launch set startup captured stays active.
func TestClearTriggersReloadForStartupThatLandsFirst(t *testing.T) {
	withTempHome(t)
	seedClearableStores(t)
	if err := writeProxyState(&ProxyState{Source: "/some/proxies.txt", NextID: 7}); err != nil {
		t.Fatal(err)
	}
	release, err := acquireProxyLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		proxyClear(parseArgsForTest(t, []string{"proxy", "clear"}))
	}()
	select {
	case <-finished:
		t.Fatal("clear finished while the proxy lock was held")
	case <-time.After(150 * time.Millisecond):
	}
	// Startup's transaction records the start under the lock it holds.
	if err := writeProxyState(&ProxyState{
		Source:    "/some/proxies.txt",
		StartedAt: time.Now(),
		NextID:    7,
		Proxies:   map[string]ProxyEntry{"1.1.1.1:1080": {ID: 3}},
	}); err != nil {
		t.Fatal(err)
	}
	release()
	<-finished

	reloadPath, err := proxyReloadPath()
	if err != nil {
		t.Fatal(err)
	}
	if seq, err := readReloadSeq(reloadPath); err != nil || seq != 1 {
		t.Fatalf("reload trigger seq = %d (%v), want 1", seq, err)
	}
}

// Run clear and startup against the real stores. Whichever wins, no store may
// hold a removed proxy afterwards.
func TestClearAndStartupNeverLeaveAMixedState(t *testing.T) {
	for i := 0; i < 25; i++ {
		withTempHome(t)
		writeProxyConfig(&ProxyConfig{Servers: map[string]string{"198.51.100.10:1080": ""}})
		if err := writeProxyState(&ProxyState{NextID: 4, Proxies: map[string]ProxyEntry{"198.51.100.10:1080": {ID: 3}}}); err != nil {
			t.Fatal(err)
		}
		if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{"198.51.100.11:1080": {ProbeOK: true}}}); err != nil {
			t.Fatal(err)
		}
		m := newProxyManager(defaultProxyManagerConfig())
		var wg sync.WaitGroup
		var startErr, clearErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, _, startErr = m.start(context.Background()) }()
		go func() { defer wg.Done(); clearErr = clearProxyStores() }()
		wg.Wait()
		if startErr != nil || clearErr != nil {
			t.Fatalf("start=%v clear=%v", startErr, clearErr)
		}
		m.stop()
		if cfg := readProxyConfig(); len(cfg.Servers) != 0 {
			t.Fatalf("iteration %d: internal config revived: %v", i, cfg.Servers)
		}
		if state, err := readProxyState(); err != nil || len(state.Proxies) != 0 {
			t.Fatalf("iteration %d: proxy.state revived: %v %v", i, state, err)
		}
		if urlState, err := readProxyURLState(); err != nil || len(urlState.Cache) != 0 {
			t.Fatalf("iteration %d: URL cache revived: %v %v", i, urlState, err)
		}
	}
}
