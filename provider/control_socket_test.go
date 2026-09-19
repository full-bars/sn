package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"
)

func resetGlobalControlStateForTest() {
	globalControlState.txMu.Lock()
	defer globalControlState.txMu.Unlock()
	globalControlState.replaceAll(map[string]string{})
}

func captureTlog(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out)
}

func captureControlApplyLog(fn func(string, ...any)) func() {
	orig := controlApplyLog
	controlApplyLog = fn
	return func() { controlApplyLog = orig }
}

func readFileString(name string) (string, error) {
	b, err := os.ReadFile(name)
	return string(b), err
}

func TestControlSocket_SetGetClear_EndToEnd(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "set", Key: "node_name", Value: "nyc-1"})
	if err != nil {
		t.Fatalf("dial set: %v", err)
	}
	if !resp.OK {
		t.Fatalf("set response: %+v", resp)
	}

	resp, err = dialControlSocket(controlRequest{Cmd: "get", Key: "node_name"})
	if err != nil {
		t.Fatalf("dial get: %v", err)
	}
	if !resp.OK || !resp.Found || resp.Value != "nyc-1" {
		t.Fatalf("get response: %+v", resp)
	}

	// The live provider read path sees it immediately — no restart, no poll.
	if got := resolveNodeName("startup-host"); got != "nyc-1" {
		t.Fatalf("resolveNodeName after socket set = %q, want %q", got, "nyc-1")
	}

	resp, err = dialControlSocket(controlRequest{Cmd: "clear", Key: "node_name"})
	if err != nil {
		t.Fatalf("dial clear: %v", err)
	}
	if !resp.OK {
		t.Fatalf("clear response: %+v", resp)
	}
	if got := resolveNodeName("startup-host"); got != "startup-host" {
		t.Fatalf("resolveNodeName after clear = %q, want startup default", got)
	}
}

func TestControlSocket_UnknownKeyRejected(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "set", Key: "not-a-real-key", Value: "x"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp.OK {
		t.Fatalf("expected rejection for unknown key, got %+v", resp)
	}
}

func TestControlSocket_UnknownCommandRejected(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "delete-everything", Key: "node_name"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp.OK {
		t.Fatalf("expected rejection for unknown command, got %+v", resp)
	}
}

func TestDialControlSocket_NoProviderRunning(t *testing.T) {
	withTempHome(t)

	_, err := dialControlSocket(controlRequest{Cmd: "get", Key: "node_name"})
	if err != errNoProvider {
		t.Fatalf("got err=%v, want errNoProvider", err)
	}
}

func TestStartControlSocket_RemovesStaleSocketFile(t *testing.T) {
	home := withTempHome(t)
	resetGlobalControlStateForTest()

	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stalePath := filepath.Join(dir, "provider.sock")
	if err := os.WriteFile(stalePath, nil, 0o600); err != nil {
		t.Fatalf("write stale socket file: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket should reclaim a stale socket file, got: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "get", Key: "node_name"})
	if err != nil {
		t.Fatalf("dial after reclaiming stale socket: %v", err)
	}
	if !resp.OK {
		t.Fatalf("get response: %+v", resp)
	}
}

func TestStartControlSocket_RefusesWhenAlreadyListening(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	_, err = startControlSocket(ctx, newControlState())
	if err == nil {
		t.Fatalf("expected error starting a second listener on the same socket")
	}
}

func TestStartControlSocket_SocketFilePermissions(t *testing.T) {
	home := withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	path := filepath.Join(home, ".urnetwork", "provider.sock")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket perms = %o, want 0600 (owner-only)", perm)
	}
}

func TestControlSocket_PersistFailureRollsBackInMemoryState(t *testing.T) {
	home := withTempHome(t)
	resetGlobalControlStateForTest()
	globalControlState.set("node_name", "old-value")

	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	resp := handleControlRequest(globalControlState, controlRequest{Cmd: "set", Key: "node_name", Value: "new-value"})
	if resp.OK {
		t.Fatalf("expected persist failure to surface as an error, got %+v", resp)
	}
	if v, _ := globalControlState.get("node_name"); v != "old-value" {
		t.Fatalf("in-memory state after failed persist = %q, want rollback to %q", v, "old-value")
	}
}

func TestControlSocket_ConcurrentSetsMemoryMatchesDisk(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	const writers = 12
	const rounds = 40
	written := make(map[string]bool, writers*rounds)
	var writtenMu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				value := fmt.Sprintf("w%d-%d", w, i)
				writtenMu.Lock()
				written[value] = true
				writtenMu.Unlock()
				handleControlRequest(globalControlState, controlRequest{
					Cmd: "set", Key: "node_name", Value: value,
				})
			}
		}(w)
	}
	wg.Wait()

	memVal, memFound := globalControlState.get("node_name")
	if !memFound {
		t.Fatalf("node_name should be set after concurrent sets")
	}

	reloaded, err := loadControlState()
	if err != nil {
		t.Fatalf("loadControlState: %v", err)
	}
	diskVal, diskFound := reloaded.get("node_name")
	if !diskFound {
		t.Fatalf("node_name should be persisted after concurrent sets")
	}
	if memVal != diskVal {
		t.Fatalf("memory = %q, disk = %q — concurrent set/persist lost an update", memVal, diskVal)
	}
	if !written[memVal] {
		t.Fatalf("final value %q was never written by any writer — corrupted by the concurrent set/persist path", memVal)
	}
}

func TestIsTruthyOn(t *testing.T) {
	on := []string{"on", "1", "true", "yes"}
	off := []string{"off", "0", "false", "no", "", "2", "bogus"}
	for _, v := range on {
		if !isTruthyOn(v) {
			t.Errorf("isTruthyOn(%q) = false, want true", v)
		}
	}
	for _, v := range off {
		if isTruthyOn(v) {
			t.Errorf("isTruthyOn(%q) = true, want false", v)
		}
	}
}

func TestHotRestartEnabled_GuessBooleanForms(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	cases := []struct {
		stored string
		want   bool
	}{
		{"off", false}, {"0", false}, {"false", false}, {"no", false},
		{"on", true}, {"1", true}, {"true", true}, {"yes", true},
		{"bogus", true},
	}
	for _, tc := range cases {
		globalControlState.set("hot_restart", tc.stored)
		if got := hotRestartEnabled(); got != tc.want {
			t.Errorf("hot_restart=%q -> hotRestartEnabled()=%v, want %v", tc.stored, got, tc.want)
		}
	}
	globalControlState.clear("hot_restart")
	if got := hotRestartEnabled(); got != true {
		t.Errorf("cleared hot_restart (env unset) -> hotRestartEnabled()=%v, want true", got)
	}
}

func TestControlSocket_WaitForReleaseUnblocksWhenListenerGone(t *testing.T) {
	withTempHome(t)
	path, err := controlSocketPath()
	if err != nil {
		t.Fatalf("controlSocketPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	done := make(chan struct{})
	go func() {
		waitForControlSocketRelease(5 * time.Second)
		close(done)
	}()
	select {
	case <-done:
		t.Fatalf("waitForControlSocketRelease returned while the listener was still live")
	case <-time.After(300 * time.Millisecond):
	}

	ln.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("waitForControlSocketRelease did not return after the listener closed")
	}
}

func TestControlSocketLogsSetAndClear(t *testing.T) {
	withTempHome(t)
	s := newControlState()

	out := captureTlog(t, func() {
		if resp := handleControlRequest(s, controlRequest{Cmd: "set", Key: "node_name", Value: "nyc-1"}); !resp.OK {
			t.Fatalf("set failed: %s", resp.Error)
		}
	})
	if !strings.Contains(out, "[control] set node_name=nyc-1") {
		t.Errorf("first set must be logged, got: %q", out)
	}
	if !strings.Contains(out, "was unset") {
		t.Errorf("a key with no previous value must log 'was unset', got: %q", out)
	}

	out = captureTlog(t, func() {
		if resp := handleControlRequest(s, controlRequest{Cmd: "set", Key: "node_name", Value: "nyc-2"}); !resp.OK {
			t.Fatalf("second set failed: %s", resp.Error)
		}
	})
	if !strings.Contains(out, "set node_name=nyc-2") || !strings.Contains(out, "was nyc-1") {
		t.Errorf("overwrite must log the previous value, got: %q", out)
	}

	out = captureTlog(t, func() {
		if resp := handleControlRequest(s, controlRequest{Cmd: "clear", Key: "node_name"}); !resp.OK {
			t.Fatalf("clear failed: %s", resp.Error)
		}
	})
	if !strings.Contains(out, "cleared node_name") || !strings.Contains(out, "was nyc-2") {
		t.Errorf("clear must be logged with the previous value, got: %q", out)
	}
}

func TestControlSocketLogsRejectedSet(t *testing.T) {
	withTempHome(t)
	s := newControlState()

	out := captureTlog(t, func() {
		if resp := handleControlRequest(s, controlRequest{Cmd: "set", Key: "not_a_real_key", Value: "x"}); resp.OK {
			t.Fatal("an unknown control key must not be accepted")
		}
	})
	if !strings.Contains(out, "not_a_real_key") || !strings.Contains(out, "rejected") {
		t.Errorf("a rejected set must be logged, got: %q", out)
	}
}

func TestControlSocketGetIsNotLogged(t *testing.T) {
	withTempHome(t)
	s := newControlState()
	if err := s.set("node_name", "nyc-1"); err != nil {
		t.Fatal(err)
	}

	out := captureTlog(t, func() {
		if resp := handleControlRequest(s, controlRequest{Cmd: "get", Key: "node_name"}); !resp.OK {
			t.Fatalf("get failed: %s", resp.Error)
		}
	})
	if strings.TrimSpace(out) != "" {
		t.Errorf("get must not log, got: %q", out)
	}
}

func TestDashboardLabelLoggedOnceAndOnChange(t *testing.T) {
	lastDashboardLabelMu.Lock()
	lastDashboardLabel = ""
	lastDashboardLabelMu.Unlock()

	first := captureTlog(t, func() { logDashboardLabel("nyc-1 [v1]") })
	if !strings.Contains(first, "dashboard label: nyc-1 [v1]") {
		t.Errorf("first resolve must log the label, got: %q", first)
	}

	repeat := captureTlog(t, func() {
		logDashboardLabel("nyc-1 [v1]")
		logDashboardLabel("nyc-1 [v1]")
	})
	if strings.TrimSpace(repeat) != "" {
		t.Errorf("an unchanged label must stay quiet, got: %q", repeat)
	}

	changed := captureTlog(t, func() { logDashboardLabel("nyc-2 [v1]") })
	if !strings.Contains(changed, "nyc-1 [v1] -> nyc-2 [v1]") {
		t.Errorf("a changed label must log the transition, got: %q", changed)
	}
}

func TestValidateControlValue(t *testing.T) {
	tests := []struct {
		key, value string
		wantErr    bool
	}{
		{"node_name", "nyc-1", false},
		{"report_url", "https://example.com", false},
		{"report_url", "", false},
		{"profile", "turbo-v4", false},
		{"profile", "auto", false},
		{"profile", "eco", false},
		{"profile", "lowmem", false},
		{"profile", "turbo-v8", false},
		{"profile", "v4", false},
		{"profile", "v8", false},
		{"ramlogs", "on", false},
		{"ramlogs", "off", false},
		{"fast_auth", "on", false},
		{"fast_auth", "off", false},
		{"proxy_self_heal", "on", false},
		{"proxy_self_heal", "off", false},
		{"hot_restart", "on", false},
		{"hot_restart", "off", false},
		{"hot_restart", "true", false},
		{"hot_restart", "false", false},
		{"hot_restart", "yes", false},
		{"hot_restart", "no", false},
		{"hot_restart", "1", false},
		{"hot_restart", "0", false},
		{"report_interval", "30s", false},
		{"report_interval", "5m", false},
		{"report_interval", "1h", false},
		{"proxy_url_refresh", "10s", false},
		{"proxy_dead_cleanup_interval", "1m", false},
		{"proxy_dead_cleanup_interval", "5m", false},
		{"proxy_dead_cleanup_scope", "none", false},
		{"proxy_dead_cleanup_scope", "url", false},
		{"proxy_dead_cleanup_scope", "all", false},
		{"proxy_url_max", "0", false},
		{"proxy_url_max", "50", false},
		{"gomemlimit", "2048mib", false},
		{"gomemlimit", "2gib", false},
		{"gogc", "50", false},
		{"gogc", "off", false},

		{"node_name", "", true},
		{"node_name", "has\x00null", true},
		{"profile", "turbo", true},
		{"profile", "turbo-v6", true},
		{"ramlogs", "maybe", true},
		{"fast_auth", "1", true},
		{"proxy_self_heal", "true", true},
		{"hot_restart", "y", true},
		{"report_interval", "5", true},
		{"report_interval", "1s", true},
		{"proxy_url_refresh", "invalid", true},
		{"proxy_dead_cleanup_interval", "30s", true},
		{"proxy_dead_cleanup_scope", "all-proxies", true},
		{"proxy_url_max", "-1", true},
		{"proxy_url_max", "abc", true},
		{"gogc", "-1", true},
		{"report_url", "bad url", true},
		{"report_url", "bad\nurl", true},
		{"fast_auth", "ON", false},
		{"fast_auth", "Off", false},
		{"proxy_self_heal", "ON", false},
		{"proxy_self_heal", "Off", false},
		{"proxy_self_heal", "TRUE", true},
		{"ramlogs", "YES", true},
		{"ramlogs", "On", false},
		{"hot_restart", "TRUE", false},
		{"hot_restart", "No", false},
		{"profile", "AUTO", false},
		{"profile", "Turbo-V4", false},
		{"profile", "ECO", false},
		{"gogc", "OFF", false},
		{"gogc", "Off", false},
		{"unknown_key", "anything", false},
	}

	for _, tt := range tests {
		err := validateControlValue(tt.key, tt.value)
		if (err != nil) != tt.wantErr {
			t.Errorf("validateControlValue(%q, %q): err=%v, wantErr=%v", tt.key, tt.value, err, tt.wantErr)
		}
	}
}

func TestNeedsRestart_AutoComputed(t *testing.T) {
	liveKeys := []string{
		"gomemlimit", "gogc",
		"fast_auth", "proxy_self_heal",
		"report_url", "report_interval",
		"proxy_url_refresh", "proxy_url_max",
		"proxy_dead_cleanup_scope", "proxy_dead_cleanup_interval",
		"node_name", "hot_restart",
	}
	for _, k := range liveKeys {
		if needsRestart(k) {
			t.Errorf("needsRestart(%q) = true, expected false (has live side effect)", k)
		}
	}

	restartKeys := []string{"profile", "ramlogs"}
	for _, k := range restartKeys {
		if !needsRestart(k) {
			t.Errorf("needsRestart(%q) = false, expected true (no live side effect)", k)
		}
	}
}

func TestServerSideValidation_RejectsInvalidViaSocket(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "set", Key: "node_name", Value: "nyc-1"})
	if err != nil {
		t.Fatalf("dial set: %v", err)
	}
	if !resp.OK {
		t.Fatalf("valid set should succeed: %v", resp.Error)
	}
	if resp.NeedsRestart {
		t.Errorf("node_name should NOT need restart (live key)")
	}

	resp, err = dialControlSocket(controlRequest{Cmd: "set", Key: "profile", Value: "turbo"})
	if err != nil {
		t.Fatalf("dial set: %v", err)
	}
	if resp.OK {
		t.Errorf("invalid profile value 'turbo' should be rejected, got OK=true")
	}
	if !strings.Contains(resp.Error, "must be auto") {
		t.Errorf("error should mention valid options, got: %s", resp.Error)
	}

	resp, err = dialControlSocket(controlRequest{Cmd: "set", Key: "gomemlimit", Value: "2048mib"})
	if err != nil {
		t.Fatalf("dial set: %v", err)
	}
	if !resp.OK {
		t.Fatalf("valid gomemlimit set should succeed: %v", resp.Error)
	}
	if resp.NeedsRestart {
		t.Errorf("gomemlimit should NOT need restart (applies live)")
	}

	resp, err = dialControlSocket(controlRequest{Cmd: "set", Key: "profile", Value: "eco"})
	if err != nil {
		t.Fatalf("dial set: %v", err)
	}
	if !resp.OK {
		t.Fatalf("valid profile set should succeed: %v", resp.Error)
	}
	if !resp.NeedsRestart {
		t.Errorf("profile should need restart")
	}
}

func TestClearLiveKey_ReappliesDefault(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "set", Key: "gomemlimit", Value: "256mib"})
	if err != nil {
		t.Fatalf("dial set: %v", err)
	}
	if !resp.OK {
		t.Fatalf("set gomemlimit failed: %v", resp.Error)
	}

	resp, err = dialControlSocket(controlRequest{Cmd: "clear", Key: "gomemlimit"})
	if err != nil {
		t.Fatalf("dial clear: %v", err)
	}
	if !resp.OK {
		t.Fatalf("clear gomemlimit failed: %v", resp.Error)
	}
	if resp.NeedsRestart {
		t.Errorf("clear gomemlimit should NOT need restart (default applied live)")
	}

	resp, err = dialControlSocket(controlRequest{Cmd: "set", Key: "gogc", Value: "50"})
	if err != nil {
		t.Fatalf("dial set: %v", err)
	}
	if !resp.OK {
		t.Fatalf("set gogc failed: %v", resp.Error)
	}

	resp, err = dialControlSocket(controlRequest{Cmd: "clear", Key: "gogc"})
	if err != nil {
		t.Fatalf("dial clear: %v", err)
	}
	if !resp.OK {
		t.Fatalf("clear gogc failed: %v", resp.Error)
	}
	if resp.NeedsRestart {
		t.Errorf("clear gogc should NOT need restart (default applied live)")
	}

	resp, err = dialControlSocket(controlRequest{Cmd: "set", Key: "node_name", Value: "test"})
	if err != nil {
		t.Fatalf("dial set: %v", err)
	}

	resp, err = dialControlSocket(controlRequest{Cmd: "clear", Key: "node_name"})
	if err != nil {
		t.Fatalf("dial clear: %v", err)
	}
	if !resp.OK {
		t.Fatalf("clear node_name failed: %v", resp.Error)
	}
	if resp.NeedsRestart {
		t.Errorf("clear node_name should NOT need restart (live key)")
	}
}

func TestGogcOff_AppliesLive(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "set", Key: "gogc", Value: "OFF"})
	if err != nil {
		t.Fatalf("dial set: %v", err)
	}
	if !resp.OK {
		t.Fatalf("gogc=OFF should succeed: %v", resp.Error)
	}
	if resp.NeedsRestart {
		t.Errorf("gogc should NOT need restart (applies live)")
	}

	resp, err = dialControlSocket(controlRequest{Cmd: "set", Key: "gogc", Value: "Off"})
	if err != nil {
		t.Fatalf("dial set: %v", err)
	}
	if !resp.OK {
		t.Fatalf("gogc=Off should succeed: %v", resp.Error)
	}
}

func TestProfileAliasCanonicalization(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	aliases := map[string]string{
		"v4": "turbo-v4",
		"v8": "turbo-v8",
	}
	for alias, canonical := range aliases {
		resp, err := dialControlSocket(controlRequest{Cmd: "set", Key: "profile", Value: alias})
		if err != nil {
			t.Fatalf("set profile=%s: %v", alias, err)
		}
		if !resp.OK {
			t.Fatalf("profile=%s should be accepted: %s", alias, resp.Error)
		}
		val, found := globalControlState.get("profile")
		if !found || val != canonical {
			t.Errorf("profile=%s should persist as %q, got %q (found=%v)", alias, canonical, val, found)
		}
	}
}

func TestEveryControlKeyClassified(t *testing.T) {
	restartOnly := map[string]bool{
		"profile": true,
		"ramlogs": true,
	}

	for key := range controlKeys {
		isLive := liveEffectKeys[key]
		isRestart := restartOnly[key]
		if !isLive && !isRestart {
			t.Errorf("control key %q is neither in liveEffectKeys nor restartOnly — classify it", key)
		}
		if isLive && isRestart {
			t.Errorf("control key %q is in both liveEffectKeys and restartOnly — pick one", key)
		}
	}
}

func TestStatusCommand(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "set", Key: "node_name", Value: "nyc-1"})
	if err != nil {
		t.Fatalf("dial set: %v", err)
	}
	if !resp.OK {
		t.Fatalf("set response: %+v", resp)
	}

	resp, err = dialControlSocket(controlRequest{Cmd: "set", Key: "fast_auth", Value: "on"})
	if err != nil {
		t.Fatalf("dial set: %v", err)
	}
	if !resp.OK {
		t.Fatalf("set response: %+v", resp)
	}

	statusResp, err := dialControlSocket(controlRequest{Cmd: "status"})
	if err != nil {
		t.Fatalf("dial status: %v", err)
	}
	if !statusResp.OK {
		t.Fatalf("status response: %+v", statusResp)
	}
	if statusResp.Settings == nil {
		t.Fatalf("expected settings map in status response, got nil")
	}

	nodeInfo, ok := statusResp.Settings["node_name"]
	if !ok {
		t.Fatalf("expected node_name in settings map")
	}
	if nodeInfo.Value != "nyc-1" {
		t.Errorf("node_name value = %q, want nyc-1", nodeInfo.Value)
	}
	if nodeInfo.Source != string(SourceSocket) {
		t.Errorf("node_name source = %q, want %q", nodeInfo.Source, SourceSocket)
	}
	if nodeInfo.SetAt == nil || nodeInfo.SetAt.IsZero() {
		t.Errorf("node_name set_at should be set")
	}

	authInfo, ok := statusResp.Settings["fast_auth"]
	if !ok {
		t.Fatalf("expected fast_auth in settings map")
	}
	if authInfo.Value != "on" {
		t.Errorf("fast_auth value = %q, want on", authInfo.Value)
	}
	if authInfo.Source != string(SourceSocket) {
		t.Errorf("fast_auth source = %q, want %q", authInfo.Source, SourceSocket)
	}
	if authInfo.SetAt == nil || authInfo.SetAt.IsZero() {
		t.Errorf("fast_auth set_at should be set")
	}
}

func TestStatusCommand_EmptyState(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	statusResp, err := dialControlSocket(controlRequest{Cmd: "status"})
	if err != nil {
		t.Fatalf("dial status: %v", err)
	}
	if !statusResp.OK {
		t.Fatalf("status response on empty state: %+v", statusResp)
	}
	if statusResp.Error != "" {
		t.Errorf("expected no error on empty state status, got %q", statusResp.Error)
	}
	if len(statusResp.Settings) != len(controlKeys) {
		t.Errorf("expected %d settings on empty state (all controlKeys with defaults), got %d", len(controlKeys), len(statusResp.Settings))
	}
	for k, info := range statusResp.Settings {
		if info.Source != string(SourceDefault) && info.Source != string(SourceEnv) {
			t.Errorf("empty state key %q: expected source default/env, got %q", k, info.Source)
		}
	}
}

func TestControlSocket_VersionCommand_ReturnsBuildVersion(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	origVersion := Version
	Version = "1.2.3-test"
	t.Cleanup(func() { Version = origVersion })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "version"})
	if err != nil {
		t.Fatalf("dial version: %v", err)
	}
	if !resp.OK {
		t.Fatalf("version response not OK: %+v", resp)
	}
	if resp.BuildVersion != "1.2.3-test" {
		t.Errorf("BuildVersion = %q, want %q", resp.BuildVersion, "1.2.3-test")
	}
}

func TestControlSocket_VersionCommand_ReturnsDevWhenEmpty(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	origVersion := Version
	Version = ""
	t.Cleanup(func() { Version = origVersion })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "version"})
	if err != nil {
		t.Fatalf("dial version: %v", err)
	}
	if !resp.OK {
		t.Fatalf("version response not OK: %+v", resp)
	}
	if resp.BuildVersion != "dev" {
		t.Errorf("BuildVersion = %q, want %q", resp.BuildVersion, "dev")
	}
}

func TestAcceptLoopSurvivesTransientErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantStop bool
	}{
		{"file descriptors exhausted", syscallEMFILE(), false},
		{"listener closed", net.ErrClosed, true},
		{"closed wrapped", &net.OpError{Op: "accept", Err: net.ErrClosed}, true},
	} {
		if got := acceptLoopShouldStop(tc.err); got != tc.wantStop {
			t.Errorf("%s: acceptLoopShouldStop = %v, want %v", tc.name, got, tc.wantStop)
		}
	}
}

func syscallEMFILE() error {
	return &net.OpError{Op: "accept", Err: os.NewSyscallError("accept", errors.New("too many open files"))}
}

func TestHandlerSetsAWriteDeadline(t *testing.T) {
	b, err := os.ReadFile("control_socket.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "SetWriteDeadline") {
		t.Error("handleControlConn sets no write deadline; a client that stops reading blocks the response forever")
	}
}

func TestClearMetricsStopsTheListener(t *testing.T) {
	if _, ok := liveDefaults["metrics"]; !ok {
		t.Fatal("liveDefaults has no metrics entry, so clearing it cannot stop the listener")
	}
	if got := liveDefaults["metrics"]; !strings.EqualFold(got, "off") {
		t.Errorf("liveDefaults[metrics] = %q, want \"off\"", got)
	}
}

func TestClearReportsFailureAsFailure(t *testing.T) {
	b, err := readFileString("control_socket.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b, "OK: true, Error:") {
		t.Error("a clear path returns OK:true alongside an Error; callers checking OK see success")
	}
}

func TestClearGomemlimitRestoresUnlimitedNotZero(t *testing.T) {
	orig := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	if err := applyLiveDefault("gomemlimit"); err != nil {
		t.Fatalf("applyLiveDefault(gomemlimit): %v", err)
	}

	got := debug.SetMemoryLimit(-1)
	if got == 0 {
		t.Fatal("clearing gomemlimit set a zero-byte memory limit; the runtime would GC continuously and peg the CPU")
	}
	if got != math.MaxInt64 {
		t.Errorf("memory limit after clear = %d, want math.MaxInt64 (unlimited)", got)
	}
}

func TestApplyLiveSideEffectLogsTheEffectiveValue(t *testing.T) {
	orig := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	var logged []string
	restore := captureControlApplyLog(func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	t.Cleanup(restore)

	if err := applyLiveSideEffect("gomemlimit", "0"); err != nil {
		t.Fatalf("applyLiveSideEffect: %v", err)
	}

	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "gomemlimit") {
		t.Errorf("apply did not name the key in the log; got %q", joined)
	}
	if !strings.Contains(joined, "unlimited") {
		t.Errorf("apply did not report the effective value; got %q", joined)
	}
}

func TestGogcDisabledIsAcceptedAndDisablesCollection(t *testing.T) {
	origGC := debug.SetGCPercent(100)
	t.Cleanup(func() { debug.SetGCPercent(origGC) })

	if err := validateControlValue("gogc", "disabled"); err != nil {
		t.Fatalf("validateControlValue(gogc, disabled) = %v, want accepted", err)
	}

	var logged []string
	restore := captureControlApplyLog(func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	t.Cleanup(restore)

	if err := applyLiveSideEffect("gogc", "disabled"); err != nil {
		t.Fatalf("applyLiveSideEffect(gogc, disabled): %v", err)
	}

	if prev := debug.SetGCPercent(100); prev != -1 {
		t.Errorf("gogc after 'disabled' = %d, want -1 (collection disabled)", prev)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "disabled") {
		t.Errorf("apply did not announce that collection was disabled; got %q", logged)
	}
}

func TestCleanOncePreventsDoubleClose(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}

	// Cancel ctx — triggers internal goroutine's cleanOnce.Do(doClean).
	cancel()

	// Call cleanup — second entry into cleanOnce.Do, must be a no-op.
	// This must not panic or error from double-closing the listener/socket.
	cleanup()

	// Verify the socket file was removed (by either path).
	sockPath, err := controlSocketPath()
	if err != nil {
		t.Fatalf("controlSocketPath: %v", err)
	}
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("expected socket file to be removed, stat err = %v", err)
	}
}

func TestGetSetHotSwapTrigger(t *testing.T) {
	withTempHome(t)
	// Ensure clean state before and after test.
	setHotSwapTrigger(nil)
	t.Cleanup(func() { setHotSwapTrigger(nil) })

	// Initially nil.
	if got := getHotSwapTrigger(); got != nil {
		t.Fatal("expected nil trigger initially, got non-nil trigger")
	}

	// Set a trigger and verify it can be retrieved and called.
	called := make(chan struct{}, 1)
	setHotSwapTrigger(func() error {
		close(called)
		return nil
	})

	fn := getHotSwapTrigger()
	if fn == nil {
		t.Fatal("expected non-nil trigger after set")
	}
	if err := fn(); err != nil {
		t.Fatalf("trigger returned error: %v", err)
	}
	select {
	case <-called:
	default:
		t.Fatal("trigger function was not called")
	}

	// Unset — should return nil again.
	setHotSwapTrigger(nil)
	if got := getHotSwapTrigger(); got != nil {
		t.Fatal("expected nil after unsetting, got non-nil trigger")
	}

	// Concurrent set/get must not race (verified by -race detector).
	var wg sync.WaitGroup
	const goroutines = 20
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			setHotSwapTrigger(func() error { return nil })
			_ = getHotSwapTrigger()
		}(i)
	}
	wg.Wait()
}
