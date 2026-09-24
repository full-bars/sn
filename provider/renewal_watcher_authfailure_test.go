package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestRunProxyJWTWatcherDoesNotRenewOnStaleAuthFailures pins the baseline
// comparison (H-4): ProxyAuthFailureCount is a CUMULATIVE lifetime counter,
// so failures that occurred BEFORE the watcher started (and are therefore
// already part of the baseline) must NOT by themselves trigger a renewal —
// only NEW failures since the baseline was captured count.
func TestRunProxyJWTWatcherDoesNotRenewOnStaleAuthFailures(t *testing.T) {
	setTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	clientID := connect.NewId()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	healthy := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": clientID.String(),
		"exp":       float64(time.Now().Add(30 * time.Hour).Unix()),
	})
	if err := store.Put("proxy-stale-authfail", clientJWTEntry{
		ByClientJWT: healthy,
		ClientID:    clientID.String(),
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	oldStore := loadGlobalClientJWTStore()
	storeGlobalClientJWTStore(store)
	defer func() { storeGlobalClientJWTStore(oldStore) }()

	const proxyIndex = 918274
	RegisterProxy(proxyIndex, "test-proxy-stale-authfail-addr", "test-proxy-stale-authfail-addr")
	defer UnregisterProxy(proxyIndex)

	// Failures recorded BEFORE the watcher starts become part of its
	// baseline snapshot.
	for i := 0; i < revokedIdentityAuthFailureThreshold; i++ {
		RecordProxyAuthFailure(proxyIndex, errors.New("401 Unauthorized"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy-stale-authfail",
			ClientID:       clientID,
			Description:    "test [beta-test]",
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            NewRenewalOOB(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       make(chan struct{}, 1),
			Tick:           tick,
			ProxyIndex:     proxyIndex,
		})
	}()

	tick <- time.Now()
	time.Sleep(300 * time.Millisecond)

	entry, ok := store.Get("proxy-stale-authfail")
	if !ok {
		t.Fatal("store entry missing")
	}
	if entry.ByClientJWT != healthy {
		t.Fatalf("watcher renewed on pre-existing (stale) auth failures — baseline comparison must ignore failures that predate the watcher's startup snapshot")
	}
	cancel()
	<-done
}
