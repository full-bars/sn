package miner

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// pprofLogSink captures the retry helper's lines so a test can assert on the
// outcome without a live stderr.
type pprofLogSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *pprofLogSink) logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
}

func (l *pprofLogSink) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "")
}

// getPprof fetches an endpoint from the running pprof listener.
func getPprof(t *testing.T, addr, path string) (int, string, string) {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + addr + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, string(body), resp.Header.Get("Content-Type")
}

// Profiling is off unless the operator asked for it: no env value, no
// listener, and nothing bound to a default port.
func TestProfilingOffByDefault(t *testing.T) {
	if err := stopProfiling(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	t.Setenv(envProfiling, "")

	startProfilingAtStartup(context.Background())
	if addr := profilingAddr(); addr != "" {
		_ = stopProfiling()
		t.Fatalf("profiling started without %s: listening on %s", envProfiling, addr)
	}
}

// A non-loopback, wildcard, hostname or malformed value must be refused before
// anything binds. This is the security half of the port: the provider hands
// its value straight to net.Listen, so 0.0.0.0 would publish heap dumps; the
// miner must not.
func TestEnableProfilingRefusesNonLoopback(t *testing.T) {
	_ = stopProfiling()
	t.Cleanup(func() { _ = stopProfiling() })

	for _, spec := range []string{
		"0.0.0.0:6060",     // every IPv4 interface
		"[::]:6060",        // every IPv6 interface
		":6060",            // no host at all
		"localhost:6060",   // hostname: can resolve anywhere
		"example.com:6060", // hostname
		"192.0.2.1:6060",   // TEST-NET-1, not loopback
		"10.0.0.1:6060",    // private but not loopback
		"127.0.0.1",        // no port
		"127.0.0.1:http",   // non-numeric port
		"127.0.0.1:70000",  // out of range
		"::1:6060",         // not host:port for IPv6
		"",                 // nothing
	} {
		if err := enableProfiling(context.Background(), spec); err == nil {
			_ = stopProfiling()
			t.Fatalf("enableProfiling(%q) accepted a non-loopback address", spec)
		}
		if addr := profilingAddr(); addr != "" {
			_ = stopProfiling()
			t.Fatalf("enableProfiling(%q) bound %s despite being refused", spec, addr)
		}
	}
}

// Enabled starts a listener on loopback, and only on loopback: the same port
// must not answer on a non-loopback interface address.
func TestEnableProfilingBindsLoopbackOnly(t *testing.T) {
	_ = stopProfiling()
	t.Cleanup(func() { _ = stopProfiling() })

	if err := enableProfiling(context.Background(), "127.0.0.1:0"); err != nil {
		t.Fatalf("enableProfiling: %v", err)
	}
	addr := profilingAddr()
	if addr == "" {
		t.Fatal("no listener after enableProfiling")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("bound address %q: %v", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("bound %s, want a loopback address", addr)
	}
	if code, _, _ := getPprof(t, addr, "/debug/pprof/"); code != http.StatusOK {
		t.Fatalf("/debug/pprof/ = %d, want 200", code)
	}

	// The port must be closed on every other local address. A host with no
	// non-loopback IPv4 has nothing to prove here.
	for _, local := range nonLoopbackIPv4s() {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(local, port), 500*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatalf("pprof answered on %s:%s, which is not loopback", local, port)
		}
	}
}

// nonLoopbackIPv4s lists this host's non-loopback IPv4 addresses.
func nonLoopbackIPv4s() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ip.String())
	}
	return out
}

// The listener serves the pprof endpoints a field diagnosis needs: the index,
// the named profiles, cmdline, symbol, a CPU profile and a trace.
func TestProfilingServesEndpoints(t *testing.T) {
	_ = stopProfiling()
	t.Cleanup(func() { _ = stopProfiling() })

	if err := enableProfiling(context.Background(), "127.0.0.1:0"); err != nil {
		t.Fatalf("enableProfiling: %v", err)
	}
	addr := profilingAddr()

	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/cmdline",
		"/debug/pprof/symbol",
		"/debug/pprof/allocs?debug=1",
		"/debug/pprof/block?debug=1",
		"/debug/pprof/goroutine?debug=1",
		"/debug/pprof/mutex?debug=1",
		"/debug/pprof/heap?debug=1",
		"/debug/pprof/threadcreate?debug=1",
	} {
		if code, _, _ := getPprof(t, addr, path); code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", path, code)
		}
	}

	// The index names the profiles, and the goroutine dump is the one an
	// operator reads first.
	if _, body, _ := getPprof(t, addr, "/debug/pprof/"); !strings.Contains(body, "goroutine") {
		t.Fatalf("index does not list goroutine: %q", body)
	}
	if _, body, _ := getPprof(t, addr, "/debug/pprof/heap?debug=1"); !strings.Contains(body, "heap profile") {
		t.Fatalf("heap profile body = %q", body)
	}

	// A real CPU profile and execution trace, both bounded to a second.
	code, body, _ := getPprof(t, addr, "/debug/pprof/profile?seconds=1")
	if code != http.StatusOK || len(body) == 0 {
		t.Fatalf("/debug/pprof/profile = %d, %d bytes", code, len(body))
	}
	code, body, _ = getPprof(t, addr, "/debug/pprof/trace?seconds=1")
	if code != http.StatusOK || len(body) == 0 {
		t.Fatalf("/debug/pprof/trace = %d, %d bytes", code, len(body))
	}

	// Nothing else is served: not the miner's /metrics payload and not an
	// unknown profile name.
	for _, path := range []string{"/metrics", "/debug/pprof/nope", "/"} {
		if code, _, _ := getPprof(t, addr, path); code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", path, code)
		}
	}
}

// A second enable is refused instead of replacing or double-serving the
// listener.
func TestEnableProfilingSecondListenerRefused(t *testing.T) {
	_ = stopProfiling()
	t.Cleanup(func() { _ = stopProfiling() })

	if err := enableProfiling(context.Background(), "127.0.0.1:0"); err != nil {
		t.Fatalf("first enableProfiling: %v", err)
	}
	first := profilingAddr()
	if err := enableProfiling(context.Background(), "127.0.0.1:0"); err == nil {
		t.Fatal("a second enableProfiling was accepted")
	}
	if addr := profilingAddr(); addr != first {
		t.Fatalf("listener moved from %s to %s", first, addr)
	}
	if code, _, _ := getPprof(t, first, "/debug/pprof/"); code != http.StatusOK {
		t.Fatalf("first listener stopped answering: %d", code)
	}
}

// startProfilingAtStartup is the only path the miner's run uses: it honors
// URNETWORK_PPROF when set and stays silent when the value is not loopback.
func TestStartProfilingAtStartupHonorsEnv(t *testing.T) {
	_ = stopProfiling()
	t.Cleanup(func() { _ = stopProfiling() })

	t.Setenv(envProfiling, "127.0.0.1:0")
	startProfilingAtStartup(context.Background())

	var addr string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if addr = profilingAddr(); addr != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		t.Fatalf("%s did not start the listener", envProfiling)
	}
	if code, _, _ := getPprof(t, addr, "/debug/pprof/"); code != http.StatusOK {
		t.Fatalf("/debug/pprof/ = %d, want 200", code)
	}
	_ = stopProfiling()

	// A widened value is refused at startup: no listener, and no panic.
	t.Setenv(envProfiling, "0.0.0.0:0")
	startProfilingAtStartup(context.Background())
	if addr := profilingAddr(); addr != "" {
		t.Fatalf("a non-loopback %s started a listener on %s", envProfiling, addr)
	}
}

// The HotSwap reproduction: the parent holds the port, the candidate's first
// bind fails with EADDRINUSE, and it must bind once the parent exits.
func TestEnableProfilingWithRetryBindsOnceParentReleases(t *testing.T) {
	_ = stopProfiling()
	t.Cleanup(func() { _ = stopProfiling() })

	parent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := parent.Addr().String()
	go func() {
		time.Sleep(250 * time.Millisecond)
		parent.Close() // parent drain finished, port is free
	}()

	var sink pprofLogSink
	done := make(chan struct{})
	go func() {
		enableProfilingWithRetry(context.Background(), addr, enableProfiling, 5*time.Second, 25*time.Millisecond, sink.logf)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("retry did not finish")
	}
	if got := profilingAddr(); got != addr {
		t.Fatalf("listener on %q, want %q (log: %q)", got, addr, sink.joined())
	}
	if !strings.Contains(sink.joined(), "released it") {
		t.Fatalf("expected the release message, log: %q", sink.joined())
	}
	if code, _, _ := getPprof(t, addr, "/debug/pprof/"); code != http.StatusOK {
		t.Fatalf("/debug/pprof/ after retry = %d, want 200", code)
	}
}

func TestEnableProfilingWithRetryGivesUpWhilePortStaysBusy(t *testing.T) {
	_ = stopProfiling()
	t.Cleanup(func() { _ = stopProfiling() })

	parent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	var sink pprofLogSink
	start := time.Now()
	enableProfilingWithRetry(context.Background(), parent.Addr().String(), enableProfiling, 150*time.Millisecond, 20*time.Millisecond, sink.logf)
	if !strings.Contains(sink.joined(), "gave up") {
		t.Fatalf("expected a give-up message, got %q", sink.joined())
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("did not honor the wait bound: %s", time.Since(start))
	}
	if addr := profilingAddr(); addr != "" {
		t.Fatalf("a busy port left a listener on %s", addr)
	}
}

// An address that can never bind (not loopback, malformed) will never start
// working, so it is reported once, not retried.
func TestEnableProfilingWithRetryDoesNotRetryPermanentErrors(t *testing.T) {
	_ = stopProfiling()
	t.Cleanup(func() { _ = stopProfiling() })

	calls := 0
	var sink pprofLogSink
	enableProfilingWithRetry(context.Background(), "0.0.0.0:6060", func(ctx context.Context, a string) error {
		calls++
		return enableProfiling(ctx, a)
	}, 5*time.Second, 10*time.Millisecond, sink.logf)
	if calls != 1 {
		t.Fatalf("permanent error retried %d times", calls)
	}
	if !strings.Contains(sink.joined(), "failed to enable pprof") {
		t.Fatalf("log: %q", sink.joined())
	}
}

func TestEnableProfilingWithRetryStopsOnContextCancel(t *testing.T) {
	_ = stopProfiling()
	t.Cleanup(func() { _ = stopProfiling() })

	parent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		enableProfilingWithRetry(ctx, parent.Addr().String(), enableProfiling, time.Minute, 20*time.Millisecond, func(string, ...any) {})
		close(done)
	}()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not stop on context cancel")
	}
}

// currentProfilingRetry is the retry loop in flight, if any. The test needs the
// handle before stopProfiling clears it.
func currentProfilingRetry() *profilingRetryLoop {
	profilingMu.Lock()
	defer profilingMu.Unlock()
	return profilingRetry
}

// A context that is already cancelled must not bind at all: the run context is
// cancelled by the time the deferred stopProfiling has run, and a bind after
// that would leave a listener nothing ever closes.
func TestStartProfilingWithCancelledContextBindsNothing(t *testing.T) {
	_ = stopProfiling()
	t.Cleanup(func() { _ = stopProfiling() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Setenv(envProfiling, "127.0.0.1:0")

	startProfilingAtStartup(ctx)
	// The handle is installed before the goroutine starts and cleared by its
	// deferred cleanup, so a nil handle means the goroutine already returned.
	if loop := currentProfilingRetry(); loop != nil {
		select {
		case <-loop.done:
		case <-time.After(2 * time.Second):
			t.Fatal("the retry on a cancelled context did not return")
		}
	}
	if addr := profilingAddr(); addr != "" {
		t.Fatalf("a cancelled context still bound %s", addr)
	}
}

// enableProfiling must close the socket it already bound when the context is
// cancelled underneath it, not register it: a stop can land between net.Listen
// and the bookkeeping.
func TestEnableProfilingClosesSocketOnCancelledContext(t *testing.T) {
	_ = stopProfiling()
	t.Cleanup(func() { _ = stopProfiling() })

	// A concrete port, so the test can prove the refused bind released it.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := enableProfiling(ctx, addr); err == nil {
		t.Fatal("enableProfiling accepted a cancelled context")
	}
	if got := profilingAddr(); got != "" {
		t.Fatalf("registered %s on a cancelled context", got)
	}
	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the cancelled enable left %s bound: %v", addr, err)
	}
	_ = again.Close()
}

// The retry is bounded at 90s by its caller; a stop while it waits for the
// HotSwap parent must end it promptly instead of letting it bind later.
func TestStopProfilingCancelsWaitingRetry(t *testing.T) {
	_ = stopProfiling()
	t.Cleanup(func() { _ = stopProfiling() })

	parent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	t.Setenv(envProfiling, parent.Addr().String())
	startProfilingAtStartup(context.Background())
	loop := currentProfilingRetry()
	if loop == nil {
		t.Fatal("startProfilingAtStartup did not launch a retry")
	}
	// Let it take its first bind (EADDRINUSE) and settle into the 1s poll.
	time.Sleep(100 * time.Millisecond)

	started := time.Now()
	if err := stopProfiling(); err != nil {
		t.Errorf("stopProfiling: %v", err)
	}
	if elapsed := time.Since(started); 2*time.Second < elapsed {
		t.Fatalf("stopProfiling took %s for a retry bounded at 90s", elapsed)
	}
	select {
	case <-loop.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the retry loop outlived stopProfiling")
	}
	if retry := currentProfilingRetry(); retry != nil {
		t.Fatal("stopProfiling left a retry registered")
	}

	// The parent releases the port after the stop; nothing may claim it.
	_ = parent.Close()
	time.Sleep(250 * time.Millisecond)
	if addr := profilingAddr(); addr != "" {
		t.Fatalf("a stopped pprof listener came back on %s", addr)
	}
}
