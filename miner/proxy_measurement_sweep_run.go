package miner

// Dispatches UDP relay checks for the identities the provider runs, so a slot
// that no client is talking to still gets a verdict.
//
// The check itself is bounded and remembers its result (proxy_udp_check.go),
// but until now it only ran when a candidate set was noted or an identity
// dialled (h3_proxy_set.go, proxy_udp_egress.go). A quiet slot therefore had no
// verdict when it came back into use, and a restart began from zero. One
// goroutine per run now walks the pool on a timer:
//   - at most measurementSweepBatch checks are started per tick, and the
//     store's own slots bound how many hold sockets at once;
//   - the order is the selector's (proxy_measurement_sweep.go): never checked
//     first, then the earliest due time, with a key tie break, and an identity
//     whose check was dispatched but never published sorts behind the ones the
//     sweep has not tried, so one identity cannot hold the only slot;
//   - an identity whose verdict is still inside its lifetime is not picked,
//     so the sweep costs nothing while the pool is warm;
//   - the identity set is the runtime's proxy list (h3Candidates), which the
//     provide run notes at start and after every applied reload. The direct
//     slot is not in it.
//
// The verdict decides H3 eligibility and the identity's client UDP path
// independently of the h3 mode, so the sweep is not gated on that mode. The
// batch bound keeps the cost at a few probes a minute.

import (
	"context"
	"time"

	"github.com/urnetwork/connect"
)

var (
	// measurementSweepInterval is the pause between ticks. A variable so a
	// test can drive the loop without waiting.
	measurementSweepInterval = time.Minute
	// measurementSweepBatch bounds the checks one tick may start.
	measurementSweepBatch = 4
)

// runtimeMeasurementCandidates copies the proxy list the provide run noted.
func runtimeMeasurementCandidates() []*connect.ProxySettings {
	h3CandidatesMutex.Lock()
	defer h3CandidatesMutex.Unlock()
	return append([]*connect.ProxySettings(nil), h3Candidates...)
}

// sweepCandidatesLocked builds the scheduling evidence for the identities in
// settings. An identity already being checked is left out: its running entry
// has no verdict yet, and Check would make the sweep wait on it. Called with
// the lock held.
func (self *proxyUDPCheckStore) sweepCandidatesLocked(now time.Time, settings []*connect.ProxySettings) []measurementCandidate {
	candidates := make([]measurementCandidate, 0, len(settings))
	for _, proxySettings := range settings {
		if proxySettings == nil {
			continue
		}
		key := proxySettings.Key()
		if entry := self.entries[key]; entry != nil && entry.done != nil {
			continue
		}
		candidate := measurementCandidate{key: key, attemptedAt: self.attemptAtLocked(key)}
		if entry := self.restoredEntryLocked(key); entry != nil {
			// keep the stored stamp as it is: it decides both due time and
			// eligibility, and only the sort key strips the monotonic reading
			candidate.udpAt = entry.at
			candidate.udpInterval = proxyUDPTTL(entry.result.Status)
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

// sweepOnce starts one bounded batch of checks and reports how many it started.
// The checks publish through the normal path, so each one records its attempt
// before the slow step, writes the verdict when it has one, and leaves the
// attempt behind when it is cancelled.
func (self *proxyUDPCheckStore) sweepOnce(ctx context.Context, now time.Time, settings []*connect.ProxySettings, limit int) int {
	if limit <= 0 || len(settings) == 0 {
		return 0
	}
	self.mu.Lock()
	candidates := self.sweepCandidatesLocked(now, settings)
	self.mu.Unlock()

	selected := selectMeasurementSweep(now, candidates, limit)
	if len(selected) == 0 {
		return 0
	}
	// record the dispatch before the goroutine exists, so a tick that follows
	// before the check has even started still orders these identities last
	self.mu.Lock()
	for _, candidate := range selected {
		self.noteAttemptLocked(candidate.key, now)
	}
	self.mu.Unlock()
	byKey := make(map[string]*connect.ProxySettings, len(settings))
	for _, proxySettings := range settings {
		if proxySettings != nil {
			byKey[proxySettings.Key()] = proxySettings
		}
	}
	started := 0
	for _, candidate := range selected {
		proxySettings, ok := byKey[candidate.key]
		if !ok {
			continue
		}
		go self.Check(ctx, proxySettings, now)
		started++
	}
	return started
}

// sweepLoop ticks until ctx ends. ticks is passed in so a test drives it
// without a clock.
func (self *proxyUDPCheckStore) sweepLoop(
	ctx context.Context,
	ticks <-chan time.Time,
	candidates func() []*connect.ProxySettings,
	limit int,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			self.sweepOnce(ctx, now, candidates(), limit)
		}
	}
}

// startProcessMeasurements loads the durable UDP verdicts and starts the sweep
// that keeps them current. The store is process-wide, so both are published on
// the control runtime and stopped by finishControlRun.
func (s *controlRuntimeState) startProcessMeasurements(now time.Time) {
	s.stopProcessMeasurements()
	globalProxyUDPCheck.enableMeasurementPersistence(mustStatePathQuiet("udp-measurements.json"), now)
	stop := startProxyMeasurementSweep(context.Background())
	s.measurementsMu.Lock()
	s.measurementsStop = stop
	s.measurementsMu.Unlock()
}

// stopProcessMeasurements stops the sweep and writes the records it still
// holds. Safe when nothing was started.
func (s *controlRuntimeState) stopProcessMeasurements() {
	s.measurementsMu.Lock()
	stop := s.measurementsStop
	s.measurementsStop = nil
	s.measurementsMu.Unlock()
	if stop != nil {
		stop()
	}
	globalProxyUDPCheck.stopMeasurementPersistence()
}

// startProxyMeasurementSweep runs the sweep for this process and returns its
// stop, which waits for the loop to leave.
func startProxyMeasurementSweep(ctx context.Context) (stop func()) {
	sweepCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	ticker := time.NewTicker(measurementSweepInterval)
	go func() {
		defer close(done)
		defer ticker.Stop()
		globalProxyUDPCheck.sweepLoop(sweepCtx, ticker.C, runtimeMeasurementCandidates, measurementSweepBatch)
	}()
	return func() {
		cancel()
		<-done
	}
}
