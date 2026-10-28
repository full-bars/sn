package miner

import "testing"

// A live-apply read rewrites proxy.state, so it must wait on the same
// cross-process lock `proxy clear` holds. Without it a clear that lands between
// the read and the write is undone by the stale snapshot.
func TestDesiredForReloadRefusesWhileProxyLockHeld(t *testing.T) {
	withTempHome(t)

	release, err := acquireProxyLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	m := newProxyManager(defaultProxyManagerConfig())
	if _, _, _, err := m.desiredForReload(); err == nil {
		t.Fatal("desiredForReload applied state while another process held the proxy lock")
	}
	// The command readers never write, so they stay lock-free.
	if _, _, _, err := m.desired(false); err != nil {
		t.Fatalf("a plain read must not need the proxy lock: %v", err)
	}
}
