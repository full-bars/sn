package miner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func floatPtr(v float64) *float64 { return &v }

// The omission rules: an unknown field is absent, never a zero, because a
// zero reads as "measured, and the answer was nothing".
func TestBuildBaselineSampleOmitsUnknowns(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	s := buildBaselineSample(baselineInputs{now: now})
	if s.V != 1 || s.Kind != "sample" || s.TS != "2026-10-23T12:00:00Z" {
		t.Fatalf("envelope = %+v", s)
	}
	if s.Host != nil || s.OOM != nil || s.Traffic != nil || s.Desired != nil || s.TrimCap != nil {
		t.Fatalf("an empty input must omit every measurement, got %+v", s)
	}

	h, err := buildBaselineHost(baselineHostRaw{HostMiB: -1, CgroupMiB: -1, Load1: floatPtr(1.5)})
	if err == nil {
		t.Fatal("no memory reading must report errNoHostData")
	}
	if h.MemAvailableMiB != nil || h.AvailSource != "" {
		t.Fatal("an unreadable memory reading must be omitted")
	}
	if h.Load1 == nil || *h.Load1 != 1.5 {
		t.Fatal("a failed memory reader must not discard the other readings")
	}
}

func TestBuildBaselineHostTakesTheTighterReading(t *testing.T) {
	h, err := buildBaselineHost(baselineHostRaw{HostMiB: 1000, CgroupMiB: 250})
	if err != nil {
		t.Fatal(err)
	}
	if h.MemAvailableMiB == nil || *h.MemAvailableMiB != 250 || h.AvailSource != "cgroup" {
		t.Fatalf("the cgroup reading must win when it is tighter, got %+v", h)
	}
	h, err = buildBaselineHost(baselineHostRaw{HostMiB: 250, CgroupMiB: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if h.MemAvailableMiB == nil || *h.MemAvailableMiB != 250 || h.AvailSource != "host" {
		t.Fatalf("the host reading must win when it is tighter, got %+v", h)
	}
	// 0 is a real reading (memory exhausted), not no-data.
	h, err = buildBaselineHost(baselineHostRaw{HostMiB: 0, CgroupMiB: -1})
	if err != nil {
		t.Fatal(err)
	}
	if h.MemAvailableMiB == nil || *h.MemAvailableMiB != 0 || h.AvailSource != "host" {
		t.Fatalf("a zero reading must be kept, got %+v", h)
	}
}

func TestResolveBaselineInterval(t *testing.T) {
	t.Setenv("URNETWORK_BASELINE_INTERVAL", "")
	if got := resolveBaselineInterval(); got != baselineInterval {
		t.Fatalf("default interval = %v, want %v", got, baselineInterval)
	}
	t.Setenv("URNETWORK_BASELINE_INTERVAL", "30m")
	if got := resolveBaselineInterval(); got != 30*time.Minute {
		t.Fatalf("override = %v, want 30m", got)
	}
	t.Setenv("URNETWORK_BASELINE_INTERVAL", "10s")
	if got := resolveBaselineInterval(); got != baselineMinInterval {
		t.Fatalf("below-floor override = %v, want the %v floor", got, baselineMinInterval)
	}
	t.Setenv("URNETWORK_BASELINE_INTERVAL", "not-a-duration")
	if got := resolveBaselineInterval(); got != baselineInterval {
		t.Fatalf("a typo must fall back to the default, got %v", got)
	}
}

func TestBaselineRecordAppendsAndHonorsTheSwitch(t *testing.T) {
	withTempHome(t)
	path, err := baselinePath()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { baselineEnabled.Store(true) })
	baselineEnabled.Store(true)

	baselineRecord(baselineStartSample("v-test", "", time.Now()))
	baselineEnabled.Store(false)
	baselineRecord(baselineStartSample("v-off", "", time.Now()))

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "\n"); got != 1 {
		t.Fatalf("wrote %d lines, want 1 (the second record must be dropped while off): %q", got, string(b))
	}
	if !strings.Contains(string(b), `"kind":"start"`) || !strings.Contains(string(b), `"version":"v-test"`) {
		t.Fatalf("start line = %q", string(b))
	}
}

func TestBaselineStartWritesTheStartMarkAndStopsOnCancel(t *testing.T) {
	withTempHome(t)
	t.Cleanup(func() { baselineEnabled.Store(true) })
	baselineEnabled.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := baselineStart(ctx, "v-start", "v-prev")
	path, err := baselinePath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"kind":"start"`) || !strings.Contains(string(b), `"previous_version":"v-prev"`) {
		t.Fatalf("start mark = %q", string(b))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sampler must stop when its context ends")
	}
}

// The sampler must actually be started by the manager lifecycle, not just
// exist: start writes the start mark and stop joins the sampler.
func TestManagerStartWritesTheBaselineStartMark(t *testing.T) {
	home := withTempHome(t)
	t.Cleanup(func() { baselineEnabled.Store(true) })
	baselineEnabled.Store(true)
	src := filepath.Join(home, "proxies.txt")
	if err := os.WriteFile(src, []byte("4.4.4.4:1080:carol:secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultProxyManagerConfig()
	cfg.ProxyFile = src
	m := newProxyManager(cfg)
	if _, _, err := m.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.stop()

	path, err := baselinePath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("manager.start must write the baseline start mark: %v", err)
	}
	if !strings.Contains(string(b), `"kind":"start"`) {
		t.Fatalf("baseline file has no start mark: %q", string(b))
	}
}

// The baseline key is the recorder's switch: on by default, live off, live on,
// and nothing but on/off is accepted.
func TestBaselineControlKeyIsLiveAndDefaultsOn(t *testing.T) {
	state := newAuditWiringState(t)
	t.Cleanup(func() { baselineEnabled.Store(true) })
	baselineEnabled.Store(true)

	if controlUnsupportedKeys["baseline"] {
		t.Fatal("baseline is still listed as unsupported, so a set would be inert")
	}
	if !controlLiveEffectKeys["baseline"] {
		t.Fatal("baseline must be a live-effect key, or set baseline off would not stop sampling")
	}
	if got := controlLiveDefaults["baseline"]; got != "on" {
		t.Fatalf("clearing baseline must restore on, got %q", got)
	}

	resp := handleControlRequest(state, controlRequest{Cmd: "set", Key: "baseline", Value: "off"})
	if !resp.OK {
		t.Fatalf("set baseline off failed: %s", resp.Error)
	}
	if resp.NeedsRestart {
		t.Fatal("baseline is live; it must not require a restart")
	}
	if baselineEnabled.Load() {
		t.Fatal("set baseline off must stop the recorder live")
	}
	resp = handleControlRequest(state, controlRequest{Cmd: "set", Key: "baseline", Value: "on"})
	if !resp.OK || !baselineEnabled.Load() {
		t.Fatalf("set baseline on = %+v, enabled = %v", resp, baselineEnabled.Load())
	}
	if resp := handleControlRequest(state, controlRequest{Cmd: "set", Key: "baseline", Value: "yes"}); resp.OK {
		t.Fatal("only on and off may be accepted")
	}
}

// One failed memory reader must not drop the readings that succeeded. The
// host block still carries swap, psi and load, and only the memory fields go.
func TestApplyBaselineHostKeepsTheBlockWhenMemoryIsMissing(t *testing.T) {
	swap := int64(128)
	host := baselineHost{SwapUsedMiB: &swap}

	var partial baselineInputs
	applyBaselineHost(&partial, host, errNoHostData)
	if !partial.hostKnown {
		t.Fatalf("host block dropped for errNoHostData; swap, psi and load were readable")
	}
	if partial.host.SwapUsedMiB == nil || *partial.host.SwapUsedMiB != swap {
		t.Fatalf("swap reading lost: %v", partial.host.SwapUsedMiB)
	}
	if partial.host.MemAvailableMiB != nil {
		t.Fatalf("memory reading invented: %v", *partial.host.MemAvailableMiB)
	}

	var failed baselineInputs
	applyBaselineHost(&failed, host, errors.New("reader exploded"))
	if failed.hostKnown {
		t.Fatalf("host block kept for an unknown error")
	}

	var ok baselineInputs
	applyBaselineHost(&ok, host, nil)
	if !ok.hostKnown || ok.hostErr != nil {
		t.Fatalf("clean reading: known=%v err=%v", ok.hostKnown, ok.hostErr)
	}
}
