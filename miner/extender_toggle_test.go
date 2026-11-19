package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type toggleDeviceFake struct {
	held, enabled bool
	starts, stops int
}

func (d *toggleDeviceFake) HoldProvideExtenderSession(v bool) { d.held = v }
func (d *toggleDeviceFake) SetProvideExtender(v bool) {
	if d.enabled != v {
		if v {
			d.starts++
		} else {
			d.stops++
		}
	}
	d.enabled = v
}

func TestExtenderToggleLifecycle(t *testing.T) {
	d := &toggleDeviceFake{}
	budgetCalls := 0
	m := &extenderToggle{device: d, allow: func() bool { budgetCalls++; return true }}
	m.reconcile(false)
	if d.enabled || !d.held || budgetCalls != 0 {
		t.Fatalf("off startup: %+v calls %d", d, budgetCalls)
	}
	m.reconcile(true)
	if !d.enabled || d.held || d.starts != 1 {
		t.Fatalf("on: %+v", d)
	}
	m.reconcile(true)
	if budgetCalls != 1 {
		t.Fatalf("duplicate on spent budget: %d", budgetCalls)
	}
	m.reconcile(false)
	if d.enabled || !d.held || d.stops != 1 || m.cancel != nil {
		t.Fatalf("off: %+v", d)
	}
}
func TestExtenderToggleBudgetStopped(t *testing.T) {
	d := &toggleDeviceFake{}
	m := &extenderToggle{device: d, allow: func() bool { return false }}
	m.reconcile(true)
	if m.cancel == nil || !d.held {
		t.Fatal("budget hold not scheduled")
	}
	m.reconcile(false)
	if m.cancel != nil || !d.held || d.enabled {
		t.Fatal("operator off did not cancel check")
	}
}
func TestExtenderSettingPersistsAndRollback(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	state := newControlState()
	if !extenderSettingEnabled(state) {
		t.Fatal("default must be on")
	}
	if resp := handleControlRequest(state, controlRequest{Cmd: "set", Key: "extender", Value: "off"}); !resp.OK {
		t.Fatal(resp.Error)
	}
	loaded, err := loadControlState()
	if err != nil {
		t.Fatal(err)
	}
	if extenderSettingEnabled(loaded) {
		t.Fatal("off not restored")
	}
	t.Setenv("DISABLE_DIRECT_IP", "1")
	reading := providerExtenderControlForState(loaded)
	if reading == nil || !strings.Contains(reading.OffReason, "turned off") {
		t.Fatalf("reason: %+v", reading)
	}
	bad := filepath.Join(dir, "file")
	if err := os.WriteFile(bad, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("URNETWORK_STATE_DIR", bad)
	if resp := handleControlRequest(loaded, controlRequest{Cmd: "set", Key: "extender", Value: "on"}); resp.OK {
		t.Fatal("persist should fail")
	}
	if extenderSettingEnabled(loaded) {
		t.Fatal("failed persist changed setting")
	}
}
