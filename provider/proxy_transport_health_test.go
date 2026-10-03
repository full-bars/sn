package provider

import (
	"context"
	"errors"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// fakeConnTransport stands in for *connect.PlatformTransport's connection
// state: IsConnected plus a ConnectedNotify channel closed on every change.
type fakeConnTransport struct {
	mu        sync.Mutex
	connected bool
	notify    chan struct{}
}

func newFakeConnTransport() *fakeConnTransport {
	return &fakeConnTransport{notify: make(chan struct{})}
}

func (f *fakeConnTransport) IsConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeConnTransport) ConnectedNotify() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notify
}

func (f *fakeConnTransport) set(connected bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = connected
	close(f.notify)
	f.notify = make(chan struct{})
}

// discardLogger is a connect.Logger with every level disabled.
type discardLogger struct{}

func (discardLogger) Info(...any)             {}
func (discardLogger) Infof(string, ...any)    {}
func (discardLogger) Warningf(string, ...any) {}
func (discardLogger) Errorf(string, ...any)   {}
func (discardLogger) V(int32) connect.Verbose { return discardVerbose{} }

type discardVerbose struct{}

func (discardVerbose) Enabled() bool        { return false }
func (discardVerbose) Info(...any)          {}
func (discardVerbose) Infof(string, ...any) {}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// failConnect emits one failed connect attempt the way the engine does: the
// INFO line when the throttle allows it, otherwise the V(1) line.
func failConnect(log connect.Logger, throttled bool) {
	err := errors.New("Timeout.")
	if throttled {
		if v := log.V(1); v.Enabled() {
			v.Infof(transportAuthErrFormat, "client-id", err)
		}
		return
	}
	log.Infof(transportAuthErrFormat, "client-id", err)
}

func setupTransportHealth(t *testing.T, index int, key string) (*proxyTransportHealth, *fakeConnTransport, connect.Logger, context.CancelFunc) {
	t.Helper()
	ResetProxyHealthForTesting()
	t.Cleanup(ResetProxyHealthForTesting)
	RegisterProxy(index, key, key)
	ctx, cancel := context.WithCancel(context.Background())
	h := newProxyTransportHealth(ctx, index)
	log := h.logger(discardLogger{})
	fake := newFakeConnTransport()
	stop := h.start(fake)
	t.Cleanup(func() {
		cancel()
		stop()
	})
	return h, fake, log, cancel
}

func proxyHealthOf(key string) string {
	return ProxyHealthByKey()[key].Health
}

func TestTransportHealth_UpOnlyOnConnectionDownOnDrop(t *testing.T) {
	const key = "10.1.1.1:1080"
	_, fake, _, _ := setupTransportHealth(t, 11, key)

	// Give the watcher time to act on the initial (disconnected) state.
	time.Sleep(20 * time.Millisecond)
	if ProxyEverUp(11) || proxyHealthOf(key) == "up" {
		t.Fatalf("proxy marked up before its transport connected: health=%q everUp=%v", proxyHealthOf(key), ProxyEverUp(11))
	}

	fake.set(true)
	waitFor(t, "proxy up after connect", func() bool { return proxyHealthOf(key) == "up" && ProxyEverUp(11) })

	fake.set(false)
	waitFor(t, "proxy down after drop", func() bool { return proxyHealthOf(key) != "up" })
	if got := ProxyHealthByKey()[key].TransportDrops; got != 1 {
		t.Fatalf("TransportDrops = %d, want 1", got)
	}
}

func TestTransportHealth_ConnectFailuresCountedOnlyWhileDisconnected(t *testing.T) {
	const key = "10.1.1.2:1080"
	_, fake, log, cancel := setupTransportHealth(t, 12, key)

	failConnect(log, false)
	failConnect(log, true)
	log.Infof(transportAuthErrSuppressedFormat, "client-id", errors.New("Timeout."), int64(3))
	if got := ProxyAuthFailureCount(12); got != 3 {
		t.Fatalf("auth failures while disconnected = %d, want 3", got)
	}

	// A lost H3 race while H1 serves is not a failure to connect.
	fake.set(true)
	waitFor(t, "proxy up", func() bool { return proxyHealthOf(key) == "up" })
	failConnect(log, false)
	if got := ProxyAuthFailureCount(12); got != 3 {
		t.Fatalf("failure counted while connected: %d", got)
	}

	// Canceled attempts and teardown are not failures.
	fake.set(false)
	waitFor(t, "proxy down", func() bool { return proxyHealthOf(key) != "up" })
	log.Infof(transportAuthErrFormat, "client-id", context.Canceled)
	cancel()
	failConnect(log, false)
	if got := ProxyAuthFailureCount(12); got != 3 {
		t.Fatalf("canceled or teardown attempt counted: %d", got)
	}
}

func shortenRevocationWatch(t *testing.T) {
	t.Helper()
	prev := revocationWatchInterval
	revocationWatchInterval = 5 * time.Millisecond
	t.Cleanup(func() { revocationWatchInterval = prev })
}

func runRevocationWatcher(t *testing.T, key string, index int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchReusedIdentityForRevocation(ctx, key, index, nil)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("revocation watcher did not return")
	}
}

// A reused identity the server revoked never connects; its failed connects
// must reach the revocation watcher, which evicts it from the JWT store.
func TestRevocationWatcher_EvictsIdentityThatNeverConnects(t *testing.T) {
	withTempHome(t)
	shortenRevocationWatch(t)
	const key = "10.1.1.3:1080"
	_, _, log, _ := setupTransportHealth(t, 13, key)
	if err := loadGlobalClientJWTStore().Put(key, clientJWTEntry{ByClientJWT: "reused", ClientID: "c", MintedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < revokedIdentityAuthFailureThreshold; i++ {
		failConnect(log, i%2 == 1)
	}
	runRevocationWatcher(t, key, 13)

	if _, ok := loadGlobalClientJWTStore().Get(key); ok {
		t.Fatal("revoked identity that never connected was not evicted")
	}
}

// A reused identity that did connect is good, even if the connection later
// drops and reconnects keep failing.
func TestRevocationWatcher_KeepsIdentityThatConnected(t *testing.T) {
	withTempHome(t)
	shortenRevocationWatch(t)
	const key = "10.1.1.4:1080"
	_, fake, log, _ := setupTransportHealth(t, 14, key)
	if err := loadGlobalClientJWTStore().Put(key, clientJWTEntry{ByClientJWT: "reused", ClientID: "c", MintedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	fake.set(true)
	waitFor(t, "proxy up", func() bool { return ProxyEverUp(14) })
	fake.set(false)
	waitFor(t, "proxy down", func() bool { return proxyHealthOf(key) != "up" })
	for i := 0; i < 2*revokedIdentityAuthFailureThreshold; i++ {
		failConnect(log, false)
	}
	runRevocationWatcher(t, key, 14)

	if _, ok := loadGlobalClientJWTStore().Get(key); !ok {
		t.Fatal("identity that connected was evicted")
	}
}

// TestTransportAuthErrorFormatsMatchPinnedEngine pins the "[t]auth error"
// format strings transportFailureLogger matches against the pinned engine
// source. If an engine bump rewords them, connect failures would silently
// stop being counted; this makes that bump fail here instead.
func TestTransportAuthErrorFormatsMatchPinnedEngine(t *testing.T) {
	// The binary records where the engine's code came from; this resolves to
	// the pinned module's transport.go unless built with -trimpath.
	file, _ := runtime.FuncForPC(reflect.ValueOf(connect.NewPlatformTransport).Pointer()).FileLine(reflect.ValueOf(connect.NewPlatformTransport).Pointer())
	src, err := os.ReadFile(file)
	if err != nil {
		t.Skipf("cannot read pinned engine source %q: %v", file, err)
	}
	for _, format := range []string{transportAuthErrFormat, transportAuthErrSuppressedFormat} {
		// The engine source spells the trailing newline as the two
		// characters backslash and n.
		literal := strconv.Quote(format)
		if n := strings.Count(string(src), literal); n < 2 {
			t.Fatalf("pinned engine logs %s %d times, want at least 2 (H1 and H3 connect loops); transportFailureLogger no longer sees failed connects", literal, n)
		}
	}
	if !strings.Contains(string(src), "func (self *PlatformTransport) ConnectedNotify() <-chan struct{}") {
		t.Fatal("pinned engine no longer exposes PlatformTransport.ConnectedNotify")
	}
}
