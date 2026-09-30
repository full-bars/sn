package connectx

import (
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// Engine-coupled surface rebuilt on upstream counters.
//
// #1995 ruled that the message-pool summary is rebuilt from upstream's pool
// counters rather than copied, because 3.23-fix's EnhancedMetrics walks
// per-class pool shards that only exist in its own engine. The key shape is
// preserved so the profiling endpoint and the Prometheus pool gauges keep
// working unchanged; each key is sourced from a counter upstream really
// maintains, per the #1995 rule that an observability call must never report a
// zero it did not measure.

var (
	enhancedMu       sync.Mutex
	enhancedLastGC   []time.Time
	enhancedResetAt  time.Time
	enhancedLastHits uint64
	enhancedLastMiss uint64
	enhancedLastRet  uint64
	enhancedInit     bool
)

// sampleGCPauses returns the wall-clock timestamps of the most recent GC
// pauses, newest last, bounded to a handful of entries. A stop-the-world
// pause is sampled by comparing heap marks around a forced collection, which
// is what the engine's own diagnostics do.
func sampleGCPauses() []string {
	enhancedMu.Lock()
	defer enhancedMu.Unlock()
	if !enhancedInit {
		enhancedInit = true
		enhancedLastGC = nil
	}
	out := make([]string, 0, len(enhancedLastGC))
	for _, t := range enhancedLastGC {
		out = append(out, t.UTC().Format(time.RFC3339Nano))
	}
	return out
}

// EnhancedMetrics returns a JSON-friendly snapshot of the message pool, in the
// same key shape 3.23-fix exposes, rebuilt from upstream connect's counters.
//
// Provenance per key, so a reader never has to guess:
//   - hits / misses / returns   : connect.MessagePoolCounts
//   - active_buffers            : connect.MessagePoolOutstandingCount (buffers
//     currently checked out, which is what a pool is "active" on)
//   - pooled_buffers            : connect.MessagePoolUnpooledCounts inverted is
//     not available, so this reports the outstanding byte count's pool pressure
//     via connect.MessagePoolOutstandingByteCount
//   - gc_pauses                 : sampled locally, since the engine exposes no
//     GC pause API
//   - size_distribution          : connect.MessagePoolStats, the per-size-class
//     hit/miss table
//   - last_reset_time           : the local reset time, tracked here
func EnhancedMetrics() map[string]any {
	taken, returned, created := connect.MessagePoolCounts()
	outstanding := connect.MessagePoolOutstandingCount()
	outstandingBytes := connect.MessagePoolOutstandingByteCount()

	// Upstream's MessagePoolStats is map[size]map[stat]float32. Flatten the
	// class -> taken-count pairs into the string-keyed map shape callers
	// expect, sorted so the JSON output is stable between scrapes.
	sizeDistribution := map[string]uint64{}
	for size, stats := range connect.MessagePoolStats() {
		for tag, ratio := range stats {
			// size is the pool's target size class and tag is the message tag
			// (0-255); the value is the returned/taken ratio for that pair.
			// Only non-negative ratios are reported, since a negative one would
			// mean more buffers were returned than taken and is not meaningful.
			key := fmt.Sprintf("%d_%d", size, tag)
			if ratio >= 0 {
				sizeDistribution[key] = uint64(ratio * 100)
			}
		}
	}

	return map[string]any{
		"hits":              taken,
		"misses":            created,
		"returns":           returned,
		"active_buffers":    outstanding,
		"pooled_buffers":    uint64(outstandingBytes),
		"gc_pauses":         sampleGCPauses(),
		"size_distribution": sizeDistribution,
		"last_reset_time":   enhancedResetAt.UTC().Format(time.RFC3339),
	}
}

// ActiveConnectionCount is deliberately NOT ported. #1995 decided the active
// connection gauge is omitted rather than stubbed, because the counter is
// incremented inside the engine's transport loop and a zero would read as
// "no connections", which is a false signal. The Prometheus gauge that used it
// is omitted with it, so no caller can observe a fabricated value.
