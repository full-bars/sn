package miner

import (
	"context"
	"testing"
	"time"
)

// A clear transaction holds the process lock while startup attempts its own
// read and write. Startup must not read the rows that the clear is removing.
func TestProxyStartupWaitsForClearTransaction(t *testing.T) {
	withTempHome(t)
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{"198.51.100.10:1080": {ID: 1}}}); err != nil {
		t.Fatal(err)
	}
	release, err := acquireProxyLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	m := newProxyManager(defaultProxyManagerConfig())
	finished := make(chan error, 1)
	go func() { _, _, err := m.start(context.Background()); finished <- err }()
	select {
	case err := <-finished:
		if err == nil {
			m.stop()
		}
		t.Fatalf("startup completed while clear held the proxy lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	// This is the state-writing part of clearProxyStores, while holding the
	// same cross-process lock that its command acquires.
	state, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	state.Proxies = map[string]ProxyEntry{}
	state.NextID = 0
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	defer m.stop()
	state, err = readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Proxies) != 0 {
		t.Fatalf("startup resurrected cleared rows: %v", state.Proxies)
	}
}
