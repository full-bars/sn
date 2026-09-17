package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestRunProxyJWTWatcherKeepsOldTokenOnFailure verifies a failed renewal
// leaves the store untouched (old token still cached) so the hourly retry can
// try again — no data loss, no panic.
func TestRunProxyJWTWatcherKeepsOldTokenOnFailure(t *testing.T) {
	setTestHome(t)
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/network/auth-client" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer failSrv.Close()

	clientID := connect.NewId()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	old := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": clientID.String(),
		"exp":       float64(time.Now().Add(1 * time.Hour).Unix()),
	})
	if err := store.Put("proxy", clientJWTEntry{
		ByClientJWT: old,
		ClientID:    clientID.String(),
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	oldStore := loadGlobalClientJWTStore()
	storeGlobalClientJWTStore(store)
	defer func() { storeGlobalClientJWTStore(oldStore) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy",
			ClientID:       clientID,
			Description:    "test [beta-test]",
			ApiURL:         failSrv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            NewRenewalOOB(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", failSrv.URL),
			RenewNow:       make(chan struct{}, 1),
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	tick <- time.Now()
	time.Sleep(200 * time.Millisecond)

	entry, ok := store.Get("proxy")
	if !ok {
		t.Fatal("store entry missing after failed renewal")
	}
	if entry.ByClientJWT != old {
		t.Fatalf("store token changed after failed renewal — must keep the old token for retry")
	}
	cancel()
	<-done
}
