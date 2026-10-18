package miner

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
	"github.com/urnetwork/connect"
)

// providerSlotKeys returns the identity keys of the non-direct proxy slots:
// the set the trim preview and the snapshot's proxy counts describe. The
// direct slot carries no ProxySettings and is not a proxy.
func providerSlotKeys(providers []*connect.ProxySettings) []string {
	keys := make([]string, 0, len(providers))
	for _, p := range providers {
		if p == nil || p.Address == "" {
			continue
		}
		keys = append(keys, p.Key())
	}
	return keys
}

// control_runtime.go is the bridge between the miner's provide run and the
// control socket's data commands (P2b). The run publishes what it owns - the
// slots it launched, the bandwidth registry that counts through the proxies,
// the per-slot progress owner, and the run's start time - and the socket's
// snapshot/traffic/internals/trim_preview builders read it. Everything the
// miner does not have is reported as explicitly missing, never invented.

// runStateFileName is the little marker the run writes on a clean exit and
// reads on the next start, so the snapshot can report why the process
// restarted (first-start, clean, unclean, hotswap).
const runStateFileName = ".miner-run-state.json"

// runStateMarker is the persisted shape of that marker.
type runStateMarker struct {
	Reason  string `json:"reason"` // clean, hotswap, update
	Version string `json:"version,omitempty"`
}

// controlRuntime is the process-wide publication point. One provide run at a
// time writes it; any goroutine may read it.
type controlRuntimeState struct {
	mu sync.Mutex

	startedAt time.Time

	// runningKeys are the identity keys of the proxy slots this run launched
	// (the direct slot has no key and is not counted as a proxy).
	runningKeys []string

	registry *bandwidth.ProxyRegistry
	progress *providerProgressOwner

	// lastMarker is what the previous run left behind (why it stopped and what
	// version it was), captured before the marker is removed.
	lastMarker  runStateMarker
	hadPriorRun bool

	// lastStartedVersion is the version of the previous start, read from the
	// .miner_version breadcrumb. Unlike the marker (written only by a clean
	// exit) it exists after a crash too, which is what lets the snapshot and
	// /metrics tell a crash-restart from a first start.
	lastStartedVersion string

	// rate is the billable/total byte rate ring behind the snapshot's rate and
	// traffic history.
	rate controlRateSampler
}

// minerVersionBreadcrumb is the version file written at every run start
// (v33's .provider_version). metrics_provider.go and the snapshot read it.
//
// It must not be named .provider_version here: the state directory is also
// the provider-key custody directory, and clientauth treats every `.provider*`
// file as retained provider history, which refuses a first registration
// (clientauth/provider_key.go: a missing .provider.key beside any `.provider`
// file is "provider_key_missing_with_retained_history", never a new install).
// The breadcrumb is the miner's own, so it lives under the miner's own name.
const minerVersionBreadcrumb = ".miner_version"

var controlRuntime = &controlRuntimeState{}

// beginControlRun reads and removes the previous run's marker, so the snapshot
// can report a restart reason and the previous version. Safe to call once per
// run; a missing state directory is not an error.
func beginControlRun(now time.Time) {
	controlRuntime.mu.Lock()
	defer controlRuntime.mu.Unlock()
	controlRuntime.startedAt = now

	path, err := providerStatePath(runStateFileName)
	if err != nil {
		return
	}
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		var marker runStateMarker
		if err := json.Unmarshal(data, &marker); err == nil {
			controlRuntime.lastMarker = marker
		}
		controlRuntime.hadPriorRun = true
	}
	os.Remove(path)

	if proxyStateFile := mustStatePathQuiet("proxy.state"); proxyStateFile != "" {
		if _, err := os.Stat(proxyStateFile); err == nil {
			controlRuntime.hadPriorRun = true
		}
	}

	// The version breadcrumb: read the previous start's version (survives a
	// crash, unlike the marker), then record this one. A breadcrumb that was
	// already there also marks the run as a restart in its own right.
	if versionPath := mustStatePathQuiet(minerVersionBreadcrumb); versionPath != "" {
		if data, err := os.ReadFile(versionPath); err == nil {
			controlRuntime.lastStartedVersion = strings.TrimSpace(string(data))
			if controlRuntime.lastStartedVersion != "" {
				controlRuntime.hadPriorRun = true
			}
		}
		if err := atomicWriteFile(versionPath, []byte(minerControlVersion()), 0o600); err != nil {
			tlog("[control] could not write the version breadcrumb: %v\n", err)
		}
	}
}

// finishControlRun writes the marker a clean exit leaves behind, and flushes
// the audit ring. reason is "clean" for a graceful stop; the hot restart path
// writes "hotswap" itself before it execs.
func finishControlRun(reason string) {
	path, err := providerStatePath(runStateFileName)
	if err != nil {
		return
	}
	marker := runStateMarker{Reason: reason, Version: minerControlVersion()}
	if data, err := json.Marshal(marker); err == nil {
		if err := atomicWriteFile(path, data, 0o600); err != nil {
			tlog("[control] could not write the run marker: %v\n", err)
		}
	}
	forceControlAuditPersist()
}

// mustStatePathQuiet resolves one file in the state directory, or "".
func mustStatePathQuiet(name string) string {
	path, err := providerStatePath(name)
	if err != nil {
		return ""
	}
	return path
}

// setRunningProxies publishes the slots this run launched: every non-direct
// provider's identity key. The trim preview uses it, because this process is
// the only one that knows its running set.
func (s *controlRuntimeState) setRunningProxies(addresses []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runningKeys = append([]string(nil), addresses...)
}

// setBandwidthRegistry publishes the run's per-proxy byte counters.
func (s *controlRuntimeState) setBandwidthRegistry(registry *bandwidth.ProxyRegistry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry = registry
}

// setProviderProgress publishes the per-slot progress owner.
func (s *controlRuntimeState) setProviderProgress(progress *providerProgressOwner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress = progress
}

// runningKeySnapshot returns the launched proxy keys.
func (s *controlRuntimeState) runningKeySnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.runningKeys...)
}

// startedAtSnapshot returns the run's start time.
func (s *controlRuntimeState) startedAtSnapshot() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startedAt
}

// markerSnapshot returns the previous run's marker and whether any prior run
// left state behind.
func (s *controlRuntimeState) markerSnapshot() (runStateMarker, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastMarker, s.hadPriorRun
}

// lastStartedVersionSnapshot returns the version the previous start recorded
// in the .miner_version breadcrumb ("" when there is none, or before any
// prior run started).
func (s *controlRuntimeState) lastStartedVersionSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastStartedVersion
}

// counterTotals reads the run's cumulative billable and total byte counts, and
// the number of live clients, from the bandwidth registry. The second result
// is false when this run has no registry yet (for example a snapshot taken
// before the provide loop started).
func (s *controlRuntimeState) counterTotals() (billable, total uint64, clients int64, ok bool) {
	s.mu.Lock()
	registry := s.registry
	s.mu.Unlock()
	if registry == nil {
		return 0, 0, 0, false
	}
	for _, bw := range registry.Snapshot() {
		if bw == nil {
			continue
		}
		billable += bw.BillableRx.Load() + bw.BillableTx.Load()
		total += bw.TotalRx.Load() + bw.TotalTx.Load()
		clients += bw.Clients.Load()
	}
	return billable, total, clients, true
}

// slotSummary counts the progress owner's slots by lifecycle and readiness.
// ok is false when this run has no progress owner.
func (s *controlRuntimeState) slotSummary(ctx context.Context) (total, connected, ready int, ok bool) {
	s.mu.Lock()
	progress := s.progress
	s.mu.Unlock()
	if progress == nil {
		return 0, 0, 0, false
	}
	snapshot, err := progress.snapshot(ctx)
	if err != nil || snapshot == nil {
		return 0, 0, 0, false
	}
	for _, member := range snapshot.Members {
		total++
		if member.Connected {
			connected++
		}
		if member.Ready {
			ready++
		}
	}
	return total, connected, ready, true
}

// controlRateSampler turns the cumulative byte counters into a per-second rate
// ring, one sample per tick. Only the delta between two readings of registers
// that went up is counted; a rebuilt proxy handing back a zeroed counter
// contributes nothing.
type controlRateSampler struct {
	mu       sync.Mutex
	prev     uint64
	prevTime time.Time

	billableRing [snapshotRingSize]float64
	next         int
	count        int
	recorded     uint64

	totalPrev     uint64
	totalRing     [snapshotRingSize]float64
	totalNext     int
	totalCount    int
	totalRecorded uint64
}

// snapshotRingSize is ten minutes of one-second samples, the same ring the
// 3.23-fix provider uses.
const snapshotRingSize = 600

// sample records one tick. The first call only establishes the baseline.
func (s *controlRateSampler) sample(billable, total uint64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	first := s.prevTime.IsZero()
	elapsed := now.Sub(s.prevTime).Seconds()
	if first {
		s.prev, s.totalPrev = billable, total
		s.prevTime = now
		return
	}
	if elapsed < 1 {
		elapsed = 1
	}
	if billable >= s.prev {
		s.billableRing[s.next] = float64(billable-s.prev) / elapsed
		s.recorded++
		s.next = (s.next + 1) % snapshotRingSize
		if s.count < snapshotRingSize {
			s.count++
		}
	}
	if total >= s.totalPrev {
		s.totalRing[s.totalNext] = float64(total-s.totalPrev) / elapsed
		s.totalRecorded++
		s.totalNext = (s.totalNext + 1) % snapshotRingSize
		if s.totalCount < snapshotRingSize {
			s.totalCount++
		}
	}
	s.prev, s.totalPrev = billable, total
	s.prevTime = now
}

// rates returns the newest billable sample and the 1m/5m averages.
func (s *controlRateSampler) rates() (now, avg1m, avg5m float64, seq uint64, history []float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	history = ringValues(s.billableRing[:], s.next, s.count)
	if len(history) == 0 {
		return 0, 0, 0, s.recorded, history
	}
	return history[len(history)-1], avgTailFloat(history, 60), avgTailFloat(history, 300), s.recorded, history
}

// totalHistory returns the total-traffic rate ring and its averages.
func (s *controlRateSampler) totalHistory() (now, avg1m, avg5m float64, history []float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	history = ringValues(s.totalRing[:], s.totalNext, s.totalCount)
	if len(history) == 0 {
		return 0, 0, 0, history
	}
	return history[len(history)-1], avgTailFloat(history, 60), avgTailFloat(history, 300), history
}

// ringValues returns the valid samples oldest first.
func ringValues(ring []float64, next, count int) []float64 {
	out := make([]float64, count)
	start := (next - count + len(ring)) % len(ring)
	for i := 0; i < count; i++ {
		out[i] = ring[(start+i)%len(ring)]
	}
	return out
}

// avgTailFloat averages the newest n samples.
func avgTailFloat(h []float64, n int) float64 {
	if len(h) < n {
		n = len(h)
	}
	if n == 0 {
		return 0
	}
	var sum float64
	for _, v := range h[len(h)-n:] {
		sum += v
	}
	return sum / float64(n)
}

// resetControlRuntimeForTest clears the published run state. Tests only.
func resetControlRuntimeForTest() {
	controlRuntime.mu.Lock()
	defer controlRuntime.mu.Unlock()
	controlRuntime.startedAt = time.Time{}
	controlRuntime.runningKeys = nil
	controlRuntime.registry = nil
	controlRuntime.progress = nil
	controlRuntime.lastMarker = runStateMarker{}
	controlRuntime.hadPriorRun = false
	controlRuntime.lastStartedVersion = ""
	controlRuntime.rate = controlRateSampler{}
}

// runSampler takes one rate sample per second until ctx is done.
func (s *controlRuntimeState) runSampler(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if billable, total, _, ok := s.counterTotals(); ok {
				s.rate.sample(billable, total, now)
			}
		}
	}
}
