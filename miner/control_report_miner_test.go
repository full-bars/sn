package miner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
	"github.com/urnetwork/connect"
)

// writeTestProxyState writes a proxy.state file into the current test state
// directory.
func writeTestProxyState(t *testing.T, state *ProxyState) {
	t.Helper()
	if err := writeProxyState(state); err != nil {
		t.Fatalf("writeProxyState: %v", err)
	}
}

func TestMinerSnapshotReportsRealState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	resetControlRuntimeForTest()
	resetProxyLiveHealthRegistryForTest()
	saved := globalControlState
	globalControlState = newControlState()
	defer func() {
		globalControlState = saved
		resetControlRuntimeForTest()
	}()

	// Two proxies: one up from the persisted health, one dead.
	writeTestProxyState(t, &ProxyState{
		Version:   2,
		StartedAt: time.Now().Add(-time.Hour),
		Proxies: map[string]ProxyEntry{
			"a.example:1080": {ID: 1, Health: "up"},
			"b.example:1080": {ID: 2, Health: "dead"},
		},
	})
	controlRuntime.startedAt = time.Now().Add(-10 * time.Minute)

	registry := bandwidth.NewRegistry()
	one := registry.Register(1)
	one.BillableTx.Add(100)
	one.BillableRx.Add(50)
	one.TotalTx.Add(200)
	one.TotalRx.Add(100)
	one.Clients.Store(3)
	controlRuntime.setBandwidthRegistry(registry)
	controlRuntime.setRunningProxies([]string{"a.example:1080", "b.example:1080"})

	snap, err := buildMinerSnapshot()
	if err != nil {
		t.Fatalf("buildMinerSnapshot: %v", err)
	}
	if snap.Version == "" {
		t.Fatalf("snapshot has no version")
	}
	if snap.Proxies.Up != 1 || snap.Proxies.Dead != 1 || snap.Proxies.total() != 2 {
		t.Fatalf("proxy counts = %+v", snap.Proxies)
	}
	if snap.Clients != 3 {
		t.Fatalf("clients = %d", snap.Clients)
	}
	if snap.Traffic == nil || snap.Traffic.BillableBytes != 150 || snap.Traffic.TotalBytes != 300 {
		t.Fatalf("traffic = %+v", snap.Traffic)
	}
	if snap.Resources.Goroutines == 0 {
		t.Fatalf("resources.goroutines not read")
	}
	if snap.State == "" {
		t.Fatalf("state not derived")
	}
	if len(snap.MinerNotes) == 0 {
		t.Fatalf("miner_notes must name what the miner cannot supply")
	}
}

func TestMinerSnapshotRestartReasonFromMarker(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	resetControlRuntimeForTest()
	defer resetControlRuntimeForTest()

	// A clean-exit marker from the previous run is read and removed.
	marker := runStateMarker{Reason: "clean", Version: "v1"}
	data, _ := json.Marshal(marker)
	if err := os.WriteFile(filepath.Join(dir, runStateFileName), data, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	beginControlRun(time.Now())
	if _, err := os.Stat(filepath.Join(dir, runStateFileName)); !os.IsNotExist(err) {
		t.Fatalf("beginControlRun did not remove the marker")
	}
	snap, err := buildMinerSnapshot()
	if err != nil {
		t.Fatalf("buildMinerSnapshot: %v", err)
	}
	if snap.Restart.Reason != "clean" || !snap.Restart.CleanShutdown {
		t.Fatalf("restart = %+v", snap.Restart)
	}
	if snap.PreviousVersion != "v1" {
		t.Fatalf("previous_version = %q", snap.PreviousVersion)
	}

	// finishControlRun leaves the marker the next start reads.
	finishControlRun("clean")
	data, err = os.ReadFile(filepath.Join(dir, runStateFileName))
	if err != nil {
		t.Fatalf("finishControlRun did not write the marker: %v", err)
	}
	var written runStateMarker
	if err := json.Unmarshal(data, &written); err != nil || written.Reason != "clean" {
		t.Fatalf("marker = %s (%v)", string(data), err)
	}
}

func TestMinerSnapshotRestartReasonUncleanAndFirstStart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	resetControlRuntimeForTest()
	defer resetControlRuntimeForTest()

	beginControlRun(time.Now())
	snap, err := buildMinerSnapshot()
	if err != nil {
		t.Fatalf("buildMinerSnapshot: %v", err)
	}
	if snap.Restart.Reason != "first-start" {
		t.Fatalf("fresh state dir restart = %q", snap.Restart.Reason)
	}

	// A proxy.state file with no clean marker means the last run died.
	if err := writeProxyState(&ProxyState{Version: 2, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatalf("writeProxyState: %v", err)
	}
	resetControlRuntimeForTest()
	beginControlRun(time.Now())
	snap, err = buildMinerSnapshot()
	if err != nil {
		t.Fatalf("buildMinerSnapshot: %v", err)
	}
	if snap.Restart.Reason != "unclean" {
		t.Fatalf("state without a clean marker = %q", snap.Restart.Reason)
	}
}

func TestMinerTrafficAndTrimPreview(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	resetControlRuntimeForTest()
	resetProxyLiveHealthRegistryForTest()
	defer resetControlRuntimeForTest()

	registry := bandwidth.NewRegistry()
	bw := registry.Register(1)
	bw.TotalTx.Add(4096)
	bw.BillableTx.Add(2048)
	controlRuntime.setBandwidthRegistry(registry)

	traffic, err := buildMinerTraffic()
	if err != nil {
		t.Fatalf("buildMinerTraffic: %v", err)
	}
	if traffic.TotalBytes != 4096 || traffic.BillableBytes != 2048 {
		t.Fatalf("traffic = %+v", traffic)
	}

	writeTestProxyState(t, &ProxyState{
		Version:   2,
		StartedAt: time.Now(),
		Proxies: map[string]ProxyEntry{
			"a.example:1080": {ID: 1, Health: "up"},
			"b.example:1080": {ID: 2, Health: "up"},
			"c.example:1080": {ID: 3, Health: "up"},
		},
	})
	controlRuntime.setRunningProxies([]string{"a.example:1080", "b.example:1080", "c.example:1080"})

	text, err := buildMinerTrimPreview(1)
	if err != nil {
		t.Fatalf("buildMinerTrimPreview: %v", err)
	}
	if !strings.Contains(text, "preview:") || !strings.Contains(text, "would shed 2") {
		t.Fatalf("trim preview = %q", text)
	}

	// A preview that fits says so.
	text, err = buildMinerTrimPreview(10)
	if err != nil {
		t.Fatalf("buildMinerTrimPreview: %v", err)
	}
	if !strings.Contains(text, "nothing to shed") {
		t.Fatalf("trim preview over the running set = %q", text)
	}
}

func TestMinerTrafficUnavailableBeforeRun(t *testing.T) {
	resetControlRuntimeForTest()
	defer resetControlRuntimeForTest()
	if _, err := buildMinerTraffic(); err == nil {
		t.Fatalf("traffic must be explicitly unavailable before the provide loop starts")
	}
}

func TestControlSocketDataCommands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	resetControlRuntimeForTest()
	defer resetControlRuntimeForTest()
	saved := globalControlState
	globalControlState = newControlState()
	defer func() { globalControlState = saved }()

	registry := bandwidth.NewRegistry()
	bw := registry.Register(1)
	bw.TotalTx.Add(1024)
	controlRuntime.setBandwidthRegistry(registry)

	state := newControlState()
	path, cleanup := testControlSocket(t, state)
	defer cleanup()

	resp := controlRequestJSON(t, path, controlRequest{Cmd: "snapshot"})
	if !resp.OK || resp.Snapshot == nil {
		t.Fatalf("snapshot = %+v", resp)
	}
	if resp.Snapshot.V != 1 {
		t.Fatalf("snapshot schema = %d", resp.Snapshot.V)
	}
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "traffic"})
	if !resp.OK || resp.Traffic == nil || resp.Traffic.TotalBytes != 1024 {
		t.Fatalf("traffic = %+v", resp)
	}
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "internals"})
	if !resp.OK || resp.Internals == nil || resp.Internals.Goroutines == 0 {
		t.Fatalf("internals = %+v", resp)
	}
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "goroutines"})
	if !resp.OK || resp.Goroutines == nil || resp.Goroutines.Total == 0 {
		t.Fatalf("goroutines = %+v", resp)
	}
	// No audit engine: the audit command says so explicitly.
	resp = controlRequestJSON(t, path, controlRequest{Cmd: "audit", Action: "status"})
	if resp.OK || !strings.Contains(resp.Error, "audit") {
		t.Fatalf("audit status = %+v", resp)
	}
}

func TestControlRateSamplerDeltas(t *testing.T) {
	var s controlRateSampler
	base := time.Now()
	s.sample(1000, 2000, base)
	s.sample(1250, 2300, base.Add(1*time.Second))
	now, avg1m, _, seq, history := s.rates()
	if now != 250 || avg1m != 250 {
		t.Fatalf("rate = %v avg1m=%v", now, avg1m)
	}
	if seq != 1 || len(history) != 1 {
		t.Fatalf("seq=%d history=%v", seq, history)
	}
	totalNow, totalAvg1m, _, totalHistory := s.totalHistory()
	if totalNow != 300 || totalAvg1m != 300 || len(totalHistory) != 1 {
		t.Fatalf("total rate = %v avg=%v history=%v", totalNow, totalAvg1m, totalHistory)
	}
	// A counter that went backwards (a rebuilt proxy) contributes nothing.
	s.sample(10, 2300, base.Add(2*time.Second))
	now, _, _, seq, _ = s.rates()
	if now != 250 || seq != 1 {
		t.Fatalf("backwards counter counted: now=%v seq=%d", now, seq)
	}
}

func TestProviderSlotKeysSkipsDirect(t *testing.T) {
	keys := providerSlotKeys([]*connect.ProxySettings{
		nil,
		{Network: "tcp", Address: "a.example:1080"},
		{Network: "tcp", Address: "b.example:1080"},
	})
	if len(keys) != 2 {
		t.Fatalf("keys = %v", keys)
	}
}
