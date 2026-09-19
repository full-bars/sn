package provider

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// TestSameAuth distinguishes identical vs changed credentials — the decision
// function behind proxy credential rotation.
func TestSameAuth(t *testing.T) {
	t.Run("both nil", func(t *testing.T) {
		a := &connect.ProxySettings{}
		b := &connect.ProxySettings{}
		if !sameAuth(a, b) {
			t.Fatal("two nil-auth settings must compare equal")
		}
	})
	t.Run("nil vs set is different", func(t *testing.T) {
		a := &connect.ProxySettings{}
		b := &connect.ProxySettings{Auth: &proxy.Auth{User: "u", Password: "p"}}
		if sameAuth(a, b) {
			t.Fatal("nil auth vs set auth must differ")
		}
	})
	t.Run("same credentials equal", func(t *testing.T) {
		a := &connect.ProxySettings{Auth: &proxy.Auth{User: "sppmr4vcnj", Password: "naIi5=EuO4ns5Fis5h"}}
		b := &connect.ProxySettings{Auth: &proxy.Auth{User: "sppmr4vcnj", Password: "naIi5=EuO4ns5Fis5h"}}
		if !sameAuth(a, b) {
			t.Fatal("identical credentials must compare equal")
		}
	})
	t.Run("credential rotation detected", func(t *testing.T) {
		oldCred := &connect.ProxySettings{Auth: &proxy.Auth{User: "sppmr4vcnj", Password: "old-pass"}}
		newCred := &connect.ProxySettings{Auth: &proxy.Auth{User: "user-sppmr4vcnj-country-us", Password: "new-pass"}}
		if sameAuth(oldCred, newCred) {
			t.Fatal("different credentials must not compare equal (LA7 paste incident)")
		}
	})
}

// TestSameAuth_TopLevelNilGuards verifies top-level nil pointer handling in sameAuth.
func TestSameAuth_TopLevelNilGuards(t *testing.T) {
	nonNil := &connect.ProxySettings{
		Auth: &proxy.Auth{User: "user", Password: "pass"},
	}

	t.Run("nil vs non-nil", func(t *testing.T) {
		if sameAuth(nil, nonNil) {
			t.Fatal("nil vs non-nil *ProxySettings must not compare equal")
		}
	})

	t.Run("non-nil vs nil", func(t *testing.T) {
		if sameAuth(nonNil, nil) {
			t.Fatal("non-nil vs nil *ProxySettings must not compare equal")
		}
	})

	t.Run("both nil", func(t *testing.T) {
		if !sameAuth(nil, nil) {
			t.Fatal("both nil (*ProxySettings) must compare equal")
		}
	})
}

// TestProxyAddRotatesCredentials verifies that proxyAdd purges any existing entry
// for the same host:port when new credentials are provided, replacing the old entry
// in the Servers map (keyed by full proxyAddress string host:port:user:pass).
func TestProxyAddRotatesCredentials(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Write initial config with old credentials for 1.2.3.4:1080
	initialProxy := "1.2.3.4:1080:olduser:oldpass"
	cfg := ProxyConfig{
		Servers: map[string]string{
			initialProxy: "",
		},
	}
	writeProxyConfig(&cfg)

	// Re-paste with new credentials for the same host:port under -f (force)
	newProxy := "1.2.3.4:1080:newuser:newpass"
	opts := docopt.Opts{
		"<key_address>": []string{newProxy},
		"-f":            true,
	}
	proxyAdd(opts)

	got := readProxyConfig()
	if _, err := os.Stat(expandPath("~/.urnetwork/proxy")); err != nil {
		t.Fatalf("expected proxy config file on disk: %v", err)
	}
	if len(got.Servers) != 1 {
		t.Fatalf("want 1 server after rotation, got %d: %v", len(got.Servers), got.Servers)
	}
	if _, ok := got.Servers[newProxy]; !ok {
		t.Fatalf("expected new entry %s in servers, got: %v", newProxy, got.Servers)
	}
	if _, ok := got.Servers[initialProxy]; ok {
		t.Fatalf("expected old entry %s to be purged from servers", initialProxy)
	}

	settings := readProxySettings()
	if len(settings) != 1 {
		t.Fatalf("want 1 ProxySettings returned, got %d", len(settings))
	}
	s := settings[0]
	parsedAddr, _, _ := parseProxyAddress(newProxy)
	if s.Address != parsedAddr {
		t.Fatalf("expected address %s, got %s", parsedAddr, s.Address)
	}
	if s.Auth == nil {
		t.Fatal("expected non-nil Auth on rotated proxy")
	}
	if s.Auth.User != "newuser" || s.Auth.Password != "newpass" {
		t.Fatalf("expected newuser/newpass, got %s/%s", s.Auth.User, s.Auth.Password)
	}
}

// TestProxyAddMultiProxyInlineCreds verifies that pasting multiple proxies with different
// inline credentials preserves the distinct credentials for each proxy without clobbering.
func TestProxyAddMultiProxyInlineCreds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	proxies := []string{
		"1.1.1.1:1080:userA:passA",
		"2.2.2.2:1080:userB:passB",
		"3.3.3.3:1080",
	}

	opts := docopt.Opts{
		"<key_address>": proxies,
		"-f":            true,
	}
	proxyAdd(opts)

	settings := readProxySettings()
	if len(settings) != 3 {
		t.Fatalf("expected 3 settings returned, got %d", len(settings))
	}

	byAddr := make(map[string]*connect.ProxySettings, len(settings))
	for _, s := range settings {
		byAddr[s.Address] = s
	}

	s1, ok := byAddr["1.1.1.1:1080"]
	if !ok {
		t.Fatal("missing setting for 1.1.1.1:1080")
	}
	if s1.Auth == nil {
		t.Fatal("1.1.1.1:1080 expected Auth, got nil")
	}
	if s1.Auth.User != "userA" || s1.Auth.Password != "passA" {
		t.Fatalf("1.1.1.1:1080 expected userA/passA, got %s/%s", s1.Auth.User, s1.Auth.Password)
	}

	s2, ok := byAddr["2.2.2.2:1080"]
	if !ok {
		t.Fatal("missing setting for 2.2.2.2:1080")
	}
	if s2.Auth == nil {
		t.Fatal("2.2.2.2:1080 expected Auth, got nil")
	}
	if s2.Auth.User != "userB" || s2.Auth.Password != "passB" {
		t.Fatalf("2.2.2.2:1080 expected userB/passB, got %s/%s", s2.Auth.User, s2.Auth.Password)
	}

	s3, ok := byAddr["3.3.3.3:1080"]
	if !ok {
		t.Fatal("missing setting for 3.3.3.3:1080")
	}
	if s3.Auth != nil {
		t.Fatalf("3.3.3.3:1080 expected nil Auth, got %v", s3.Auth)
	}
}

// TestProxyReloader_ReloadRotationExecution verifies that ProxyReloader.reload()
// cancels a running proxy and spawns a new one when credentials change in the config.
func TestProxyReloader_ReloadRotationExecution(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DISABLE_DIRECT_IP", "1")

	proxyAddr := "1.2.3.4:1080"
	oldSettings := &connect.ProxySettings{
		Network: "tcp",
		Address: proxyAddr,
		Auth: &proxy.Auth{
			User:     "olduser",
			Password: "oldpass",
		},
	}

	// Write new credentials to config
	cfg := ProxyConfig{
		Servers: map[string]string{
			fmt.Sprintf("%s:newuser:newpass", proxyAddr): "",
		},
	}
	writeProxyConfig(&cfg)

	cancelCalled := make(chan struct{}, 1)
	oldCancel := func() {
		select {
		case cancelCalled <- struct{}{}:
		default:
		}
	}

	spawnedChan := make(chan *connect.ProxySettings, 1)
	parentCtx, parentCancel := context.WithCancel(context.Background())
	t.Cleanup(parentCancel)

	spawnProxy := func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
		spawnedChan <- settings
		<-proxyCtx.Done()
	}

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap: map[string]context.CancelFunc{
			proxyAddr: oldCancel,
		},
		cancelMapMu: cancelMapMu,
		runningAuth: map[string]*connect.ProxySettings{
			proxyAddr: oldSettings,
		},
		state:           &ProxyState{Proxies: make(map[string]ProxyEntry)},
		sourcePath:      "",
		parentCtx:       parentCtx,
		wg:              &sync.WaitGroup{},
		spawnProxy:      spawnProxy,
		drainingProxies: make(map[string]context.CancelFunc),
	}

	reloader.reload()

	// 1. Verify old cancel function was called
	select {
	case <-cancelCalled:
	case <-time.After(1 * time.Second):
		t.Fatal("expected old proxy cancel function to be called on credential rotation")
	}

	// 2. Verify new proxy was spawned with new credentials
	select {
	case spawned := <-spawnedChan:
		if spawned.Address != proxyAddr {
			t.Fatalf("expected spawned address %s, got %s", proxyAddr, spawned.Address)
		}
		if spawned.Auth == nil {
			t.Fatal("expected spawned proxy to have Auth credentials, got nil")
		}
		if spawned.Auth.User != "newuser" || spawned.Auth.Password != "newpass" {
			t.Fatalf("expected spawned credentials newuser/newpass, got %s/%s", spawned.Auth.User, spawned.Auth.Password)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for spawnProxy callback on credential rotation")
	}

	// 3. Verify runningAuth was updated to new credentials
	auth, ok := reloader.runningAuthFor(proxyAddr)
	if !ok {
		t.Fatalf("runningAuth missing entry for %s after rotation", proxyAddr)
	}
	if auth.Auth == nil || auth.Auth.User != "newuser" || auth.Auth.Password != "newpass" {
		t.Fatalf("expected runningAuth updated to newuser/newpass, got %v", auth.Auth)
	}
}

// TestStartupRunningAuthSeeding verifies that seeding runningAuth at startup prevents
// reload() from churning running proxies when the configuration matches.
func TestStartupRunningAuthSeeding(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DISABLE_DIRECT_IP", "1")

	proxyKey := "1.2.3.4:1080:user:pass"
	cfg := ProxyConfig{
		Servers: map[string]string{
			proxyKey: "",
		},
	}
	writeProxyConfig(&cfg)

	settingsList := readProxySettings()
	if len(settingsList) != 1 {
		t.Fatalf("expected 1 proxy setting, got %d", len(settingsList))
	}
	seededSetting := settingsList[0]

	cancelFunc := func() {
		t.Fatal("cancel must not be invoked when runningAuth is seeded at startup")
	}

	spawnProxy := func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
		t.Fatalf("spawnProxy must not be invoked when runningAuth matches, got: %v", settings)
	}

	parentCtx, parentCancel := context.WithCancel(context.Background())
	t.Cleanup(parentCancel)

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap: map[string]context.CancelFunc{
			seededSetting.Address: cancelFunc,
		},
		cancelMapMu: cancelMapMu,
		runningAuth: map[string]*connect.ProxySettings{
			seededSetting.Address: seededSetting,
		},
		state:           &ProxyState{Proxies: make(map[string]ProxyEntry)},
		sourcePath:      "",
		parentCtx:       parentCtx,
		wg:              &sync.WaitGroup{},
		spawnProxy:      spawnProxy,
		drainingProxies: make(map[string]context.CancelFunc),
	}

	reloader.reload()

	// Verify the proxy remains in cancelMap and runningAuth
	cancelMapMu.Lock()
	_, stillRunning := reloader.cancelMap[seededSetting.Address]
	cancelMapMu.Unlock()
	if !stillRunning {
		t.Fatal("proxy was unexpectedly removed from cancelMap")
	}

	auth, ok := reloader.runningAuthFor(seededSetting.Address)
	if !ok || !sameAuth(auth, seededSetting) {
		t.Fatal("runningAuth entry altered or missing after reload")
	}
}

// TestSameAuth_FieldLevelMismatches verifies edge cases where user or password differ individually.
func TestSameAuth_FieldLevelMismatches(t *testing.T) {
	base := &connect.ProxySettings{
		Auth: &proxy.Auth{User: "user", Password: "password"},
	}

	t.Run("set vs nil Auth", func(t *testing.T) {
		other := &connect.ProxySettings{}
		if sameAuth(base, other) {
			t.Fatal("set Auth vs nil Auth must not compare equal")
		}
	})

	t.Run("same user different password", func(t *testing.T) {
		other := &connect.ProxySettings{
			Auth: &proxy.Auth{User: "user", Password: "different-password"},
		}
		if sameAuth(base, other) {
			t.Fatal("same user with different password must not compare equal")
		}
	})

	t.Run("different user same password", func(t *testing.T) {
		other := &connect.ProxySettings{
			Auth: &proxy.Auth{User: "different-user", Password: "password"},
		}
		if sameAuth(base, other) {
			t.Fatal("different user with same password must not compare equal")
		}
	})
}

// TestProxyReloader_UnrecordedAuthTriggersRotation verifies that an address present
// in cancelMap but absent in runningAuth triggers rotation on reload.
func TestProxyReloader_UnrecordedAuthTriggersRotation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DISABLE_DIRECT_IP", "1")

	proxyAddr := "1.2.3.4:1080"
	cfg := ProxyConfig{
		Servers: map[string]string{
			fmt.Sprintf("%s:u:p", proxyAddr): "",
		},
	}
	writeProxyConfig(&cfg)

	cancelCalled := false
	oldCancel := func() {
		cancelCalled = true
	}

	spawnedChan := make(chan *connect.ProxySettings, 1)
	parentCtx, parentCancel := context.WithCancel(context.Background())
	t.Cleanup(parentCancel)

	spawnProxy := func(proxyCtx context.Context, settings *connect.ProxySettings, isNative bool, isURLSourced bool) {
		spawnedChan <- settings
		<-proxyCtx.Done()
	}

	cancelMapMu := &sync.Mutex{}
	reloader := &ProxyReloader{
		cancelMap: map[string]context.CancelFunc{
			proxyAddr: oldCancel,
		},
		cancelMapMu: cancelMapMu,
		runningAuth: map[string]*connect.ProxySettings{}, // empty: proxy was unrecorded
		state:       &ProxyState{Proxies: make(map[string]ProxyEntry)},
		sourcePath:  "",
		parentCtx:   parentCtx,
		wg:          &sync.WaitGroup{},
		spawnProxy:  spawnProxy,
		drainingProxies: make(map[string]context.CancelFunc),
	}

	reloader.reload()

	if !cancelCalled {
		t.Fatal("expected unrecorded proxy to be cancelled on reload")
	}
	select {
	case spawned := <-spawnedChan:
		if spawned.Address != proxyAddr {
			t.Fatalf("expected spawned address %s, got %s", proxyAddr, spawned.Address)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for spawnProxy")
	}
}

// TestProxyReloader_RemovedProxyCleansUpRunningAuth verifies that removing a proxy
// cancels it and drops its entry from runningAuth.
func TestProxyReloader_RemovedProxyCleansUpRunningAuth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("DISABLE_DIRECT_IP", "1")

	proxyAddr := "1.2.3.4:1080"
	// Empty config: desired set is empty, but reload should not prune if desired is empty
	// so write another desired proxy to allow reload removal pass.
	keptAddr := "9.9.9.9:1080"
	cfg := ProxyConfig{
		Servers: map[string]string{
			keptAddr: "",
		},
	}
	writeProxyConfig(&cfg)

	cancelCalled := false
	reloader := &ProxyReloader{
		cancelMap: map[string]context.CancelFunc{
			proxyAddr: func() { cancelCalled = true },
			keptAddr:  func() {},
		},
		cancelMapMu: &sync.Mutex{},
		runningAuth: map[string]*connect.ProxySettings{
			proxyAddr: {Address: proxyAddr},
			keptAddr:  {Address: keptAddr},
		},
		state:           &ProxyState{Proxies: make(map[string]ProxyEntry)},
		sourcePath:      "",
		parentCtx:       context.Background(),
		wg:              &sync.WaitGroup{},
		spawnProxy:      func(ctx context.Context, s *connect.ProxySettings, n bool, u bool) {},
		drainingProxies: make(map[string]context.CancelFunc),
	}

	reloader.reload()

	if !cancelCalled {
		t.Fatal("expected removed proxy cancel function to be called")
	}
	if _, ok := reloader.runningAuthFor(proxyAddr); ok {
		t.Fatal("expected runningAuth entry to be purged for removed proxy")
	}
}

// TestProxyAdd_DifferentPortsSameHostPreserved verifies that adding proxies with
// the same host IP but different ports does not purge existing entries.
func TestProxyAdd_DifferentPortsSameHostPreserved(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	cfg := ProxyConfig{
		Servers: map[string]string{
			"1.2.3.4:1080:u1:p1": "",
		},
	}
	writeProxyConfig(&cfg)

	for port := 1081; port <= 1083; port++ {
		portStr := strconv.Itoa(port)
		opts := docopt.Opts{
			"<key_address>": []string{fmt.Sprintf("1.2.3.4:%s:u%s:p%s", portStr, portStr, portStr)},
			"-f":            true,
		}
		proxyAdd(opts)
	}

	got := readProxyConfig()
	if len(got.Servers) != 4 {
		t.Fatalf("expected 4 servers across different ports, got %d: %v", len(got.Servers), got.Servers)
	}
}
