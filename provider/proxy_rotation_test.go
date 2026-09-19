//go:build linux

package provider

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"

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
	r := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{boot.Address: cancel},
		cancelMapMu: &sync.Mutex{},
		runningAuth: make(map[string]*connect.ProxySettings),
		state:       &ProxyState{Proxies: map[string]ProxyEntry{}},
		sourcePath:  file,
		parentCtx:   context.Background(),
		wg:          &sync.WaitGroup{},
		spawnProxy: func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
			<-proxyCtx.Done()
		},
		drainingProxies: map[string]context.CancelFunc{},
	}
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
	boot := &connect.ProxySettings{Network: "tcp", Address: "1.1.1.1:1080", Auth: &proxy.Auth{User: "alice", Password: "secret"}}
	r, cancelled := bootLaunchedReloader(t, writeProxyFile(t, "1.1.1.1:1080:alice:secret"), boot)
	r.seedRunningAuth([]*connect.ProxySettings{boot})

	r.reload()

	if n := cancelled.Load(); n != 0 {
		t.Fatalf("unchanged boot-launched proxy was cancelled %d time(s) by the first reload", n)
	}
}

// Pins the reason seeding must precede the first reload: an unseeded
// boot-launched proxy is treated as unknown and rotated.
func TestReload_UnseededBootProxy_IsRotated(t *testing.T) {
	boot := &connect.ProxySettings{Network: "tcp", Address: "1.1.1.1:1080", Auth: &proxy.Auth{User: "alice", Password: "secret"}}
	r, cancelled := bootLaunchedReloader(t, writeProxyFile(t, "1.1.1.1:1080:alice:secret"), boot)

	r.reload()

	if n := cancelled.Load(); n != 1 {
		t.Fatalf("expected the unseeded proxy to be cancelled once, got %d", n)
	}
}

// Re-pasting the same address with new credentials rotates the seeded proxy.
func TestReload_SeededBootProxy_RotatesOnCredentialChange(t *testing.T) {
	boot := &connect.ProxySettings{Network: "tcp", Address: "1.1.1.1:1080", Auth: &proxy.Auth{User: "alice", Password: "secret"}}
	r, cancelled := bootLaunchedReloader(t, writeProxyFile(t, "1.1.1.1:1080:alice:NEWPASS"), boot)
	r.seedRunningAuth([]*connect.ProxySettings{boot})

	r.reload()

	if n := cancelled.Load(); n != 1 {
		t.Fatalf("expected old goroutine cancelled once on credential change, got %d", n)
	}
	got, ok := r.runningAuthFor("1.1.1.1:1080")
	if !ok || got.Auth == nil || got.Auth.Password != "NEWPASS" {
		t.Fatalf("relaunched proxy must record the new credentials, got %+v ok=%v", got, ok)
	}
}

// A rotated proxy with active clients must be cancelled immediately, not
// drained: draining keeps the old credentials serving until the last client
// leaves, and the launch pass skips addresses that are still draining.
func TestReload_RotatedBusyProxy_IsNotDrained(t *testing.T) {
	const addr = "10.255.0.7:1080"
	boot := &connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: "alice", Password: "secret"}}
	r, cancelled := bootLaunchedReloader(t, writeProxyFile(t, addr+":alice:NEWPASS"), boot)
	r.seedRunningAuth([]*connect.ProxySettings{boot})

	RegisterProxy(987001, addr)
	bw := RegisterProxyBandwidth(987001)
	bw.Clients.Store(3) // active sessions on the old credentials

	r.reload()

	if n := cancelled.Load(); n != 1 {
		t.Fatalf("busy rotated proxy: expected old goroutine cancelled once, got %d", n)
	}
	if r.isDraining(addr) {
		t.Fatal("rotated proxy must not enter the draining state")
	}
	got, ok := r.runningAuthFor(addr)
	if !ok || got.Auth == nil || got.Auth.Password != "NEWPASS" {
		t.Fatalf("relaunched proxy must record the new credentials, got %+v ok=%v", got, ok)
	}
}
