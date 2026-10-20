package miner

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testControlSocket starts the control socket in a fresh state directory and
// returns the path plus a cleanup.
func testControlSocket(t *testing.T, state *controlState) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	ctx, cancel := context.WithCancel(context.Background())
	cleanup, err := startControlSocket(ctx, state)
	if err != nil {
		cancel()
		t.Fatalf("startControlSocket: %v", err)
	}
	path, err := controlSocketPath()
	if err != nil {
		cancel()
		t.Fatalf("controlSocketPath: %v", err)
	}
	return path, func() {
		cleanup()
		cancel()
	}
}

// controlExchange sends raw bytes and returns the one response line.
func controlExchange(t *testing.T, path, raw string) string {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(raw + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasSuffix(line, "\n") {
		t.Fatalf("response is not newline-terminated: %q", line)
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("response is not one line: %q", line)
	}
	return strings.TrimSuffix(line, "\n")
}

func controlRequestJSON(t *testing.T, path string, req controlRequest) controlResponse {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	line := controlExchange(t, path, string(data))
	var resp controlResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal response %q: %v", line, err)
	}
	return resp
}

func TestControlSocketSetGetClearRoundTrip(t *testing.T) {
	state := newControlState()
	path, cleanup := testControlSocket(t, state)
	defer cleanup()

	resp := controlRequestJSON(t, path, controlRequest{Cmd: "set", Key: "proxy_url_max", Value: "42", V: 1})
	if !resp.OK {
		t.Fatalf("set failed: %s", resp.Error)
	}
	if resp.Version != 1 {
		t.Fatalf("protocol version not echoed: %d", resp.Version)
	}
	if resp.NeedsRestart {
		t.Fatalf("proxy_url_max is a live key; needs_restart must be false")
	}

	resp = controlRequestJSON(t, path, controlRequest{Cmd: "get", Key: "proxy_url_max"})
	if !resp.OK || !resp.Found || resp.Value != "42" {
		t.Fatalf("get after set = %+v", resp)
	}

	// The value is persisted in the provider_state.json v2 envelope.
	statePath, err := controlStatePath()
	if err != nil {
		t.Fatalf("controlStatePath: %v", err)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var envelope controlStateEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	if envelope.Version != 2 || envelope.Values["proxy_url_max"] != "42" {
		t.Fatalf("persisted envelope = %+v", envelope)
	}

	resp = controlRequestJSON(t, path, controlRequest{Cmd: "clear", Key: "proxy_url_max"})
	if !resp.OK {
		t.Fatalf("clear failed: %s", resp.Error)
	}
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "get", Key: "proxy_url_max"})
	if !resp.OK || resp.Found {
		t.Fatalf("get after clear = %+v", resp)
	}
}

func TestControlSocketRestartOnlyKeyReportsRestart(t *testing.T) {
	state := newControlState()
	path, cleanup := testControlSocket(t, state)
	defer cleanup()

	// profile has no live consumer in the miner: it must persist and tell the
	// operator a restart is required.
	resp := controlRequestJSON(t, path, controlRequest{Cmd: "set", Key: "profile", Value: "eco"})
	if !resp.OK {
		t.Fatalf("set profile failed: %s", resp.Error)
	}
	if !resp.NeedsRestart {
		t.Fatalf("profile must report needs_restart")
	}
	// v4 canonicalizes to turbo-v4.
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "set", Key: "profile", Value: "v4"})
	if !resp.OK {
		t.Fatalf("set profile v4 failed: %s", resp.Error)
	}
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "get", Key: "profile"})
	if resp.Value != "turbo-v4" {
		t.Fatalf("profile alias not canonicalized: %q", resp.Value)
	}
}

func TestControlSocketValidationAndCaps(t *testing.T) {
	state := newControlState()
	path, cleanup := testControlSocket(t, state)
	defer cleanup()

	// Unknown key and unknown command are structured errors.
	if resp := controlRequestJSON(t, path, controlRequest{Cmd: "get", Key: "nope"}); resp.OK {
		t.Fatalf("unknown key accepted")
	}
	if resp := controlRequestJSON(t, path, controlRequest{Cmd: "frobnicate"}); resp.OK {
		t.Fatalf("unknown command accepted")
	}

	// A set value over 4 KiB is refused before any state change.
	big := strings.Repeat("x", maxControlSetValue+1)
	resp := controlRequestJSON(t, path, controlRequest{Cmd: "set", Key: "node_name", Value: big})
	if resp.OK || !strings.Contains(resp.Error, "4 KiB") {
		t.Fatalf("oversized set = %+v", resp)
	}

	// Server-side validation mirrors the client grammar.
	for _, tc := range []struct {
		key, value string
	}{
		{"proxy_url_refresh", "1s"},
		{"proxy_dead_cleanup_scope", "sometimes"},
		{"metrics", "maybe"},
		{"hot_restart", "sure"},
		{"node_name", "bad\nname"},
	} {
		r := controlRequestJSON(t, path, controlRequest{Cmd: "set", Key: tc.key, Value: tc.value})
		if r.OK {
			t.Fatalf("invalid %s=%q accepted", tc.key, tc.value)
		}
	}

	// Version negotiation: a newer protocol version is rejected with the
	// supported version echoed.
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "status", V: 2})
	if resp.OK || resp.Error != "unsupported_version" || resp.Version != 1 {
		t.Fatalf("version negotiation = %+v", resp)
	}
}

func TestControlSocketVersionStatusHistoryShutdown(t *testing.T) {
	state := newControlState()
	path, cleanup := testControlSocket(t, state)
	defer cleanup()

	resp := controlRequestJSON(t, path, controlRequest{Cmd: "version"})
	if !resp.OK || resp.BuildVersion == "" {
		t.Fatalf("version = %+v", resp)
	}

	// A set records a history entry; history returns it newest-first.
	if r := controlRequestJSON(t, path, controlRequest{Cmd: "set", Key: "metrics", Value: "off"}); !r.OK {
		t.Fatalf("set metrics off failed: %s", r.Error)
	}
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "history", Limit: 10})
	if !resp.OK || len(resp.Entries) == 0 {
		t.Fatalf("history = %+v", resp)
	}
	if resp.Entries[0].Cmd != "set" || resp.Entries[0].Key != "metrics" {
		t.Fatalf("newest history entry = %+v", resp.Entries[0])
	}

	// status reports the full key surface and the persisted value.
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "status"})
	if !resp.OK {
		t.Fatalf("status failed: %s", resp.Error)
	}
	if len(resp.Settings) != len(controlKeys) {
		t.Fatalf("status has %d settings, want %d", len(resp.Settings), len(controlKeys))
	}
	if got := resp.Settings["metrics"].Value; got != "off" {
		t.Fatalf("status metrics = %q", got)
	}

	// ledger on a state directory with no autopilot ledger is an empty list.
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "ledger"})
	if !resp.OK || resp.Value != "[]" {
		t.Fatalf("ledger = %+v", resp)
	}

	// shutdown invokes the configured function.
	shutdown := make(chan struct{})
	state.shutdownFn = func() { close(shutdown) }
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "shutdown"})
	if !resp.OK {
		t.Fatalf("shutdown = %+v", resp)
	}
	select {
	case <-shutdown:
	case <-time.After(3 * time.Second):
		t.Fatalf("shutdown function not called")
	}
}

func TestControlSocketFileMode(t *testing.T) {
	state := newControlState()
	path, cleanup := testControlSocket(t, state)
	defer cleanup()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o, want 600", info.Mode().Perm())
	}
}

func TestControlSocketRefusesLiveListener(t *testing.T) {
	state := newControlState()
	_, cleanup := testControlSocket(t, state)
	defer cleanup()

	// A second provider must not steal the socket from a live one.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if _, err := startControlSocket(ctx2, state); err == nil {
		t.Fatalf("second control socket start succeeded against a live listener")
	}
}

func TestRemoveStaleSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provider.sock")

	// A missing file is fine.
	if err := removeStaleSocket(path); err != nil {
		t.Fatalf("missing socket: %v", err)
	}
	// A leftover file with nothing listening is removed.
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	if err := removeStaleSocket(path); err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale socket file still present")
	}
}

func TestWriteControlResponseRefusesOversize(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	go writeControlResponse(server, controlResponse{OK: true, Value: strings.Repeat("x", maxControlResponseBytes+64)})

	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(client).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(line) > maxControlResponseBytes {
		t.Fatalf("oversized response was written (%d bytes)", len(line))
	}
	var resp controlResponse
	if err := json.Unmarshal([]byte(strings.TrimSuffix(line, "\n")), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.OK || !strings.Contains(resp.Error, "response too large") {
		t.Fatalf("oversize handling = %+v", resp)
	}
}

func TestMergePendingControlOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)

	state := newControlState()
	state.setWithValue("gogc", "50", SourceSocket)
	queue := `[{"op":"set","key":"proxy_url_max","value":"7"},{"op":"clear","key":"gogc"}]`
	if err := os.WriteFile(filepath.Join(dir, "pending_overrides.json"), []byte(queue), 0o600); err != nil {
		t.Fatalf("write queue: %v", err)
	}
	mergePendingControlOverrides(state)

	if v, ok := state.get("proxy_url_max"); !ok || v != "7" {
		t.Fatalf("pending set not applied: %q %v", v, ok)
	}
	if _, ok := state.get("gogc"); ok {
		t.Fatalf("pending clear not applied")
	}
	if _, err := os.Stat(filepath.Join(dir, "pending_overrides.json")); !os.IsNotExist(err) {
		t.Fatalf("pending queue not removed")
	}
}

func TestHotRestartEnabledResolution(t *testing.T) {
	state := newControlState()
	saved := globalControlState
	globalControlState = state
	defer func() { globalControlState = saved }()

	t.Setenv("URNETWORK_HOT_RESTART", "")
	if !hotRestartEnabled() {
		t.Fatalf("default must be on")
	}
	t.Setenv("URNETWORK_HOT_RESTART", "0")
	if hotRestartEnabled() {
		t.Fatalf("URNETWORK_HOT_RESTART=0 must disable")
	}
	state.setWithValue("hot_restart", "on", SourceSocket)
	if !hotRestartEnabled() {
		t.Fatalf("persisted hot_restart=on must beat the env var")
	}
	state.setWithValue("hot_restart", "off", SourceSocket)
	if hotRestartEnabled() {
		t.Fatalf("persisted hot_restart=off must disable")
	}
}

func TestDialControlSocketRoundTrip(t *testing.T) {
	state := newControlState()
	_, cleanup := testControlSocket(t, state)
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "version"})
	if err != nil {
		t.Fatalf("dialControlSocket: %v", err)
	}
	if !resp.OK || resp.BuildVersion == "" {
		t.Fatalf("dial version = %+v", resp)
	}
}
