package provider

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestDirectOffBeforeAnyReload_WaitsForStartupDirect: the startup direct
// goroutine published no done channel, so a `direct off` handled by the
// first reload cancelled it and unregistered proxy[0] without waiting,
// leaving a still-tearing-down direct transport running unregistered.
func TestDirectOffBeforeAnyReload_WaitsForStartupDirect(t *testing.T) {
	withTempHome(t)
	ResetProxyHealthForTesting()
	t.Cleanup(ResetProxyHealthForTesting)
	t.Setenv("DISABLE_DIRECT_IP", "")
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &provideState{ctx: ctx, proxyCancelMap: map[string]context.CancelFunc{}}

	// The fake direct transport takes a moment to tear down after cancel.
	var exited atomic.Bool
	done := startDirectTransport(st, func(nativeCtx context.Context) {
		<-nativeCtx.Done()
		time.Sleep(200 * time.Millisecond)
		exited.Store(true)
	})

	if err := writeDirectEnabled(false); err != nil {
		t.Fatal(err)
	}
	reloader := &ProxyReloader{
		cancelMap:       st.proxyCancelMap,
		cancelMapMu:     &st.proxyCancelMu,
		runningAuth:     map[string]*connect.ProxySettings{},
		state:           &ProxyState{Proxies: map[string]ProxyEntry{}},
		parentCtx:       ctx,
		wg:              &sync.WaitGroup{},
		spawnProxy:      func(context.Context, *connect.ProxySettings, bool, bool) {},
		drainingProxies: map[string]context.CancelFunc{},
		directDone:      done,
	}
	reloader.reload()

	if !exited.Load() {
		t.Fatal("reload returned from direct off before the startup direct goroutine exited")
	}
	st.wg.Wait()
}
