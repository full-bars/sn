package miner

import (
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// stubMetricsAuto pins the auto bind path off the test box: no Tailscale
// addresses, not a container, and an ephemeral port instead of 9100-9103.
// The stubbed vars and funcs are restored when the test ends.
func stubMetricsAuto(t *testing.T) {
	t.Helper()
	t.Setenv("URNETWORK_METRICS", "")
	prevTailscale := tailscaleAddrsFunc
	tailscaleAddrsFunc = func() []netip.Addr { return nil }
	prevContainer := containerAddrsFunc
	containerAddrsFunc = func() []netip.Addr { return nil }
	prevMarkers := metricsContainerMarkers
	metricsContainerMarkers = nil
	prevPorts := metricsAutoPorts
	metricsAutoPorts = []int{0}
	t.Cleanup(func() {
		tailscaleAddrsFunc = prevTailscale
		containerAddrsFunc = prevContainer
		metricsContainerMarkers = prevMarkers
		metricsAutoPorts = prevPorts
		_ = stopMetrics()
	})
}

// withMetricsState swaps the process-wide control state for a fresh one, the
// way provider startup installs loadControlState's result.
func withMetricsState(t *testing.T) *controlState {
	t.Helper()
	saved := globalControlState
	state := newControlState()
	globalControlState = state
	t.Cleanup(func() { globalControlState = saved })
	return state
}

// reserveFreePort returns a port that was free a moment ago. A test that needs
// a concrete rebound address uses it; a race with another process fails the
// bind loudly instead of silently moving to another port.
func reserveFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserveFreePort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// /metrics is on unless an operator turned it off: an unset key follows the
// default, an explicit off is honored, an explicit on stays on.
func TestMetricsEnabledByStateDefaultsOn(t *testing.T) {
	state := newControlState()
	if !metricsEnabledByState(state) {
		t.Fatal("an unset metrics key must default to on")
	}
	if err := state.set("metrics", "off"); err != nil {
		t.Fatal(err)
	}
	if metricsEnabledByState(state) {
		t.Fatal("a persisted off must win over the default")
	}
	if err := state.set("metrics", "on"); err != nil {
		t.Fatal(err)
	}
	if !metricsEnabledByState(state) {
		t.Fatal("a persisted on must stay on")
	}
}

func TestValidateMetricsListen(t *testing.T) {
	for _, good := range []string{"auto", "off", "AUTO", "Off", "100.64.0.10:9100", "0.0.0.0:9100", "[::1]:9100"} {
		if err := validateMetricsListen(good); err != nil {
			t.Errorf("validateMetricsListen(%q) = %v", good, err)
		}
	}
	for _, bad := range []string{"", "9100", "example.com:9100", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1", "::1:9100"} {
		if err := validateMetricsListen(bad); err == nil {
			t.Errorf("validateMetricsListen(%q) accepted an invalid address", bad)
		}
	}
}

// Where /metrics binds, in precedence order: an explicit URNETWORK_METRICS
// address, then the persisted metrics_listen setting, then auto. Auto is
// loopback plus the Tailscale rescan wrapper; an explicit address binds
// exactly and must fail rather than silently move when it cannot.
func TestListenMetricsPrecedence(t *testing.T) {
	withTempHome(t)
	stubMetricsAuto(t)
	state := withMetricsState(t)

	ln, err := listenMetrics(0)
	if err != nil {
		t.Fatalf("auto: %v", err)
	}
	multi, ok := ln.(*metricsMultiListener)
	if !ok {
		t.Fatalf("auto must serve the Tailscale-aware listener, got %T", ln)
	}
	if host, _, err := net.SplitHostPort(multi.Addr().String()); err != nil || host != "127.0.0.1" {
		t.Fatalf("auto listener = %q, want a 127.0.0.1 address", multi.Addr().String())
	}
	multi.Close()

	// An unbindable explicit address fails instead of falling back to auto.
	// 192.0.2.0/24 is TEST-NET-1, never assigned to a local interface.
	state.setWithValue("metrics_listen", "192.0.2.1:9100", SourceSocket)
	if ln, err := listenMetrics(0); err == nil {
		ln.Close()
		t.Fatal("an unbindable metrics_listen address must fail, not fall back to auto")
	}

	// URNETWORK_METRICS overrides the persisted address entirely.
	t.Setenv("URNETWORK_METRICS", "127.0.0.1:0")
	ln, err = listenMetrics(0)
	if err != nil {
		t.Fatalf("URNETWORK_METRICS: %v", err)
	}
	if _, ok := ln.(*metricsMultiListener); ok {
		t.Fatal("an explicit address must bind exactly, not wrap in the auto listener")
	}
	ln.Close()
}

// startDefaultMetrics is the startup default: on for a node that never set
// the key, out of the way of an explicit off, an explicit URNETWORK_METRICS
// address, and a HotSwap candidate (the parent still holds the port).
func TestStartDefaultMetrics(t *testing.T) {
	withTempHome(t)
	stubMetricsAuto(t)

	off := newControlState()
	if err := off.set("metrics", "off"); err != nil {
		t.Fatal(err)
	}
	startDefaultMetrics(off, false)
	if metricsServedAddrs() != nil {
		t.Fatal("a persisted off must not start the listener")
	}

	startDefaultMetrics(newControlState(), true)
	if metricsServedAddrs() != nil {
		t.Fatal("a HotSwap candidate must not bind: the parent still holds the address")
	}

	t.Setenv("URNETWORK_METRICS", "127.0.0.1:1")
	startDefaultMetrics(newControlState(), false)
	if metricsServedAddrs() != nil {
		t.Fatal("an explicit URNETWORK_METRICS address must be bound by the startup path, not the default")
	}
	t.Setenv("URNETWORK_METRICS", "")

	startDefaultMetrics(newControlState(), false)
	if metricsServedAddrs() == nil {
		t.Fatal("a node that never set the key must start /metrics at startup")
	}
	// Idempotent: a second default start must not bind a second port.
	startDefaultMetrics(newControlState(), false)
	if addrs := metricsServedAddrs(); len(addrs) != 1 {
		t.Fatalf("a second default start rebinds: %v", addrs)
	}
}

// applyMetricsLive is the live half behind `set metrics on|off`: bind before
// reporting success, stop and forget on off, and stay idempotent.
func TestApplyMetricsLiveStartsAndStops(t *testing.T) {
	withTempHome(t)
	stubMetricsAuto(t)
	if metricsServedAddrs() != nil {
		t.Fatal("no listener expected before metrics on")
	}
	if err := applyMetricsLive("on"); err != nil {
		t.Fatalf("metrics on: %v", err)
	}
	if addrs := metricsServedAddrs(); len(addrs) == 0 {
		t.Fatal("metrics on must report its listen address")
	}
	if err := applyMetricsLive("on"); err != nil {
		t.Fatalf("second metrics on: %v", err)
	}
	if addrs := metricsServedAddrs(); len(addrs) != 1 {
		t.Fatalf("metrics on must be idempotent, got %v", addrs)
	}
	if err := applyMetricsLive("off"); err != nil {
		t.Fatalf("metrics off: %v", err)
	}
	if metricsServedAddrs() != nil {
		t.Fatal("metrics off must stop and forget the listener")
	}
	if err := applyMetricsLive("off"); err != nil {
		t.Fatalf("second metrics off: %v", err)
	}
}

// The handler answers /metrics only, with the full miner payload under the
// classic text content type, and gzips it for a client that asks.
func TestMetricsHandlerServesExposition(t *testing.T) {
	withTempHome(t)
	withMetricsState(t)

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	metricsHandler().ServeHTTP(w, req)
	body := w.Body.String()
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("Content-Type = %q", got)
	}
	for _, want := range []string{
		"# TYPE urnet_info gauge",
		`urnet_info{version="`,
		`urnet_control_commands_total{cmd="get"}`,
		`urnet_control_commands_total{cmd="set"}`,
		`urnet_control_commands_total{cmd="clear"}`,
		`urnet_control_commands_total{cmd="any"}`,
		`urnet_proxy_pool_size{status="up"}`,
		"urnet_startup_clean_shutdown",
		"urnet_restart_reason",
		"urnet_uptime_seconds",
		"urnet_gc_cycles_total",
		"urnet_mem_sys_bytes",
		"urnet_goroutines",
		"urnet_pressure_score",
		"urnet_h1plus_enabled",
		"urnet_proxy_grades",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing %s", want)
		}
	}
	for _, err := range Lint(body) {
		t.Error(err)
	}

	// Nothing but /metrics answers.
	req = httptest.NewRequest("GET", "/", nil)
	w = httptest.NewRecorder()
	metricsHandler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-/metrics path answered %d", w.Code)
	}

	// A gzip-accepting client gets a gzipped payload that inflates to the
	// same exposition.
	req = httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w = httptest.NewRecorder()
	metricsHandler().ServeHTTP(w, req)
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("gzip body: %v", err)
	}
	inflated, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("inflate: %v", err)
	}
	if !strings.Contains(string(inflated), "# TYPE urnet_info gauge") {
		t.Fatal("inflated payload is not the exposition")
	}
}

// The control socket wiring: `set metrics on|off`, `clear metrics`,
// metrics_listen rebinds, and the status reply carries MetricsAddrs.
func TestControlSocketMetricsLiveWiring(t *testing.T) {
	stubMetricsAuto(t)
	state := withMetricsState(t)
	path, cleanup := testControlSocket(t, state)
	defer cleanup()

	resp := controlRequestJSON(t, path, controlRequest{Cmd: "set", Key: "metrics", Value: "maybe"})
	if resp.OK {
		t.Fatal("set metrics maybe must be refused")
	}

	resp = controlRequestJSON(t, path, controlRequest{Cmd: "set", Key: "metrics", Value: "on"})
	if !resp.OK {
		t.Fatalf("set metrics on: %s", resp.Error)
	}
	if metricsServedAddrs() == nil {
		t.Fatal("set metrics on must start the listener")
	}
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "status"})
	if !resp.OK {
		t.Fatalf("status: %s", resp.Error)
	}
	if len(resp.MetricsAddrs) == 0 {
		t.Fatal("status must report the metrics listen addresses while on")
	}
	if resp.Settings["metrics"].Value != "on" {
		t.Fatalf("status metrics = %q", resp.Settings["metrics"].Value)
	}

	// Clearing the key reapplies the off default: the operator reaches for
	// this when scraping goes wrong, so it must actually stop the listener.
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "clear", Key: "metrics"})
	if !resp.OK {
		t.Fatalf("clear metrics: %s", resp.Error)
	}
	if metricsServedAddrs() != nil {
		t.Fatal("clear metrics must stop the listener")
	}
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "status"})
	if len(resp.MetricsAddrs) != 0 {
		t.Fatalf("status reports addresses while metrics is off: %v", resp.MetricsAddrs)
	}

	// metrics_listen moves a running listener, and clearing it goes back to
	// auto. A hostname is refused, an IP literal is accepted.
	if err := validateMetricsListen("example.com:9100"); err == nil {
		t.Fatal("metrics_listen accepted a hostname")
	}
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "set", Key: "metrics", Value: "on"})
	if !resp.OK {
		t.Fatalf("set metrics on: %s", resp.Error)
	}
	freePort := reserveFreePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", freePort)
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "set", Key: "metrics_listen", Value: addr})
	if !resp.OK {
		t.Fatalf("set metrics_listen %s: %s", addr, resp.Error)
	}
	if addrs := metricsServedAddrs(); len(addrs) != 1 || addrs[0] != addr {
		t.Fatalf("metrics_listen rebind = %v, want [%s]", addrs, addr)
	}
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "clear", Key: "metrics_listen"})
	if !resp.OK {
		t.Fatalf("clear metrics_listen: %s", resp.Error)
	}
	if addrs := metricsServedAddrs(); len(addrs) == 0 {
		t.Fatal("clearing metrics_listen must rebind the auto listener")
	}
	if err := applyMetricsLive("off"); err != nil {
		t.Fatal(err)
	}
}

func TestIncrControlCmdCounts(t *testing.T) {
	acked, set, get, clear := controlCmdsAcked.Load(), controlCmdsSet.Load(), controlCmdsGet.Load(), controlCmdsClear.Load()
	IncrControlCmd("set")
	IncrControlCmd("get")
	IncrControlCmd("clear")
	IncrControlCmd("version")
	if got := controlCmdsAcked.Load(); got != acked+4 {
		t.Fatalf("acked = %d, want %d", got, acked+4)
	}
	if got := controlCmdsSet.Load(); got != set+1 {
		t.Fatalf("set = %d, want %d", got, set+1)
	}
	if got := controlCmdsGet.Load(); got != get+1 {
		t.Fatalf("get = %d, want %d", got, get+1)
	}
	if got := controlCmdsClear.Load(); got != clear+1 {
		t.Fatalf("clear = %d, want %d", got, clear+1)
	}
}

// prometheusLabelValue must produce a value the text format can parse, even
// for quotes, backslashes, newlines and invalid UTF-8.
func TestPrometheusLabelValueEscapes(t *testing.T) {
	escaped := prometheusLabelValue("a\"b\\c\nd\xff")
	if !strings.HasPrefix(escaped, `"`) || !strings.HasSuffix(escaped, `"`) {
		t.Fatalf("label value is not quoted: %q", escaped)
	}
	if strings.ContainsAny(escaped[1:len(escaped)-1], "\n") {
		t.Fatalf("raw newline survived: %q", escaped)
	}
	body := "# TYPE x gauge\nx{x=" + escaped + "} 1\n"
	if errs := Lint(body); len(errs) != 0 {
		t.Fatalf("escaped label does not parse: %v", errs)
	}
}
