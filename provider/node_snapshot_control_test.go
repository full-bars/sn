package provider

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestControlSocketSnapshotEndToEnd(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "snapshot"})
	if err != nil {
		t.Fatalf("dial snapshot: %v", err)
	}
	if !resp.OK || resp.Snapshot == nil {
		t.Fatalf("snapshot response: %+v", resp)
	}
	s := resp.Snapshot
	if s.V != 1 || s.Version == "" || s.Rate.HistoryBps == nil || s.Resources.Goroutines < 1 {
		t.Fatalf("snapshot incomplete: %+v", s)
	}
	switch s.State {
	case "starting", "degraded", "idle", "flowing":
	default:
		t.Fatalf("state = %q, not one of the contract states", s.State)
	}
	switch s.Restart.Reason {
	case "update", "hotswap", "manual", "clean", "unclean", "first-start":
	default:
		t.Fatalf("restart reason = %q, not one of the contract reasons", s.Restart.Reason)
	}
}

// Existing commands must not start carrying a snapshot.
func TestControlResponseSnapshotOnlyOnSnapshotCommand(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()
	for _, cmd := range []string{"version", "status"} {
		resp := handleControlRequest(globalControlState, controlRequest{Cmd: cmd})
		b, err := json.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"snapshot"`) {
			t.Errorf("%s response carries a snapshot: %s", cmd, b)
		}
	}
}

func TestWriteNodeGauges(t *testing.T) {
	var b strings.Builder
	writeNodeGauges(&b, "update", SnapshotResources{
		MemLimitBytes: 4 << 30, RSSBytes: 123, OpenFDs: 45, FDLimit: 1024,
	})
	out := b.String()
	for _, want := range []string{
		"# TYPE urnet_restart_reason gauge\n",
		`urnet_restart_reason{reason="update"} 1` + "\n",
		"urnet_mem_limit_bytes 4294967296\n",
		"urnet_rss_bytes 123\n",
		"urnet_open_fds 45\n",
		"urnet_fd_limit 1024\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if errs := Lint(out); len(errs) != 0 {
		t.Errorf("lint: %v", errs)
	}
}

func TestWriteNodeGaugesOmitsUnknown(t *testing.T) {
	var b strings.Builder
	writeNodeGauges(&b, "", SnapshotResources{HeapInuseBytes: 1, Goroutines: 1})
	if out := b.String(); out != "" {
		t.Errorf("expected no gauges for unknown values, got:\n%s", out)
	}
}

// The gauges join an existing family list: none of the old names may move.
// sn serves the whole /metrics payload from providerExtraMetrics, so the
// scrape is taken through the same handler the provider listens with.
func TestProviderMetricsIncludeNodeGaugesAndKeepExisting(t *testing.T) {
	withTempHome(t)
	stubSetExtraMetricsProvider(providerExtraMetrics)
	t.Cleanup(func() { stubSetExtraMetricsProvider(nil) })

	w := httptest.NewRecorder()
	prometheusHandlerStub().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()

	// urnet_sessions_pqe and urnet_sessions_classical are left out: sn only
	// exports them once session counts have been measured.
	for _, name := range []string{
		"urnet_info", "urnet_startup_clean_shutdown", "urnet_startup_restarted",
		"urnet_startup_upgraded", "urnet_control_commands_total",
		"urnet_pressure_score", "urnet_doh_failures_total",
		"urnet_restart_reason", "urnet_uptime_seconds", "urnet_mem_sys_bytes",
	} {
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("family %s missing from scrape", name)
		}
	}
	if n := strings.Count(body, "# TYPE urnet_uptime_seconds "); n != 1 {
		t.Errorf("urnet_uptime_seconds declared %d times, want 1", n)
	}
	for _, e := range Lint(body) {
		t.Error(e)
	}
}

func TestWriteRuntimeGauges(t *testing.T) {
	var b strings.Builder
	writeRuntimeGauges(&b, 90*time.Second, 1<<20)
	out := b.String()
	for _, want := range []string{
		"# TYPE urnet_uptime_seconds gauge\n",
		"urnet_uptime_seconds 90\n",
		"# TYPE urnet_mem_sys_bytes gauge\n",
		"urnet_mem_sys_bytes 1048576\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if errs := Lint(out); len(errs) != 0 {
		t.Errorf("lint: %v", errs)
	}

	b.Reset()
	writeRuntimeGauges(&b, time.Second, 0)
	if strings.Contains(b.String(), "urnet_mem_sys_bytes") {
		t.Errorf("unknown memory figure exported:\n%s", b.String())
	}
}
