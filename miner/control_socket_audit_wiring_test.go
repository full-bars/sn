package miner

// control_socket_audit_wiring_test.go pins the P2 wiring of the control
// socket's audit commands onto the live P1(c) audit engine (audit items
// CS-13/CS-14): `audit on|off` and `set|clear proxy_audit` must reach the
// running engine (the in-process manager map plus the live override, not just
// provider_state.json), `audit release` must give parked proxies back, and
// `mark` must write the operator annotation the unmodified client reads.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newAuditWiringState returns a control state in a fresh state directory with
// the process-wide audit state reset and a cleanup that clears the auditor
// pointer the socket handlers read.
func newAuditWiringState(t *testing.T) *controlState {
	t.Helper()
	withTempHome(t)
	prevStatus := currentProxyAuditStatus.Swap(nil)
	t.Cleanup(func() {
		currentProxyAuditor.Store(nil)
		currentProxyAuditStatus.Store(prevStatus)
		// The live override is process-wide; leaving it set would leak into
		// every later test's acting/observing split.
		clearProxyAuditOverride()
		resetGlobalControlStateForTest()
	})
	return newControlState()
}

// waitAuditRunOnceDrain waits for the immediate tick a control toggle spawns,
// so the test does not race the harness or leak the goroutine.
func waitAuditRunOnceDrain(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(runOnceGate) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the audit immediate tick did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestControlAuditOnOffReachesLiveAuditEngine(t *testing.T) {
	state := newAuditWiringState(t)
	h := newAuditHarness()
	currentProxyAuditor.Store(h.g)

	// The engine's resolution takes the in-process override first, then the
	// manager map; before the toggle it is off (the default).
	if resolveProxyAuditEnabled(false) {
		t.Fatal("audit must start off")
	}

	resp := handleControlRequest(state, controlRequest{Cmd: "audit", Action: "on"})
	if !resp.OK || resp.Value != "on" {
		t.Fatalf("audit on = %+v", resp)
	}
	if got, ok := state.get("proxy_audit"); !ok || got != "on" {
		t.Fatalf("audit on must persist the value, got %q ok=%v", got, ok)
	}
	if got, ok := globalProxyControlState.get("proxy_audit"); !ok || got != "on" {
		t.Fatalf("audit on must reach the engine's in-process map, got %q ok=%v", got, ok)
	}
	if !resolveProxyAuditEnabled(false) {
		t.Fatal("audit on must be visible to the running engine without a restart")
	}
	waitAuditRunOnceDrain(t)

	resp = handleControlRequest(state, controlRequest{Cmd: "audit", Action: "off"})
	if !resp.OK || resp.Value != "off" {
		t.Fatalf("audit off = %+v", resp)
	}
	if resolveProxyAuditEnabled(true) {
		t.Fatal("audit off must win over the startup value")
	}
	if got, ok := globalProxyControlState.get("proxy_audit"); !ok || got != "off" {
		t.Fatalf("audit off must reach the engine's in-process map, got %q ok=%v", got, ok)
	}
	waitAuditRunOnceDrain(t)

	// The persisted file carries the same value the engine sees.
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
	if envelope.Values["proxy_audit"] != "off" {
		t.Fatalf("persisted proxy_audit = %q, want off", envelope.Values["proxy_audit"])
	}
}

func TestControlSetClearProxyAuditReachesLiveAuditEngine(t *testing.T) {
	state := newAuditWiringState(t)
	h := newAuditHarness()
	currentProxyAuditor.Store(h.g)

	// `set proxy_audit on` is the same wiring as `audit on`, and it is a live
	// key now: no restart is required.
	resp := handleControlRequest(state, controlRequest{Cmd: "set", Key: "proxy_audit", Value: "on"})
	if !resp.OK {
		t.Fatalf("set proxy_audit failed: %s", resp.Error)
	}
	if resp.NeedsRestart {
		t.Fatal("proxy_audit is live once the audit engine consumes it; needs_restart must be false")
	}
	if !resolveProxyAuditEnabled(false) {
		t.Fatal("set proxy_audit on must reach the running engine")
	}
	waitAuditRunOnceDrain(t)

	// `clear proxy_audit` reapplies the runtime default (observe mode), not
	// merely a persisted delete.
	resp = handleControlRequest(state, controlRequest{Cmd: "clear", Key: "proxy_audit"})
	if !resp.OK {
		t.Fatalf("clear proxy_audit failed: %s", resp.Error)
	}
	if resp.NeedsRestart {
		t.Fatal("clearing a live key must not ask for a restart")
	}
	if resolveProxyAuditEnabled(true) {
		t.Fatal("clearing proxy_audit must release the live override and fall back to the startup value")
	}
	if _, ok := state.get("proxy_audit"); ok {
		t.Fatal("cleared key must be gone from the persisted state")
	}
	waitAuditRunOnceDrain(t)
}

func TestControlAuditReleaseReleasesParkedProxies(t *testing.T) {
	state := newAuditWiringState(t)
	u1, u2 := identityKey("gw.example:1080", "u1"), identityKey("gw.example:1080", "u2")
	survivor := identityKey("other.example:1080", "u1")
	h := newAuditHarness(u1, u2, survivor)
	currentProxyAuditor.Store(h.g)

	cfg := defaultProxyAuditConfig()
	for _, k := range []string{u1, u2, survivor} {
		until := h.g.st.commitPark(cfg, k, h.now, 0.5)
		h.hist.SetBackoffUntil(k, until)
		globalProxyFailureHistory.SetBackoffUntil(k, until)
	}

	resp := handleControlRequest(state, controlRequest{Cmd: "audit", Action: "release", Address: "gw.example:1080"})
	if !resp.OK {
		t.Fatalf("release failed: %s", resp.Error)
	}
	if !strings.Contains(resp.Value, "released proxy") {
		t.Fatalf("release value = %q, want the released label", resp.Value)
	}
	for _, k := range []string{u1, u2} {
		if h.g.st.isParked(k) {
			t.Errorf("%q still parked after release", k)
		}
		if !h.hist.Eligible(k, h.now) {
			t.Errorf("%q audit backoff not cleared", k)
		}
		if !globalProxyFailureHistory.Eligible(k, h.now.Add(time.Second)) {
			t.Errorf("%q global failure backoff not cleared", k)
		}
	}
	if !h.g.st.isParked(survivor) {
		t.Error("a proxy at a different address must stay parked")
	}
	if h.hist.Eligible(survivor, h.now.Add(time.Second)) {
		t.Error("a proxy at a different address must keep its audit backoff")
	}

	// --all releases the rest.
	resp = handleControlRequest(state, controlRequest{Cmd: "audit", Action: "release", Address: "--all"})
	if !resp.OK || resp.Value != "released 1 proxies" {
		t.Fatalf("release --all = %+v", resp)
	}
	if h.g.st.isParked(survivor) {
		t.Error("release --all must release every park")
	}

	// With no engine there is nothing to release; naming that beats pretending.
	currentProxyAuditor.Store(nil)
	resp = handleControlRequest(state, controlRequest{Cmd: "audit", Action: "release", Address: "gw.example:1080"})
	if resp.OK || !strings.Contains(resp.Error, "not active") {
		t.Fatalf("release without an auditor = %+v", resp)
	}
}

func TestControlAuditReleaseRequiresAddress(t *testing.T) {
	state := newAuditWiringState(t)
	h := newAuditHarness()
	currentProxyAuditor.Store(h.g)

	resp := handleControlRequest(state, controlRequest{Cmd: "audit", Action: "release"})
	if resp.OK || !strings.Contains(resp.Error, "requires an address or --all") {
		t.Fatalf("release without an address = %+v", resp)
	}
}

func TestControlAuditReleaseClearsBackoffWhenNotParked(t *testing.T) {
	state := newAuditWiringState(t)
	addr := "backoff.example:1080"
	h := newAuditHarness(addr)
	currentProxyAuditor.Store(h.g)

	globalProxyFailureHistory.SetBackoffUntil(addr, h.now.Add(time.Hour))
	resp := handleControlRequest(state, controlRequest{Cmd: "audit", Action: "release", Address: addr})
	if !resp.OK || !strings.Contains(resp.Value, "cleared backoff") {
		t.Fatalf("release of an unparks proxy = %+v", resp)
	}
	if !globalProxyFailureHistory.Eligible(addr, h.now) {
		t.Fatal("release must clear the failure backoff even when the proxy was not parked")
	}
}

func TestControlMarkRequiresALabelAndWritesTheBaselineLine(t *testing.T) {
	state := newAuditWiringState(t)

	resp := handleControlRequest(state, controlRequest{Cmd: "mark", Value: "   "})
	if resp.OK || !strings.Contains(resp.Error, "label is required") {
		t.Fatalf("unlabelled mark = %+v", resp)
	}

	resp = handleControlRequest(state, controlRequest{Cmd: "mark", Value: "  before-upgrade  "})
	if !resp.OK {
		t.Fatalf("mark failed: %s", resp.Error)
	}
	if _, err := time.Parse(time.RFC3339, resp.Value); err != nil {
		t.Fatalf("mark must report the recorded timestamp, got %q: %v", resp.Value, err)
	}

	path, err := baselinePath()
	if err != nil {
		t.Fatalf("baselinePath: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("baseline must hold one line, got %d: %q", len(lines), data)
	}
	var rec baselineMarkRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("parse mark: %v", err)
	}
	if rec.V != 1 || rec.Kind != "mark" || rec.Label != "before-upgrade" {
		t.Fatalf("mark record = %+v", rec)
	}
	if _, err := time.Parse(time.RFC3339, rec.TS); err != nil {
		t.Fatalf("mark ts %q is not RFC3339: %v", rec.TS, err)
	}

	// An empty directory label is refused before any file is created.
	if err := os.Remove(filepath.Join(filepath.Dir(path), "baseline.jsonl")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove baseline: %v", err)
	}
	if resp := handleControlRequest(state, controlRequest{Cmd: "mark", Value: ""}); resp.OK {
		t.Fatal("an empty label must be refused")
	}
}

// TestControlSocketAuditAndMarkOverTheSocket drives the new commands the way
// the unmodified client does: one JSON line, one reply line.
func TestControlSocketAuditAndMarkOverTheSocket(t *testing.T) {
	withTempHome(t)
	state := newControlState()
	prevStatus := currentProxyAuditStatus.Swap(nil)
	t.Cleanup(func() {
		currentProxyAuditor.Store(nil)
		currentProxyAuditStatus.Store(prevStatus)
		// The live override and the manager map are process-wide; leaving them
		// set would leak into every later test's acting/observing split.
		clearProxyAuditOverride()
		resetGlobalControlStateForTest()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, state)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()
	path, err := controlSocketPath()
	if err != nil {
		t.Fatalf("controlSocketPath: %v", err)
	}

	addr := "socket.example:1080"
	h := newAuditHarness(addr)
	currentProxyAuditor.Store(h.g)
	h.g.st.commitPark(defaultProxyAuditConfig(), addr, h.now, 0.5)
	h.hist.SetBackoffUntil(addr, h.now.Add(time.Hour))
	globalProxyFailureHistory.SetBackoffUntil(addr, h.now.Add(time.Hour))

	resp := controlRequestJSON(t, path, controlRequest{Cmd: "audit", Action: "release", Address: addr})
	if !resp.OK || !strings.Contains(resp.Value, "released proxy") {
		t.Fatalf("socket release = %+v", resp)
	}
	if h.g.st.isParked(addr) {
		t.Fatal("socket release must unpark the proxy")
	}

	resp = controlRequestJSON(t, path, controlRequest{Cmd: "mark", Value: "socket-mark"})
	if !resp.OK {
		t.Fatalf("socket mark = %+v", resp)
	}
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "audit", Action: "on"})
	if !resp.OK || resp.Value != "on" {
		t.Fatalf("socket audit on = %+v", resp)
	}
	if !resolveProxyAuditEnabled(false) {
		t.Fatal("socket audit on must reach the live engine")
	}
	waitAuditRunOnceDrain(t)
}

// TestProxyAuditResolvesFromPersistedStateAfterRestart pins the restart half
// of the wiring: `audit on` must still act after the process that recorded it
// is gone. The live override and the in-process manager map die with the
// process, so only provider_state.json - the persisted control state loaded
// on the next start - can carry the switch across the restart.
func TestProxyAuditResolvesFromPersistedStateAfterRestart(t *testing.T) {
	state := newAuditWiringState(t)
	if resp := handleControlRequest(state, controlRequest{Cmd: "audit", Action: "on"}); !resp.OK {
		t.Fatalf("audit on failed: %s", resp.Error)
	}

	// Simulate a restart: drop what was process-local and reload the file the
	// previous process persisted.
	clearProxyAuditOverride()
	resetGlobalControlStateForTest()
	reloaded, err := loadControlState()
	if err != nil {
		t.Fatalf("loadControlState: %v", err)
	}
	prevGlobal := globalControlState
	globalControlState = reloaded
	t.Cleanup(func() { globalControlState = prevGlobal })

	if !resolveProxyAuditEnabled(false) {
		t.Fatal("a persisted audit on must still act after a restart")
	}
}
