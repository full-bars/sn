package miner

import (
	"errors"
	"os"
	"strings"
	"testing"
)

type mockExtenderDevice struct {
	extenderEnabled bool
	held            bool
	restartCount    int
}

func (d *mockExtenderDevice) HoldProvideExtenderSession(held bool) {
	d.held = held
}

func (d *mockExtenderDevice) SetProvideExtender(enabled bool) {
	d.extenderEnabled = enabled
	d.restartCount++
}

func TestExtenderCarrierRestartOnControlSetAndClear(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	state := newControlState()
	oldState := globalControlState
	globalControlState = state
	t.Cleanup(func() { globalControlState = oldState })

	dev := &mockExtenderDevice{extenderEnabled: true}
	gate := newExtenderActivationGate()
	detach := globalExtenderToggle.attach(dev, gate)
	t.Cleanup(detach)

	initialCount := dev.restartCount

	// Setting extender_udp443 to off must restart carriers and report live effect.
	res := handleControlRequest(state, controlRequest{Cmd: "set", Key: "extender_udp443", Value: "off"})
	if !res.OK {
		t.Fatalf("set extender_udp443=off failed: %s", res.Error)
	}
	if res.NeedsRestart {
		t.Fatalf("extender_udp443 must not require restart, got NeedsRestart=true")
	}
	if res.Effect != controlEffectLive {
		t.Fatalf("extender_udp443 effect = %q, want live", res.Effect)
	}
	if dev.restartCount <= initialCount {
		t.Fatalf("set did not restart carriers (count = %d, initial = %d)", dev.restartCount, initialCount)
	}

	// Setting to same value (off) must not restart again.
	countAfterFirst := dev.restartCount
	res = handleControlRequest(state, controlRequest{Cmd: "set", Key: "extender_udp443", Value: "off"})
	if !res.OK {
		t.Fatalf("set extender_udp443=off duplicate failed: %s", res.Error)
	}
	if dev.restartCount != countAfterFirst {
		t.Fatalf("redundant set caused unnecessary restart (count = %d, expected %d)", dev.restartCount, countAfterFirst)
	}

	// Clearing extender_udp443 must restart carriers to re-enable it.
	res = handleControlRequest(state, controlRequest{Cmd: "clear", Key: "extender_udp443"})
	if !res.OK {
		t.Fatalf("clear extender_udp443 failed: %s", res.Error)
	}
	if res.NeedsRestart {
		t.Fatalf("clear extender_udp443 must not require restart")
	}
	if dev.restartCount <= countAfterFirst {
		t.Fatalf("clear did not restart carriers (count = %d, previous = %d)", dev.restartCount, countAfterFirst)
	}
}

func TestExtenderCarrierRestartPersistFailureNoNeedlessRestart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	state := newControlState()
	oldState := globalControlState
	globalControlState = state
	t.Cleanup(func() { globalControlState = oldState })

	dev := &mockExtenderDevice{extenderEnabled: true}
	gate := newExtenderActivationGate()
	detach := globalExtenderToggle.attach(dev, gate)
	t.Cleanup(detach)

	initialCount := dev.restartCount

	// Make dir read-only so persist fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	// Set with failed persist rolls back in memory; restartCarriersIfChanged sees no difference,
	// so the healthy role is not restarted.
	res := handleControlRequest(state, controlRequest{Cmd: "set", Key: "extender_udp443", Value: "off"})
	if res.OK {
		t.Fatalf("expected set to fail persist, but got OK")
	}
	if dev.restartCount != initialCount {
		t.Fatalf("failed persist needlessly restarted healthy role (count = %d, initial = %d)", dev.restartCount, initialCount)
	}
}

func TestExtenderCarrierRestartReportsLiveApplyError(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	state := newControlState()
	oldState := globalControlState
	globalControlState = state
	t.Cleanup(func() { globalControlState = oldState })

	dev := &mockExtenderDevice{extenderEnabled: true}
	gate := newExtenderActivationGate()
	detach := globalExtenderToggle.attach(dev, gate)
	t.Cleanup(detach)

	// Inject a failure in restartRole.
	globalExtenderToggle.restartRole = func() error {
		return errors.New("simulated carrier restart failure")
	}
	t.Cleanup(func() { globalExtenderToggle.restartRole = nil })

	res := handleControlRequest(state, controlRequest{Cmd: "set", Key: "extender_udp4053", Value: "off"})
	if res.OK {
		t.Fatalf("expected set to report failure when restart fails, got OK")
	}
	if !strings.Contains(res.Error, "failed to apply carrier settings live") {
		t.Fatalf("unexpected error message: %q", res.Error)
	}
}
