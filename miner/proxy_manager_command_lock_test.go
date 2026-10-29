package miner

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// A command that changes proxy source state must not touch it while another
// process holds the proxy lock. Each case holds the lock, starts the command,
// and checks the store is untouched until the lock is released.
func TestSourceStateCommandsWaitForTheProxyLock(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		run     func(t *testing.T, argv []string)
		changed func() bool
	}{
		{
			name: "exclude",
			argv: []string{"proxy", "exclude", "newpattern"},
			run:  func(t *testing.T, a []string) { proxyExclude(parseArgsForTest(t, a)) },
			changed: func() bool {
				st, _ := readProxyURLState()
				return len(st.ExcludePatterns) != 1
			},
		},
		{
			name: "exclude remove",
			argv: []string{"proxy", "exclude", "bad", "--remove"},
			run:  func(t *testing.T, a []string) { proxyExclude(parseArgsForTest(t, a)) },
			changed: func() bool {
				st, _ := readProxyURLState()
				return len(st.ExcludePatterns) != 1
			},
		},
		{
			name:    "auth add",
			argv:    []string{"proxy", "auth", "add", "k", "alice", "pw", "-f"},
			run:     func(t *testing.T, a []string) { proxyAuthAdd(parseArgsForTest(t, a)) },
			changed: func() bool { return len(readProxyConfig().Auths) != 1 },
		},
		{
			name:    "auth remove",
			argv:    []string{"proxy", "auth", "remove", "--all"},
			run:     func(t *testing.T, a []string) { proxyAuthRemove(parseArgsForTest(t, a)) },
			changed: func() bool { return len(readProxyConfig().Auths) != 1 },
		},
		{
			name:    "add",
			argv:    []string{"proxy", "add", "192.0.2.9:1080", "-f"},
			run:     func(t *testing.T, a []string) { proxyAdd(parseArgsForTest(t, a)) },
			changed: func() bool { return len(readProxyConfig().Servers) != 2 },
		},
		{
			name:    "remove",
			argv:    []string{"proxy", "remove", "1.1.1.1:1080"},
			run:     func(t *testing.T, a []string) { proxyRemove(parseArgsForTest(t, a)) },
			changed: func() bool { return len(readProxyConfig().Servers) != 2 },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTempHome(t)
			seedClearableStores(t)
			cfg := readProxyConfig()
			cfg.Auths = map[string]*ProxyAuth{"old": {User: "u", Password: "p"}}
			writeProxyConfig(cfg)
			// No selected source file, so add and remove use the internal config.
			if err := writeProxyState(&ProxyState{}); err != nil {
				t.Fatal(err)
			}
			release, err := acquireProxyLock()
			if err != nil {
				t.Fatal(err)
			}
			defer release()

			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.run(t, tc.argv)
			}()
			select {
			case <-done:
				t.Fatal("command finished while the proxy lock was held")
			case <-time.After(150 * time.Millisecond):
			}
			if tc.changed() {
				t.Fatal("command changed the store before it held the proxy lock")
			}
			release()
			<-done
			if !tc.changed() {
				t.Fatal("command did not apply its change once the lock was free")
			}
		})
	}
}

// An exclude racing a clear ends in one of two states, never a mix: the clear
// last (cache empty, pattern kept) or the exclude last on the cleared store
// (the same). A stale snapshot written back would leave the cache populated.
func TestExcludeRacingClearNeverRestoresTheCache(t *testing.T) {
	withTempHome(t)
	for i := 0; i < 40; i++ {
		cache := map[string]ProxyURLEntry{}
		for j := 0; j < 3000; j++ {
			cache[fmt.Sprintf("10.%d.%d.%d:1080", i%200, j/250, j%250)] = ProxyURLEntry{ProbeOK: true}
		}
		if err := writeProxyURLState(&ProxyURLState{
			Sources: []string{"https://example.com/list.txt"},
			Cache:   cache,
		}); err != nil {
			t.Fatal(err)
		}
		pattern := fmt.Sprintf("p%d", i)

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			proxyExclude(parseArgsForTest(t, []string{"proxy", "exclude", pattern}))
		}()
		go func() {
			defer wg.Done()
			<-start
			if err := clearProxyStores(); err != nil {
				t.Error(err)
			}
		}()
		close(start)
		wg.Wait()

		st, err := readProxyURLState()
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Cache) != 0 || len(st.Sources) != 0 {
			t.Fatalf("iteration %d: clear was undone: %d cache entries, sources %v", i, len(st.Cache), st.Sources)
		}
		found := false
		for _, p := range st.ExcludePatterns {
			found = found || p == pattern
		}
		if !found {
			t.Fatalf("iteration %d: exclude pattern %q lost: %v", i, pattern, st.ExcludePatterns)
		}
	}
}
