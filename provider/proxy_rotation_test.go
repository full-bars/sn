//go:build linux

package provider

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

func TestSameAuth(t *testing.T) {
	a := &connect.ProxySettings{Auth: &proxy.Auth{User: "u", Password: "p"}}
	if !sameAuth(a, &connect.ProxySettings{Auth: &proxy.Auth{User: "u", Password: "p"}}) {
		t.Fatal("identical credentials must compare equal")
	}
	if sameAuth(a, &connect.ProxySettings{Auth: &proxy.Auth{User: "u", Password: "q"}}) {
		t.Fatal("changed password must compare different")
	}
	if sameAuth(a, &connect.ProxySettings{Auth: &proxy.Auth{User: "v", Password: "p"}}) {
		t.Fatal("changed user must compare different")
	}
	if sameAuth(a, &connect.ProxySettings{}) {
		t.Fatal("auth vs no auth must compare different")
	}
}

// bootLaunchedReloader builds a reloader whose one proxy was launched by the
// startup loop (present in cancelMap) against the given proxy file, and
// returns a counter of how many times that proxy's goroutine was cancelled.
func bootLaunchedReloader(t *testing.T, file string, boot *connect.ProxySettings) (*ProxyReloader, *atomic.Int32) {
	t.Helper()
	withTempHome(t)
	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	var cancelled atomic.Int32
	cancel := context.CancelFunc(func() { cancelled.Add(1) })
	parent, cancelParent := context.WithCancel(context.Background())
	r := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{boot.Key(): cancel},
		cancelMapMu: &sync.Mutex{},
		runningAuth: make(map[string]*connect.ProxySettings),
		state:       &ProxyState{Proxies: map[string]ProxyEntry{}},
		sourcePath:  file,
		parentCtx:   parent,
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}
	// Replacement goroutines block in spawnProxy until their context is
	// cancelled; stop them and wait, so their deferred unregistration runs and
	// nothing outlives the test.
	t.Cleanup(func() {
		cancelParent()
		r.wg.Wait()
	})
	return r, &cancelled
}

func writeProxyFile(t *testing.T, line string) string {
	t.Helper()
	f := t.TempDir() + "/proxy.txt"
	if err := os.WriteFile(f, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

// A seeded, boot-launched proxy whose credentials are unchanged must survive
// the first reload untouched.
func TestReload_SeededBootProxy_NotRestarted(t *testing.T) {
	boot := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080", Auth: &proxy.Auth{User: "alice", Password: "secret"}}
	r, cancelled := bootLaunchedReloader(t, writeProxyFile(t, "192.0.2.1:1080:alice:secret"), boot)
	r.seedRunningAuth([]*connect.ProxySettings{boot})

	r.reload()

	if n := cancelled.Load(); n != 0 {
		t.Fatalf("unchanged boot-launched proxy was cancelled %d time(s) by the first reload", n)
	}
}

// Pins the reason seeding must precede the first reload: an unseeded
// boot-launched proxy is treated as unknown and rotated.
func TestReload_UnseededBootProxy_IsRotated(t *testing.T) {
	boot := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080", Auth: &proxy.Auth{User: "alice", Password: "secret"}}
	r, cancelled := bootLaunchedReloader(t, writeProxyFile(t, "192.0.2.1:1080:alice:secret"), boot)

	r.reload()

	if n := cancelled.Load(); n != 1 {
		t.Fatalf("expected the unseeded proxy to be cancelled once, got %d", n)
	}
}

// Re-pasting the same address with new credentials rotates the seeded proxy.
func TestReload_SeededBootProxy_RotatesOnCredentialChange(t *testing.T) {
	boot := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080", Auth: &proxy.Auth{User: "alice", Password: "secret"}}
	r, cancelled := bootLaunchedReloader(t, writeProxyFile(t, "192.0.2.1:1080:alice:NEWPASS"), boot)
	r.seedRunningAuth([]*connect.ProxySettings{boot})

	r.reload()

	if n := cancelled.Load(); n != 1 {
		t.Fatalf("expected old goroutine cancelled once on credential change, got %d", n)
	}
	got, ok := r.runningAuthFor(boot.Key())
	if !ok || got.Auth == nil || got.Auth.Password != "NEWPASS" {
		t.Fatalf("relaunched proxy must record the new credentials, got %+v ok=%v", got, ok)
	}
}

// A rotated proxy with active clients must be cancelled immediately, not
// drained: draining keeps the old credentials serving until the last client
// leaves, and the launch pass skips addresses that are still draining.
func TestReload_RotatedBusyProxy_IsNotDrained(t *testing.T) {
	const addr = "192.0.2.7:1080"
	boot := &connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: "alice", Password: "secret"}}
	r, cancelled := bootLaunchedReloader(t, writeProxyFile(t, addr+":alice:NEWPASS"), boot)
	r.seedRunningAuth([]*connect.ProxySettings{boot})

	RegisterProxy(987001, addr, boot.Key())
	t.Cleanup(func() { UnregisterProxy(987001) })
	bw := RegisterProxyBandwidth(987001)
	bw.Clients.Store(3) // active sessions on the old credentials

	r.reload()

	if n := cancelled.Load(); n != 1 {
		t.Fatalf("busy rotated proxy: expected old goroutine cancelled once, got %d", n)
	}
	if r.isDraining(boot.Key()) {
		t.Fatal("rotated proxy must not enter the draining state")
	}
	got, ok := r.runningAuthFor(boot.Key())
	if !ok || got.Auth == nil || got.Auth.Password != "NEWPASS" {
		t.Fatalf("relaunched proxy must record the new credentials, got %+v ok=%v", got, ok)
	}
}

// Rotating credentials relaunches the proxy in the same pass; it must keep its
// state entry so the relaunch reuses the stable ID and persisted health.
func TestReload_RotatedProxy_KeepsStateEntry(t *testing.T) {
	const addr = "192.0.2.20:1080"
	boot := &connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: "test-user", Password: "test-pass"}}
	r, _ := bootLaunchedReloader(t, writeProxyFile(t, addr+":test-user:rotated-pass"), boot)
	r.seedRunningAuth([]*connect.ProxySettings{boot})

	state := &ProxyState{Proxies: map[string]ProxyEntry{addr: {ID: 7, Health: "up"}}}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	r.state = state

	r.reload()

	after, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	// The seeded legacy entry (bare address) is adopted to the identity key
	// (address+user) by the reload's migration, preserving ID and health.
	kept, ok := after.Proxies[boot.Key()]
	if !ok {
		t.Fatalf("rotated proxy lost its state entry: no entry at identity key %q (legacy bare-address seed %q)", boot.Key(), addr)
	}
	if kept.ID != 7 || kept.Health != "up" {
		t.Fatalf("rotated proxy must keep ID 7 and health up, got ID=%d health=%q", kept.ID, kept.Health)
	}
}

// A goroutine from a superseded launch must not delete the replacement's
// cancel-map entry.
func TestDeleteProxyCancelIfCurrent(t *testing.T) {
	const addr = "192.0.2.31:1080"
	mu := &sync.Mutex{}
	cancelMap := map[string]context.CancelFunc{addr: func() {}}

	oldCtx := withProxyLaunchGen(context.Background(), beginProxyLaunch(addr))
	newCtx := withProxyLaunchGen(context.Background(), beginProxyLaunch(addr))
	t.Cleanup(func() { proxyLaunches.mu.Lock(); delete(proxyLaunches.current, addr); proxyLaunches.mu.Unlock() })

	deleteProxyCancelIfCurrent(mu, cancelMap, oldCtx, addr)
	if _, ok := cancelMap[addr]; !ok {
		t.Fatal("stale launch deleted the replacement's cancel-map entry")
	}
	if proxyOwnsLaunch(oldCtx, addr) || !proxyOwnsLaunch(newCtx, addr) {
		t.Fatal("ownership must follow the latest launch")
	}
	deleteProxyCancelIfCurrent(mu, cancelMap, newCtx, addr)
	if _, ok := cancelMap[addr]; ok {
		t.Fatal("current launch must be able to delete its own entry")
	}

	cancelMap[addr] = func() {}
	deleteProxyCancelIfCurrent(mu, cancelMap, context.Background(), addr)
	if _, ok := cancelMap[addr]; ok {
		t.Fatal("a context without a generation keeps the unconditional delete")
	}
}

// resetReloadTriggerForTest isolates the process-global reload-trigger
// debounce: proxyAdd writes the trigger, and a trailing time.AfterFunc from
// one test must not recreate files under another test's HOME or suppress its
// trigger writes.
func resetReloadTriggerForTest(t *testing.T) {
	t.Helper()
	lastReloadTriggerTime.Lock()
	oldDebounce := writeReloadTriggerDebounce
	oldTS, oldPending := lastReloadTriggerTime.ts, lastReloadTriggerTime.pending
	writeReloadTriggerDebounce = 0
	lastReloadTriggerTime.ts = time.Time{}
	lastReloadTriggerTime.pending = false
	lastReloadTriggerTime.Unlock()
	t.Cleanup(func() {
		lastReloadTriggerTime.Lock()
		writeReloadTriggerDebounce = oldDebounce
		lastReloadTriggerTime.ts, lastReloadTriggerTime.pending = oldTS, oldPending
		lastReloadTriggerTime.Unlock()
	})
}

func writeProxyConfigForTest(t *testing.T, servers map[string]string, auths map[string]*ProxyAuth) {
	t.Helper()
	cfg := readProxyConfig()
	cfg.Servers = servers
	cfg.Auths = auths
	writeProxyConfig(cfg)
}

// proxyAdd removes an existing same-identity entry with a different password so
// a re-paste becomes a rotation instead of a silent duplicate. A DIFFERENT user
// at the same address is a separate account and is left alone.
func TestProxyAddRotatesCredentials(t *testing.T) {
	withTempHome(t)
	resetReloadTriggerForTest(t)
	writeProxyConfigForTest(t, map[string]string{
		"192.0.2.4:1080:olduser:oldpass": "",
		"192.0.2.9:1080":                 "",
	}, nil)

	proxyAdd(docopt.Opts{"<key_address>": []string{"192.0.2.4:1080:olduser:newpass"}, "-f": true})

	got := readProxyConfig()
	if _, ok := got.Servers["192.0.2.4:1080:olduser:newpass"]; !ok {
		t.Fatalf("new credential entry missing: %v", got.Servers)
	}
	if _, ok := got.Servers["192.0.2.4:1080:olduser:oldpass"]; ok {
		t.Fatalf("old password for the same identity was not rotated away: %v", got.Servers)
	}
	if _, ok := got.Servers["192.0.2.9:1080"]; !ok {
		t.Fatalf("unrelated proxy must survive: %v", got.Servers)
	}
}

// A different user at the same gateway address is a DIFFERENT account, so
// adding it must not purge the existing account.
func TestProxyAdd_DifferentUserAtSharedGatewayIsNotARotation(t *testing.T) {
	withTempHome(t)
	resetReloadTriggerForTest(t)
	writeProxyConfigForTest(t, map[string]string{
		"192.0.2.4:1080:alice:secret": "",
	}, nil)

	proxyAdd(docopt.Opts{"<key_address>": []string{"192.0.2.4:1080:bob:other"}, "-f": true})

	got := readProxyConfig()
	if _, ok := got.Servers["192.0.2.4:1080:alice:secret"]; !ok {
		t.Fatalf("adding a second account purged the first: %v", got.Servers)
	}
	if _, ok := got.Servers["192.0.2.4:1080:bob:other"]; !ok {
		t.Fatalf("the new account was not added: %v", got.Servers)
	}
}

// An existing entry whose credentials come from the Auths table is the same
// credential as an inline user:pass form of the same address; adding it is not
// a rotation and must not purge the existing mapping.
func TestProxyAddKeepsEntryWithSameEffectiveCredentials(t *testing.T) {
	withTempHome(t)
	resetReloadTriggerForTest(t)
	writeProxyConfigForTest(t,
		map[string]string{"192.0.2.4:1080": "k1"},
		map[string]*ProxyAuth{"k1": {User: "alice", Password: "secret"}})

	proxyAdd(docopt.Opts{"<key_address>": []string{"192.0.2.4:1080:alice:secret"}, "-f": true})

	got := readProxyConfig()
	if _, ok := got.Servers["192.0.2.4:1080"]; !ok {
		t.Fatalf("entry with the same effective credentials was purged: %v", got.Servers)
	}
}

// End-to-end rotation with the reloader's real goroutines: after the
// superseded goroutine unwinds, the replacement must still be registered,
// running under the rotated credentials, and present in the cancel map.
func TestReload_RotationWithRealGoroutines_KeepsRegistration(t *testing.T) {
	const addr = "192.0.2.40:1080"
	// The config is credentialed ("addr:test-user:..."), so the reload engine
	// keys every structure by identity (address+user), never the bare address.
	key := (&connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: "test-user"}}).Key()
	withTempHome(t)
	// Production seeds the ID counter at startup so ID 0 stays reserved for
	// [direct]. Without it, run in isolation the first proxy is handed ID 0
	// and races the direct goroutine's RegisterProxy(0, "direct"), which
	// overwrites this proxy's health entry.
	initProxyIDCounter(0)
	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	file := writeProxyFile(t, addr+":test-user:test-pass")
	parent, cancelParent := context.WithCancel(context.Background())
	r := &ProxyReloader{
		cancelMap:       map[string]context.CancelFunc{},
		cancelMapMu:     &sync.Mutex{},
		runningAuth:     map[string]*connect.ProxySettings{},
		state:           &ProxyState{Proxies: map[string]ProxyEntry{}},
		sourcePath:      file,
		parentCtx:       parent,
		wg:              &sync.WaitGroup{},
		drainingProxies: map[string]context.CancelFunc{},
		spawnProxy: func(ctx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-ctx.Done()
		},
	}
	t.Cleanup(func() {
		cancelParent()
		r.wg.Wait()
		if _, registered := ProxyHealthByKey()[key]; registered {
			UnregisterProxy(r.state.Proxies[key].ID)
		}
	})

	r.reload() // launch
	if _, ok := r.state.Proxies[key]; !ok {
		t.Fatal("first reload did not create a state entry")
	}
	if err := os.WriteFile(file, []byte(addr+":test-user:rotated-pass\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r.reload() // rotate: cancels the first goroutine, relaunches under the same ID

	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, registered := ProxyHealthByKey()[key]; !registered {
			t.Fatal("the superseded goroutine's exit removed the replacement's health registration")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.cancelMapMu.Lock()
	_, running := r.cancelMap[key]
	r.cancelMapMu.Unlock()
	if !running {
		t.Fatal("replacement is missing from the cancel map")
	}
	got, ok := r.runningAuthFor(key)
	if !ok || got.Auth == nil || got.Auth.Password != "rotated-pass" {
		t.Fatalf("replacement must run with the rotated credentials, got %+v ok=%v", got, ok)
	}
}

// Regression: an existing entry with the same EFFECTIVE credentials (held in
// the Auths table under a differently spelled key) made proxyAdd skip the add
// at the first such entry it saw, so a stale duplicate for the same address
// survived or was purged depending on Go's random map order. Every duplicate
// must be scanned first. Repeated to cover the iteration orders.
// An existing entry with the same effective credentials is not a rotation and
// must not be purged. Stale duplicates of the SAME identity (same user, other
// passwords) are still purged in a stable order.
func TestProxyAddPurgesStaleDuplicatesWhenSameCredentialsExist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	resetReloadTriggerForTest(t)
	if err := os.MkdirAll(filepath.Join(dir, ".urnetwork"), 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		cfg := readProxyConfig()
		cfg.Servers = map[string]string{
			"192.0.2.4:1080":           "k1",
			"192.0.2.4:1080:alice:old": "",
			"192.0.2.4:1080:alice:zzz": "",
			"192.0.2.4:1080:bob:other": "", // other account: untouched
			"192.0.2.9:1080":           "",
		}
		cfg.Auths = map[string]*ProxyAuth{"k1": {User: "alice", Password: "secret"}}
		writeProxyConfig(cfg)

		proxyAdd(docopt.Opts{
			"<key_address>": []string{"192.0.2.4:1080:alice:secret"},
			"-f":            true,
		})

		got := readProxyConfig()
		if _, ok := got.Servers["192.0.2.4:1080"]; !ok {
			t.Fatalf("iteration %d: entry with the same effective credentials was lost: %v", i, got.Servers)
		}
		for _, stale := range []string{"192.0.2.4:1080:alice:old", "192.0.2.4:1080:alice:zzz"} {
			if _, ok := got.Servers[stale]; ok {
				t.Fatalf("iteration %d: stale duplicate %q survived: %v", i, stale, got.Servers)
			}
		}
		if _, ok := got.Servers["192.0.2.4:1080:bob:other"]; !ok {
			t.Fatalf("iteration %d: the other account at the gateway must survive: %v", i, got.Servers)
		}
		if len(got.Servers) != 3 {
			t.Fatalf("iteration %d: want the kept entry, the new account, and the unrelated one, got %v", i, got.Servers)
		}
	}
}
