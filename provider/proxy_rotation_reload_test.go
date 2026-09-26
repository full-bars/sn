package provider

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect"
)

func emptyReloader(t *testing.T, sourcePath string) *ProxyReloader {
	t.Helper()
	withTempHome(t)
	proxyWarmupDone.Store(true)
	t.Cleanup(func() { proxyWarmupDone.Store(false) })

	parent, cancelParent := context.WithCancel(context.Background())
	r := &ProxyReloader{
		cancelMap:       map[string]context.CancelFunc{},
		cancelMapMu:     &sync.Mutex{},
		runningAuth:     make(map[string]*connect.ProxySettings),
		state:           &ProxyState{Proxies: map[string]ProxyEntry{}},
		sourcePath:      sourcePath,
		parentCtx:       parent,
		wg:              &sync.WaitGroup{},
		spawnProxy:      func(context.Context, *connect.ProxySettings, bool, bool) { <-parent.Done() },
		drainingProxies: map[string]context.CancelFunc{},
	}
	t.Cleanup(func() { cancelParent(); r.wg.Wait() })
	return r
}

// A direct-only node (no proxy source configured) reads a VALID, settled
// zero-proxy state, not "empty" degraded. This is the fix for the direct-only
// node staying degraded forever: a deliberate direct-only config is not a
// source that was queried and came back empty.
func TestReload_DirectOnlyNode_SettlesZeroValid(t *testing.T) {
	resetProxyCounters(t)

	// sourcePath "" = no --proxy_file source; no proxy_url.json in the temp
	// home = no URL sources. So anySourceConfigured is false and the reload
	// records the valid-zero (direct-only) state.
	r := emptyReloader(t, "")
	r.reload()

	if got := proxyResolutionStatus.Load(); got != proxyResolutionZeroValid {
		t.Fatalf("direct-only reload: resolution=%d, want proxyResolutionZeroValid(%d)", got, proxyResolutionZeroValid)
	}
	if phase := proxyStartupPhase(); phase != "" {
		t.Fatalf("direct-only node must settle (empty startup phase), got %q", phase)
	}
	if line := systemdStatusLine(); !strings.HasPrefix(line, "active:") {
		t.Fatalf("direct-only node must read active, got %q", line)
	}
}

// With the direct transport turned off and no proxy source configured,
// nothing is served, so the node must not settle as a healthy direct-only
// node: it reads degraded (empty), not active.
func TestReload_DirectOffNoSource_StillReadsEmpty(t *testing.T) {
	resetProxyCounters(t)

	t.Setenv("DISABLE_DIRECT_IP", "1") // direct transport off
	r := emptyReloader(t, "")
	r.reload()

	if got := proxyResolutionStatus.Load(); got != proxyResolutionNoSource {
		t.Fatalf("direct-off no-source reload: resolution=%d, want proxyResolutionNoSource(%d)", got, proxyResolutionNoSource)
	}
	if phase := proxyStartupPhase(); phase != startupNoSource {
		t.Fatalf("direct-off no-source node must read the no-source phase, got %q", phase)
	}
	if line := systemdStatusLine(); !strings.Contains(line, "degraded") || strings.Contains(line, "active:") {
		t.Fatalf("direct-off no-source node must NOT read active while serving nothing, got %q", line)
	}
	if line := systemdStatusLine(); !strings.Contains(line, "direct transport is off") {
		t.Fatalf("direct-off no-source line must name the missing direct transport, got %q", line)
	}
}

// A source that WAS configured and has gone empty must stop the proxies it
// used to supply, and must zero the configured count. Without this the old
// proxies keep dialling and the stale positive count makes the status line
// and startup phase ignore the empty-source resolution.
func TestReload_EmptySource_StopsRunningProxies(t *testing.T) {
	resetProxyCounters(t)

	// A source that used to supply a proxy, now empty.
	r := emptyReloader(t, writeProxyFile(t, "# empty"))
	// Seed one running proxy the way the startup loop would have.
	var cancelled atomic.Int32
	boot := &connect.ProxySettings{Address: "gone.example:1"}
	r.cancelMap[boot.Address] = func() { cancelled.Add(1) }
	r.runningAuth[boot.Address] = boot
	r.state.Proxies[boot.Address] = ProxyEntry{Source: "file"}
	setConfiguredProxyCount(1)

	r.reload()

	if got := cancelled.Load(); got != 1 {
		t.Fatalf("running proxies stopped = %d, want 1: an empty source must not keep dialling", got)
	}
	if _, still := r.cancelMap[boot.Address]; still {
		t.Fatalf("proxy %s still in the cancel map after the source went empty", boot.Address)
	}
	if _, still := r.state.Proxies[boot.Address]; still {
		t.Fatalf("proxy %s still in proxy.state after the source went empty", boot.Address)
	}
	if n := proxiesConfigured.Load(); n != 0 {
		t.Fatalf("configured count = %d, want 0: a stale positive count hides the empty source", n)
	}
	if got := proxyResolutionStatus.Load(); got != proxyResolutionEmpty {
		t.Fatalf("resolution=%d, want proxyResolutionEmpty(%d)", got, proxyResolutionEmpty)
	}
}

// A proxy source that WAS configured but yielded zero proxies still reads
// degraded (empty), even though the node may run direct alongside it — it is
// not a deliberate direct-only config.
func TestReload_EmptySource_StillReadsEmpty(t *testing.T) {
	resetProxyCounters(t)

	// A configured source with only blank lines = a source that returned zero
	// proxies (configured, so not direct-only).
	r := emptyReloader(t, writeProxyFile(t, "# empty"))
	r.reload()

	if got := proxyResolutionStatus.Load(); got != proxyResolutionEmpty {
		t.Fatalf("empty-source reload: resolution=%d, want proxyResolutionEmpty(%d)", got, proxyResolutionEmpty)
	}
	if phase := proxyStartupPhase(); phase != startupSourceEmpty {
		t.Fatalf("empty-source node must read empty/degraded, got %q", phase)
	}
}
