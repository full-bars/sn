package provider

import (
	"context"
	"os"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)


func TestRunProxyJWTWatcherRenewsOnExpiry(t *testing.T) {
	setRenewalTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	clientID := connect.NewId()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	expiring := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": clientID.String(),
		"exp":       float64(time.Now().Add(1 * time.Hour).Unix()),
	})
	if err := store.Put("proxy-a", clientJWTEntry{
		ByClientJWT: expiring,
		ClientID:    clientID.String(),
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy-a",
			ClientID:       clientID,
			Description:    "test [beta-test]",
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            NewRenewalOOB(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       make(chan struct{}, 1),
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	tick <- time.Now()

	deadline := time.After(5 * time.Second)
	for {
		entry, ok := store.Get("proxy-a")
		if ok && entry.ByClientJWT != expiring {
			break
		}
		select {
		case <-deadline:
			t.Fatal("watcher did not renew the expiring client JWT")
		case <-time.After(20 * time.Millisecond):
		}
	}

	entry, _ := store.Get("proxy-a")
	claims := decodeFakeJWTClaims(t, entry.ByClientJWT)
	if claims["client_id"] != clientID.String() {
		t.Fatalf("renewed JWT client_id = %v, want %q", claims["client_id"], clientID.String())
	}
	cancel()
	<-done
}

func TestRunProxyJWTWatcherRenewsOn401(t *testing.T) {
	setRenewalTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	clientID := connect.NewId()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	fresh := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": clientID.String(),
		"exp":       float64(time.Now().Add(30 * time.Hour).Unix()),
	})
	if err := store.Put("proxy-b", clientJWTEntry{
		ByClientJWT: fresh,
		ClientID:    clientID.String(),
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oob := NewRenewalOOB(ctx, connect.NewClientStrategyWithDefaults(ctx), "dead-jwt", ts.srv.URL)

	renewNow := make(chan struct{}, 1)
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy-b",
			ClientID:       clientID,
			Description:    "test [beta-test]",
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            oob,
			RenewNow:       renewNow,
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	if err := ts.forceOob401(oob); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for {
		entry, ok := store.Get("proxy-b")
		if ok && entry.ByClientJWT != fresh {
			break
		}
		select {
		case <-deadline:
			t.Fatal("watcher did not renew on 401 fast-path")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestRunProxyJWTWatcherRenewsExpiredAtStartup(t *testing.T) {
	setRenewalTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	clientID := connect.NewId()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	expired := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": clientID.String(),
		"exp":       float64(time.Now().Add(-1 * time.Hour).Unix()),
	})
	if err := store.Put("proxy-c", clientJWTEntry{
		ByClientJWT: expired,
		ClientID:    clientID.String(),
		NetworkID:   "net-1",
		MintedAt:    time.Now().Add(-25 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	renewNow := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy-c",
			ClientID:       clientID,
			Description:    "test [beta-test]",
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            NewRenewalOOB(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       renewNow,
			ProxyIndex:     0,
		})
	}()

	deadline := time.After(5 * time.Second)
	for {
		entry, ok := store.Get("proxy-c")
		if ok && entry.ByClientJWT != expired {
			break
		}
		select {
		case <-deadline:
			t.Fatal("watcher did not renew the already-expired token at startup")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestRunProxyJWTWatcherSkipsHealthyToken(t *testing.T) {
	setRenewalTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	clientID := connect.NewId()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	healthy := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": clientID.String(),
		"exp":       float64(time.Now().Add(30 * time.Hour).Unix()),
	})
	if err := store.Put("proxy-d", clientJWTEntry{
		ByClientJWT: healthy,
		ClientID:    clientID.String(),
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tick := make(chan time.Time)
	renewNow := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy-d",
			ClientID:       clientID,
			Description:    "test [beta-test]",
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            NewRenewalOOB(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       renewNow,
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	tick <- time.Now()
	time.Sleep(200 * time.Millisecond)

	entry, ok := store.Get("proxy-d")
	if !ok {
		t.Fatal("store entry missing")
	}
	if entry.ByClientJWT != healthy {
		t.Fatalf("healthy token was renewed — watcher must skip tokens outside the 12h window")
	}
	cancel()
	<-done
}

func TestRunProxyJWTWatcherKeepsOldTokenOnClientLimitExceeded(t *testing.T) {
	setRenewalTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

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

	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

	ts.scriptedResponse.Store(&connect.AuthNetworkClientResult{
		Error: &connect.AuthNetworkClientError{
			ClientLimitExceeded: true,
			Message:             "Client limit exceeded.",
		},
	})

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
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            NewRenewalOOB(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       make(chan struct{}, 1),
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	tick <- time.Now()
	time.Sleep(300 * time.Millisecond)

	entry, ok := store.Get("proxy")
	if !ok {
		t.Fatal("store entry missing after ClientLimitExceeded renewal failure")
	}
	if entry.ByClientJWT != old {
		t.Fatalf("store token changed after ClientLimitExceeded — must keep the old token for retry")
	}
	cancel()
	<-done
}

func TestRunProxyJWTWatcherMissingAccountJWTDoesNotPanic(t *testing.T) {
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("USERPROFILE", emptyHome)

	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

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

	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

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
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            NewRenewalOOB(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       make(chan struct{}, 1),
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	tick <- time.Now()
	time.Sleep(300 * time.Millisecond)

	entry, ok := store.Get("proxy")
	if !ok {
		t.Fatal("store entry missing")
	}
	if entry.ByClientJWT != old {
		t.Fatalf("store token changed — watcher must not renew without an account JWT")
	}
	cancel()
	<-done
}

func TestRunProxyJWTWatcherRejectsJwtMissingClientId(t *testing.T) {
	setRenewalTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

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

	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

	ts.omitClientIdClaim.Store(true)

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
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            NewRenewalOOB(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
			RenewNow:       make(chan struct{}, 1),
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	tick <- time.Now()
	time.Sleep(300 * time.Millisecond)

	entry, ok := store.Get("proxy")
	if !ok {
		t.Fatal("store entry missing")
	}
	if entry.ByClientJWT != old {
		t.Fatalf("store token changed — renewal returning a JWT without client_id must be rejected")
	}
	cancel()
	<-done
}

func TestRunProxyJWTWatcherRetriesOnStorePutFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("store-write-failure premise requires non-root (mode bits bypassed by UID 0)")
	}
	setRenewalTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	clientID := connect.NewId()
	roDir := t.TempDir()
	if err := os.Chmod(roDir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0700) })
	brokenStore := newClientJWTStore(roDir + "/client_jwts.json")
	oldStore := globalClientJWTStore
	globalClientJWTStore = brokenStore
	defer func() { globalClientJWTStore = oldStore }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oob := NewRenewalOOB(ctx, connect.NewClientStrategyWithDefaults(ctx), "dead-jwt", ts.srv.URL)
	renewNow := make(chan struct{}, 1)
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy",
			ClientID:       clientID,
			Description:    "test [beta-test]",
			ApiURL:         ts.srv.URL,
			ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
			OOB:            oob,
			RenewNow:       renewNow,
			Tick:           tick,
			ProxyIndex:     0,
		})
	}()

	if err := ts.forceOob401(oob); err != nil {
		t.Fatal(err)
	}

	baseline := ts.totalRequests.Load()
	renewNow <- struct{}{}
	ts.waitForRenewalRequest(t, baseline)
	first := ts.totalRequests.Load()

	baseline = ts.totalRequests.Load()
	renewNow <- struct{}{}
	ts.waitForRenewalRequest(t, baseline)
	second := ts.totalRequests.Load()
	if second <= first {
		t.Fatalf("401 counter was reset after a failed store write: requests %d -> %d — next 401 must re-trigger renewal", first, second)
	}
	cancel()
	<-done
}

func TestRunProxyJWTWatcherRenewsOnTransportAuthFailures(t *testing.T) {
	setRenewalTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	clientID := connect.NewId()
	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	healthy := createFakeJWTWithClaims(map[string]interface{}{
		"client_id": clientID.String(),
		"exp":       float64(time.Now().Add(30 * time.Hour).Unix()),
	})
	if err := store.Put("proxy-authfail", clientJWTEntry{
		ByClientJWT: healthy,
		ClientID:    clientID.String(),
		NetworkID:   "net-1",
		MintedAt:    time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

	const proxyIndex = 918273
	RegisterProxy(proxyIndex, "test-proxy-authfail-addr")
	defer UnregisterProxy(proxyIndex)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
			IdentityKey:    "proxy-authfail",
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

	for i := 0; i < revokedIdentityAuthFailureThreshold; i++ {
		RecordProxyAuthFailure(proxyIndex, errors.New("401 Unauthorized"))
	}

	tick <- time.Now()

	deadline := time.After(5 * time.Second)
	for {
		entry, ok := store.Get("proxy-authfail")
		if ok && entry.ByClientJWT != healthy {
			break
		}
		select {
		case <-deadline:
			t.Fatal("watcher did not renew on the transport auth-failure fast path")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestRunProxyJWTWatcherMutexSerializesRenewals(t *testing.T) {
	setRenewalTestHome(t)
	ts := newRenewalTestServer(t)
	defer ts.srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const n = 5
	ticks := make([]chan time.Time, n)
	done := make(chan struct{}, n)

	storePath := t.TempDir() + "/client_jwts.json"
	store := newClientJWTStore(storePath)
	oldStore := globalClientJWTStore
	globalClientJWTStore = store
	defer func() { globalClientJWTStore = oldStore }()

	for i := 0; i < n; i++ {
		clientID := connect.NewId()
		key := "proxy-" + string(rune('a'+i))
		ticks[i] = make(chan time.Time, 1)
		expiring := createFakeJWTWithClaims(map[string]interface{}{
			"client_id": clientID.String(),
			"exp":       float64(time.Now().Add(1 * time.Hour).Unix()),
		})
		if err := store.Put(key, clientJWTEntry{
			ByClientJWT: expiring,
			ClientID:    clientID.String(),
			NetworkID:   "net-1",
			MintedAt:    time.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		go func(key string, cid connect.Id, tick chan time.Time, idx int) {
			defer func() { done <- struct{}{} }()
			runProxyJWTWatcher(ctx, proxyJWTWatcherConfig{
				IdentityKey:    key,
				ClientID:       cid,
				Description:    "test [beta-test]",
				ApiURL:         ts.srv.URL,
				ClientStrategy: connect.NewClientStrategyWithDefaults(ctx),
				OOB:            NewRenewalOOB(ctx, connect.NewClientStrategyWithDefaults(ctx), "jwt", ts.srv.URL),
				RenewNow:       make(chan struct{}, 1),
				Tick:           tick,
				ProxyIndex:     idx,
			})
		}(key, clientID, ticks[i], i)
	}

	for i := 0; i < n; i++ {
		ticks[i] <- time.Now()
	}

	deadline := time.After(10 * time.Second)
	allRenewed := func() bool {
		for i := 0; i < n; i++ {
			key := "proxy-" + string(rune('a'+i))
			entry, ok := store.Get(key)
			if !ok {
				return false
			}
			if exp := parseJWTExpiryTime(entry.ByClientJWT); exp == nil || time.Until(*exp) < 20*time.Hour {
				return false
			}
		}
		return true
	}
	for !allRenewed() {
		select {
		case <-deadline:
			t.Fatal("not all watchers renewed their tokens")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	for i := 0; i < n; i++ {
		<-done
	}
}
