package miner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
)

// metrics_persist.go gives the miner's cumulative Prometheus counters a life
// longer than the process that counts them. They are plain atomics seeded at
// zero, so a restart (rolling upgrade, reboot, crash, hotswap) reset every
// urnet_*_total series and lost the per-node traffic history a restart-
// surviving attribution needs. This file restores those totals from a
// versioned JSON store in the miner's state directory before /metrics serves,
// and writes them back on a cadence and on shutdown. A HotSwap takeover
// catches the promoted process up to the parent's last flush, because the
// candidate registers its slots before the parent yields.
//
// The miner's counter surface is its own: per-proxy byte totals keyed by the
// bandwidth registry's index (proxyIdentityBandwidthIndex, stable across
// restarts for one proxy identity), and the control socket's command counts.
// There is no process-global aggregate to save, because the aggregate the
// traffic writer publishes is the sum over the registry. It is reconstructed
// instead: a restored registry entry is seeded with its stored totals, and
// retiredTotals adds the stored totals of the indices this process does not
// run, so the aggregate continues without counting a live proxy twice.
//
// What is NOT persisted, and why: gauges (they describe the current process),
// client counts, session ages, and Go runtime counters (GC cycles belong to
// this process). Only monotonic totals are stored.
//
// The store is deliberately separate from proxy.state and from the earnings
// lifetime store (metrics_seams.go seamLifetime). This is the raw counter
// surface /metrics exposes; folding it into an accounting ledger would double
// count what that ledger's own collectors add up.

const (
	// metricsCounterStoreVersion is bumped when the on-disk shape changes in a
	// way an older reader would misinterpret. A store written by a NEWER
	// version is left alone rather than guessed at; a store written by an
	// older one is read, because every field is additive and absent fields
	// simply stay zero.
	metricsCounterStoreVersion = 1

	// metricsCounterStoreName is the store's file name in the miner's state
	// directory.
	metricsCounterStoreName = ".metrics-counters.json"

	// defaultMetricsCounterFlushInterval is how often the store is rewritten.
	// It bounds what a SIGKILL can lose; 45s keeps that small without turning
	// a quiet node's state file into a write-rate item.
	defaultMetricsCounterFlushInterval = 45 * time.Second

	// metricsCounterProxyRetention drops per-proxy entries that have not been
	// live or flushed for this long. A proxy absent for a month is not coming
	// back with the same identity in any way that its old byte count should
	// follow.
	metricsCounterProxyRetention = 30 * 24 * time.Hour

	// metricsCounterProxyMaxEntries caps the per-proxy map even inside the
	// retention window. A node churning URL-sourced proxies can see thousands
	// of addresses; the oldest non-live entries are dropped first, so a live
	// proxy is never pruned by the cap.
	metricsCounterProxyMaxEntries = 2048

	// metricsCounterUnchangedRewriteAge is how stale the store may grow while
	// nothing changes. An idle node skips the write entirely (every write is a
	// temp file, an fsync, a rename and a directory fsync, which is what wears
	// an SD card), but saved_at is what an operator checks for liveness, so an
	// unchanged store is still refreshed this often.
	metricsCounterUnchangedRewriteAge = 24 * time.Hour
)

var errMetricsCountersNewer = errors.New("metrics counters: store written by a newer version")

// minerMetricsProxyCounters is one registry index's cumulative byte totals.
// LastSeen is restamped on every flush for a live entry, which is why the
// unchanged check ignores it.
type minerMetricsProxyCounters struct {
	TotalRx    uint64 `json:"total_rx"`
	TotalTx    uint64 `json:"total_tx"`
	BillableRx uint64 `json:"billable_rx"`
	BillableTx uint64 `json:"billable_tx"`
	// The engine's per-contract totals travel with billable because the two are
	// a cross-check: if only billable survived a restart, every restart would
	// reopen a drift between them that is a bookkeeping artifact, not traffic
	// the platform settled differently.
	ContractUsedIngress uint64 `json:"contract_used_ingress"`
	ContractUsedEgress  uint64 `json:"contract_used_egress"`
	LastSeen            int64  `json:"last_seen_unix"`
}

// minerMetricsControlCounters is the control socket's cumulative command
// counts, the miner's counterpart of the v33 provider's own counters.
type minerMetricsControlCounters struct {
	Acked uint64 `json:"acked"`
	Set   uint64 `json:"set"`
	Get   uint64 `json:"get"`
	Clear uint64 `json:"clear"`
}

// minerMetricsByteTotals is a sum of the byte fields, without LastSeen.
type minerMetricsByteTotals struct {
	TotalRx             uint64
	TotalTx             uint64
	BillableRx          uint64
	BillableTx          uint64
	ContractUsedIngress uint64
	ContractUsedEgress  uint64
}

func (t minerMetricsByteTotals) isZero() bool {
	return t.TotalRx == 0 && t.TotalTx == 0 && t.BillableRx == 0 && t.BillableTx == 0 &&
		t.ContractUsedIngress == 0 && t.ContractUsedEgress == 0
}

// minerMetricsCountersFile is the on-disk shape. PID and SavedAt are written
// for operators and for diagnosing a concurrent writer; they never decide
// whether a stored value is valid.
type minerMetricsCountersFile struct {
	Version int                               `json:"version"`
	SavedAt time.Time                         `json:"saved_at"`
	PID     int                               `json:"pid"`
	Proxies map[int]minerMetricsProxyCounters `json:"proxies,omitempty"`
	Control minerMetricsControlCounters       `json:"control"`
	// Retired is the running total of entries a prune removed from Proxies.
	// Without it urnet_*_total would step DOWN when an index ages out or the
	// entry cap evicts it, and Prometheus reads a decrease as a counter reset,
	// reporting most of the node's lifetime total as new traffic.
	Retired minerMetricsByteTotals `json:"retired,omitempty"`
}

// minerMetricsCounterSnapshot is what the store reads from the live process:
// the bandwidth registry keyed by its index, plus the control counters.
type minerMetricsCounterSnapshot struct {
	Proxies map[int]minerMetricsProxyCounters
	Control minerMetricsControlCounters
}

// metricsCounterStore owns the store's path, its clock, its write and its
// snapshot. All four are injectable so a test never sleeps, never touches the
// real state directory, and can count the exact number of writes.
type metricsCounterStore struct {
	mu   sync.Mutex
	path string

	now        func() time.Time
	write      func(path string, data []byte, perm os.FileMode) error
	snapshot   func() minerMetricsCounterSnapshot
	addControl func(minerMetricsControlCounters)

	// base holds the pre-process totals per registry index: the seed for a
	// freshly registered proxy and the history a retired proxy keeps
	// contributing to the aggregate.
	base     map[int]minerMetricsProxyCounters
	restored bool

	// retired holds the totals of entries a prune removed from base. base no
	// longer carries them, so retiredTotals has to add them or the exported
	// lifetime total would fall.
	retired minerMetricsByteTotals

	// applied is how much of the STORED totals this process has already folded
	// into its live counters. A catch-up adds only the difference, so it can
	// run twice (takeover and post-drain) without double counting, and it can
	// add at all instead of taking the max, which discarded whichever side was
	// smaller: the parent's drain bytes or this process's own.
	applied map[int]minerMetricsProxyCounters
	// foreign is the last per-index total OBSERVED in the store file. It is
	// deliberately separate from base, because flush rewrites base with this
	// process's own live totals: using base as the catch-up source made the
	// next catch-up add this process's own bytes a second time.
	foreign        map[int]minerMetricsProxyCounters
	appliedControl minerMetricsControlCounters
	// controlAppliedSet marks appliedControl as initialised. The control
	// counters are process-global and already hold this process's own commands,
	// unlike a proxy slot that starts at zero, so the baseline is taken from
	// the live counter at the first catch-up rather than assumed to be zero.
	controlAppliedSet bool

	// flushMu serialises flushes inside this process, so the periodic writer
	// and a shutdown flush cannot interleave their read-merge-write sequences.
	flushMu sync.Mutex

	stop   chan struct{}
	done   chan struct{}
	cancel context.CancelFunc
}

func newMetricsCounterStore(path string) *metricsCounterStore {
	return &metricsCounterStore{
		path:       path,
		now:        time.Now,
		write:      writeMetricsCounterFileAtomic,
		snapshot:   metricsCounterLiveSnapshot,
		addControl: addMinerControlCounters,
		base:       map[int]minerMetricsProxyCounters{},
		applied:    map[int]minerMetricsProxyCounters{},
		foreign:    map[int]minerMetricsProxyCounters{},
	}
}

// arm sets the store's path. It reports false when the store is already armed,
// so a second startup call (a hotswap takeover) does not restart the writer.
func (s *metricsCounterStore) arm(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path != "" {
		return false
	}
	s.path = path
	return true
}

// storePath returns the current path, "" while the store is not armed.
func (s *metricsCounterStore) storePath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// load reads and validates the store.
func (s *metricsCounterStore) load() (minerMetricsCountersFile, error) {
	var file minerMetricsCountersFile
	path := s.storePath()
	if path == "" {
		return file, fmt.Errorf("metrics counters: empty path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return file, err
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return file, fmt.Errorf("parse %s: %w", path, err)
	}
	if file.Version > metricsCounterStoreVersion {
		return file, errMetricsCountersNewer
	}
	if file.Version < 1 {
		return file, fmt.Errorf("parse %s: unknown version %d", path, file.Version)
	}
	return file, nil
}

// restore seeds the pre-process totals and the control counters from disk. A
// missing or unreadable store is not a fatal error: the miner starts from zero
// exactly as it did before this feature, and the caller logs the reason. It
// must run before /metrics serves so the first scrape already sees the
// continued series. The second call is a no-op, because the totals are added
// into this process's counters and a second add would double them.
func (s *metricsCounterStore) restore() error {
	s.mu.Lock()
	if s.restored {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	file, err := s.load()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Nothing to restore. Latch: a store that does not exist yet is
			// created by the first flush, not by a retry here.
			s.mu.Lock()
			s.restored = true
			s.mu.Unlock()
			return nil
		}
		// Leave restored false: a transient read failure must not permanently
		// prevent a later restore from continuing the series.
		return err
	}

	s.mu.Lock()
	s.base = mergeMetricsProxyCounters(s.base, file.Proxies)
	if s.foreign == nil {
		s.foreign = map[int]minerMetricsProxyCounters{}
	}
	s.foreign = mergeMetricsProxyCounters(s.foreign, file.Proxies)
	s.retired = maxMinerMetricsByteTotals(s.retired, file.Retired)
	// The control counters are process-global and restore has just added the
	// stored value to them, so the baseline a catch-up adds on top is what
	// restore applied, not the live value it now reads.
	s.appliedControl = maxMinerControlCounters(s.appliedControl, file.Control)
	s.controlAppliedSet = true
	// Latch only after a successful load. Latching before it made a transient
	// read failure permanent for the life of the process.
	s.restored = true
	s.mu.Unlock()
	if s.addControl != nil {
		s.addControl(file.Control)
	}
	return nil
}

// catchUp raises this process's counters to what another writer has already
// stored, and is the takeover counterpart of restore. A HotSwap candidate
// registers its slots before the parent yields, so the restore that runs at
// takeover only fills the base: those entries were created while the base was
// empty and count from zero, and because they are live the aggregate skips
// their stored totals. Raising them to the parent's numbers continues the
// series instead of dropping the parent's history for exactly the proxies this
// process runs.
//
// Every raise only moves a counter up, and takes the stored value instead of
// adding it, so a catch-up that runs after another writer flushed again, or
// runs twice over the same store, cannot double count.
func (s *metricsCounterStore) catchUp() error {
	if s.storePath() == "" {
		return nil
	}
	// Serialise against flush, and read inside it: a load taken before the lock
	// can be a view the interleaving flush has already replaced. flush also
	// rewrites base with this process's live totals, so the stored source for a
	// catch-up is foreign, which only ever mirrors the file.
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	file, err := s.load()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}

	// Snapshot before the registry is copied: Register records its seed in
	// s.applied before it publishes the entry, so taking this first means
	// every slot seen here already has its record in the copy below. Taken
	// after, a slot registered in the gap would have its stored total added
	// on top of its own seed, then lose the seed when applied is committed.
	// s.mu is not held here on purpose: Register takes r.mu then s.mu.
	live := catchUpLiveSnapshot()

	s.mu.Lock()
	s.base = mergeMetricsProxyCounters(s.base, file.Proxies)
	if s.foreign == nil {
		s.foreign = map[int]minerMetricsProxyCounters{}
	}
	s.foreign = mergeMetricsProxyCounters(s.foreign, file.Proxies)
	s.retired = maxMinerMetricsByteTotals(s.retired, file.Retired)
	base := make(map[int]minerMetricsProxyCounters, len(s.foreign))
	for index, counters := range s.foreign {
		base[index] = counters
	}
	applied := make(map[int]minerMetricsProxyCounters, len(s.applied))
	for index, counters := range s.applied {
		applied[index] = counters
	}
	appliedControl := s.appliedControl
	controlAppliedSet := s.controlAppliedSet
	s.mu.Unlock()

	if !controlAppliedSet {
		appliedControl = readMinerControlCounters()
	}

	// Add the part of the stored total this process has not folded in yet.
	// Every live slot counts from zero at registration, so stored + own is the
	// true continuation; a max would drop one side or the other.
	for index, bw := range live {
		if bw == nil {
			continue
		}
		counters, ok := base[index]
		if !ok {
			continue
		}
		addCounterDelta(bw, counters, applied[index])
		applied[index] = counters
	}
	if s.addControl != nil {
		s.addControl(controlCountersDelta(file.Control, appliedControl))
	}

	s.mu.Lock()
	// Merge rather than replace: a slot registered after the copy above has
	// already recorded its own seed, and replacing the map would drop it.
	for index, counters := range applied {
		s.applied[index] = maxMinerProxyCounters(s.applied[index], counters)
	}
	s.appliedControl = maxMinerControlCounters(appliedControl, file.Control)
	s.controlAppliedSet = true
	s.mu.Unlock()
	return nil
}

// raiseMinerControlCounters takes each control counter to at least the stored
// value. It is the raise counterpart of addMinerControlCounters, so a catch-up
// after a restore that already added the same counts changes nothing.
func raiseMinerControlCounters(c minerMetricsControlCounters) {
	raiseInt64(&controlCmdsAcked, c.Acked)
	raiseInt64(&controlCmdsSet, c.Set)
	raiseInt64(&controlCmdsGet, c.Get)
	raiseInt64(&controlCmdsClear, c.Clear)
}

// raiseMetricsProxyBandwidth takes a live entry's totals to at least the
// stored ones, and never lowers bytes this process already counted.
func raiseMetricsProxyBandwidth(bw *bandwidth.ProxyBandwidth, counters minerMetricsProxyCounters) {
	if bw == nil {
		return
	}
	raiseUint64(&bw.TotalRx, counters.TotalRx)
	raiseUint64(&bw.TotalTx, counters.TotalTx)
	raiseUint64(&bw.BillableRx, counters.BillableRx)
	raiseUint64(&bw.BillableTx, counters.BillableTx)
	raiseUint64(&bw.ContractUsedIngress, counters.ContractUsedIngress)
	raiseUint64(&bw.ContractUsedEgress, counters.ContractUsedEgress)
}

// raiseUint64 moves *v up to want, and never down. The compare-and-swap loop
// keeps a byte a connection counted concurrently: an add that lands between
// the load and the swap only makes the loop retry with the higher value.
func raiseUint64(v *atomic.Uint64, want uint64) {
	for {
		cur := v.Load()
		if cur >= want {
			return
		}
		if v.CompareAndSwap(cur, want) {
			return
		}
	}
}

// addCounterDelta adds the part of stored that this process has not already
// folded in. subFloor keeps a store that went backwards from lowering a
// counter.
func addCounterDelta(bw *bandwidth.ProxyBandwidth, stored, applied minerMetricsProxyCounters) {
	if bw == nil {
		return
	}
	bw.TotalRx.Add(subFloor(stored.TotalRx, applied.TotalRx))
	bw.TotalTx.Add(subFloor(stored.TotalTx, applied.TotalTx))
	bw.BillableRx.Add(subFloor(stored.BillableRx, applied.BillableRx))
	bw.BillableTx.Add(subFloor(stored.BillableTx, applied.BillableTx))
	bw.ContractUsedIngress.Add(subFloor(stored.ContractUsedIngress, applied.ContractUsedIngress))
	bw.ContractUsedEgress.Add(subFloor(stored.ContractUsedEgress, applied.ContractUsedEgress))
}

func subFloor(v, applied uint64) uint64 {
	if v <= applied {
		return 0
	}
	return v - applied
}

// controlCountersDelta is addCounterDelta for the control counters, which are
// added through a signed atomic.
// readMinerControlCounters snapshots the live control counters, which are the
// baseline a catch-up adds the stored remainder to.
func readMinerControlCounters() minerMetricsControlCounters {
	return minerMetricsControlCounters{
		Acked: uint64(controlCmdsAcked.Load()),
		Set:   uint64(controlCmdsSet.Load()),
		Get:   uint64(controlCmdsGet.Load()),
		Clear: uint64(controlCmdsClear.Load()),
	}
}

func controlCountersDelta(stored, applied minerMetricsControlCounters) minerMetricsControlCounters {
	return minerMetricsControlCounters{
		Acked: subFloor(stored.Acked, applied.Acked),
		Set:   subFloor(stored.Set, applied.Set),
		Get:   subFloor(stored.Get, applied.Get),
		Clear: subFloor(stored.Clear, applied.Clear),
	}
}

// maxMinerControlCounters keeps the larger of two control totals, so a flush
// cannot move the stored counters backwards.
func maxMinerControlCounters(a, b minerMetricsControlCounters) minerMetricsControlCounters {
	return minerMetricsControlCounters{
		Acked: maxU64(a.Acked, b.Acked),
		Set:   maxU64(a.Set, b.Set),
		Get:   maxU64(a.Get, b.Get),
		Clear: maxU64(a.Clear, b.Clear),
	}
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

// raiseInt64 is raiseUint64 for the control counters, which are signed
// atomics. A value already at or above want is left alone.
func raiseInt64(v *atomic.Int64, want uint64) {
	for {
		cur := v.Load()
		if 0 <= cur && uint64(cur) >= want {
			return
		}
		next := want
		if next > maxInt64Value {
			next = maxInt64Value
		}
		if v.CompareAndSwap(cur, int64(next)) {
			return
		}
	}
}

// maxInt64Value caps a stored uint64 counter raise, which cannot be represented
// as the int64 the control counters use.
const maxInt64Value uint64 = 1<<63 - 1

// flush writes the merged totals to the store atomically, unless the store
// already says the same thing.
//
// The write merges with whatever is already on disk using max() per series.
// That is the multi-process guard: during a hotswap the draining parent and the
// promoted child both flush to the same path, and a plain overwrite would let
// the process that loaded the older snapshot push the file backwards. With the
// merge no writer can lower a series, so the file only moves forward.
func (s *metricsCounterStore) flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	path := s.storePath()
	if path == "" {
		return nil
	}
	now := s.now()
	snap := s.snapshot()
	if snap.Proxies == nil {
		snap.Proxies = map[int]minerMetricsProxyCounters{}
	}
	live := make(map[int]struct{}, len(snap.Proxies))
	for index, counters := range snap.Proxies {
		counters.LastSeen = now.Unix()
		snap.Proxies[index] = counters
		live[index] = struct{}{}
	}

	prior, priorErr := s.load()
	if priorErr != nil && errors.Is(priorErr, errMetricsCountersNewer) {
		// A newer build wrote this store. Its fields are unknown to us, so
		// rewriting it in this build's schema would destroy them; leave it
		// alone and keep running from zero rather than corrupt newer state.
		return priorErr
	}
	merged := s.baseSnapshot()
	if priorErr == nil {
		merged = mergeMetricsProxyCounters(merged, prior.Proxies)
	}
	merged = mergeMetricsProxyCounters(merged, snap.Proxies)
	proxies := pruneMetricsProxyCounters(merged, live, now)

	// Anything the prune drops leaves base for good, so its totals move into
	// the retired accumulator before base is replaced below.
	retired := s.retireDropped(merged, proxies)

	// Keep the merged map in memory too, pruned the same way: a proxy that
	// unregisters right after this flush must not seed its replacement below
	// the total just written, and the base must not grow for the life of a
	// churning node.
	s.setBase(proxies)

	// The file now holds this process's own totals, so the applied baseline has
	// to advance to exactly what was written. Without this the next catch-up
	// reads the file it just wrote, finds the whole value "new", and adds this
	// process's own bytes a second time.
	s.recordApplied(proxies)

	// A process that restored, or that flushed before catching up, can hold
	// LOWER control totals than the store. Merge rather than assign, or the
	// stored command counts move backwards.
	control := snap.Control
	if priorErr == nil {
		control = maxMinerControlCounters(control, prior.Control)
	}
	file := minerMetricsCountersFile{
		Version: metricsCounterStoreVersion,
		SavedAt: now.UTC(),
		PID:     os.Getpid(),
		Proxies: proxies,
		Control: control,
		Retired: retired,
	}
	if priorErr == nil && metricsCountersUnchanged(file, prior, now) {
		return nil
	}
	blob, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return s.write(path, append(blob, '\n'), 0o600)
}

// proxySeed returns the stored totals a registry entry for index starts from.
func (s *metricsCounterStore) proxySeed(index int) (minerMetricsProxyCounters, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counters, ok := s.base[index]
	if ok {
		// A slot created after a restore STARTS at the seed rather than at
		// zero, so the catch-up must treat the seed as already applied. Without
		// this the next catch-up added the whole stored total a second time.
		if s.applied == nil {
			s.applied = map[int]minerMetricsProxyCounters{}
		}
		s.applied[index] = maxMinerProxyCounters(s.applied[index], counters)
	}
	return counters, ok
}

// recordApplied marks written totals as already folded into the live counters.
func (s *metricsCounterStore) recordApplied(written map[int]minerMetricsProxyCounters) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index, counters := range written {
		if s.applied == nil {
			s.applied = map[int]minerMetricsProxyCounters{}
		}
		s.applied[index] = maxMinerProxyCounters(s.applied[index], counters)
	}
}

// maxMinerProxyCounters keeps the larger of two per-index totals.
func maxMinerProxyCounters(a, b minerMetricsProxyCounters) minerMetricsProxyCounters {
	return minerMetricsProxyCounters{
		TotalRx:             maxU64(a.TotalRx, b.TotalRx),
		TotalTx:             maxU64(a.TotalTx, b.TotalTx),
		BillableRx:          maxU64(a.BillableRx, b.BillableRx),
		BillableTx:          maxU64(a.BillableTx, b.BillableTx),
		ContractUsedIngress: maxU64(a.ContractUsedIngress, b.ContractUsedIngress),
		ContractUsedEgress:  maxU64(a.ContractUsedEgress, b.ContractUsedEgress),
	}
}

// addMinerMetricsByteTotals sums two byte totals.
func addMinerMetricsByteTotals(a, b minerMetricsByteTotals) minerMetricsByteTotals {
	return minerMetricsByteTotals{
		TotalRx:             a.TotalRx + b.TotalRx,
		TotalTx:             a.TotalTx + b.TotalTx,
		BillableRx:          a.BillableRx + b.BillableRx,
		BillableTx:          a.BillableTx + b.BillableTx,
		ContractUsedIngress: a.ContractUsedIngress + b.ContractUsedIngress,
		ContractUsedEgress:  a.ContractUsedEgress + b.ContractUsedEgress,
	}
}

// maxMinerMetricsByteTotals takes the larger of two byte totals. The retired
// accumulator is merged this way, never summed: during a hotswap two writers
// each add the entries their own view dropped, and adding both would count the
// same entry twice.
func maxMinerMetricsByteTotals(a, b minerMetricsByteTotals) minerMetricsByteTotals {
	return minerMetricsByteTotals{
		TotalRx:             maxU64(a.TotalRx, b.TotalRx),
		TotalTx:             maxU64(a.TotalTx, b.TotalTx),
		BillableRx:          maxU64(a.BillableRx, b.BillableRx),
		BillableTx:          maxU64(a.BillableTx, b.BillableTx),
		ContractUsedIngress: maxU64(a.ContractUsedIngress, b.ContractUsedIngress),
		ContractUsedEgress:  maxU64(a.ContractUsedEgress, b.ContractUsedEgress),
	}
}

// retireDropped folds the totals of entries a prune removed from base into the
// retired accumulator and returns it. Every writer derives the same set from
// its own view, so callers merge the persisted accumulator with max.
func (s *metricsCounterStore) retireDropped(merged, kept map[int]minerMetricsProxyCounters) minerMetricsByteTotals {
	var dropped minerMetricsByteTotals
	for index, counters := range merged {
		if _, ok := kept[index]; ok {
			continue
		}
		dropped.TotalRx += counters.TotalRx
		dropped.TotalTx += counters.TotalTx
		dropped.BillableRx += counters.BillableRx
		dropped.BillableTx += counters.BillableTx
		dropped.ContractUsedIngress += counters.ContractUsedIngress
		dropped.ContractUsedEgress += counters.ContractUsedEgress
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retired = addMinerMetricsByteTotals(s.retired, dropped)
	return s.retired
}

// catchUpLiveSnapshot is the live-registry read catchUp uses. It is a variable
// so a test can register a slot in the window the ordering fix is about;
// production never assigns it.
var catchUpLiveSnapshot = func() map[int]*bandwidth.ProxyBandwidth {
	return controlRuntime.bandwidthSnapshot()
}

// retiredTotals sums the stored totals of every index this process does not
// run. A live index is skipped because its registry entry was seeded with the
// same totals, so counting both would double it.
func (s *metricsCounterStore) retiredTotals(live map[int]struct{}) minerMetricsByteTotals {
	var totals minerMetricsByteTotals
	s.mu.Lock()
	for index, counters := range s.base {
		if _, ok := live[index]; ok {
			continue
		}
		totals.TotalRx += counters.TotalRx
		totals.TotalTx += counters.TotalTx
		totals.BillableRx += counters.BillableRx
		totals.BillableTx += counters.BillableTx
	}
	totals = addMinerMetricsByteTotals(totals, s.retired)
	s.mu.Unlock()
	return totals
}

func (s *metricsCounterStore) baseSnapshot() map[int]minerMetricsProxyCounters {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int]minerMetricsProxyCounters, len(s.base))
	for index, counters := range s.base {
		out[index] = counters
	}
	return out
}

func (s *metricsCounterStore) setBase(base map[int]minerMetricsProxyCounters) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.base = base
}

// startFlush runs the periodic writer. Calling it twice is a no-op, so a
// process that restores and then takes over a hotswap cannot start two
// writers.
func (s *metricsCounterStore) startFlush(interval time.Duration) {
	if s.storePath() == "" {
		return
	}
	if interval <= 0 {
		interval = defaultMetricsCounterFlushInterval
	}
	s.mu.Lock()
	if s.stop != nil {
		s.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	// The writer is a long-running loop, so it runs under the supervisor: a
	// panic in one flush restarts the writer with backoff instead of ending
	// persistence for the rest of the process. stopFlush cancels this context,
	// so a shutdown that lands while the writer is panicking or backing off
	// ends the supervisor instead of waiting out the backoff.
	ctx, cancel := context.WithCancel(context.Background())
	s.stop, s.done, s.cancel = stop, done, cancel
	s.mu.Unlock()

	ref := newLoopRef("metrics_counter_flush")
	// Failures repeat every tick, so the last reported message is kept across
	// runs: a restart must not re-log an unchanged failure.
	var lastErr string
	go func() {
		defer close(done)
		superviseLoop(ctx, ref, func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					err := s.flush()
					if err == nil {
						lastErr = ""
						continue
					}
					// A state directory that cannot be written fails every tick.
					// Report the first failure and then only a CHANGE, so a
					// read-only home does not fill the log with one line per
					// interval.
					if msg := err.Error(); msg != lastErr {
						tlog("[metrics] counter flush failed: %v\n", err)
						lastErr = msg
					}
				case <-stop:
					cancel()
					return
				}
			}
		}, nil)
	}()
}

// stopFlush stops the periodic writer and waits for it to exit. It does not
// write.
func (s *metricsCounterStore) stopFlush() {
	s.mu.Lock()
	stop, done, cancel := s.stop, s.done, s.cancel
	s.stop, s.done, s.cancel = nil, nil, nil
	s.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	if cancel != nil {
		// Ends the supervisor even if the writer is mid-panic or in backoff.
		cancel()
	}
	<-done
}

// metricsCountersUnchanged reports whether writing next would leave the store
// saying the same thing prior already does. saved_at, the writer's pid and
// every proxy's last_seen are stamped afresh on each flush, so they are left
// out of the comparison; a store older than metricsCounterUnchangedRewriteAge
// is rewritten anyway so those stamps do not go stale on an idle node.
func metricsCountersUnchanged(next, prior minerMetricsCountersFile, now time.Time) bool {
	if prior.SavedAt.IsZero() || now.Sub(prior.SavedAt) >= metricsCounterUnchangedRewriteAge {
		return false
	}
	next.SavedAt, prior.SavedAt = time.Time{}, time.Time{}
	next.PID, prior.PID = 0, 0
	next.Proxies = withoutMetricsProxyLastSeen(next.Proxies)
	prior.Proxies = withoutMetricsProxyLastSeen(prior.Proxies)
	a, errA := json.Marshal(next)
	b, errB := json.Marshal(prior)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func withoutMetricsProxyLastSeen(in map[int]minerMetricsProxyCounters) map[int]minerMetricsProxyCounters {
	if len(in) == 0 {
		return nil
	}
	out := make(map[int]minerMetricsProxyCounters, len(in))
	for index, counters := range in {
		counters.LastSeen = 0
		out[index] = counters
	}
	return out
}

// mergeMetricsProxyCounters takes the larger of each pair. Every field is a
// monotonic total, so max() is the right merge: it never moves a series
// backwards and it is idempotent.
func mergeMetricsProxyCounters(a, b map[int]minerMetricsProxyCounters) map[int]minerMetricsProxyCounters {
	if len(a) == 0 && len(b) == 0 {
		return map[int]minerMetricsProxyCounters{}
	}
	out := make(map[int]minerMetricsProxyCounters, len(a)+len(b))
	for index, counters := range a {
		out[index] = counters
	}
	for index, counters := range b {
		prior := out[index]
		prior.TotalRx = max(prior.TotalRx, counters.TotalRx)
		prior.TotalTx = max(prior.TotalTx, counters.TotalTx)
		prior.BillableRx = max(prior.BillableRx, counters.BillableRx)
		prior.BillableTx = max(prior.BillableTx, counters.BillableTx)
		prior.ContractUsedIngress = max(prior.ContractUsedIngress, counters.ContractUsedIngress)
		prior.ContractUsedEgress = max(prior.ContractUsedEgress, counters.ContractUsedEgress)
		prior.LastSeen = max(prior.LastSeen, counters.LastSeen)
		out[index] = prior
	}
	return out
}

// pruneMetricsProxyCounters drops entries beyond the retention window and, if
// the cap is still exceeded, the oldest non-live entries. A live entry is never
// dropped.
func pruneMetricsProxyCounters(in map[int]minerMetricsProxyCounters, live map[int]struct{}, now time.Time) map[int]minerMetricsProxyCounters {
	cutoff := now.Add(-metricsCounterProxyRetention).Unix()
	out := make(map[int]minerMetricsProxyCounters, len(in))
	for index, counters := range in {
		if _, ok := live[index]; !ok && counters.LastSeen < cutoff {
			continue
		}
		out[index] = counters
	}
	if len(out) <= metricsCounterProxyMaxEntries {
		return out
	}
	prunable := make([]int, 0, len(out))
	for index := range out {
		if _, ok := live[index]; !ok {
			prunable = append(prunable, index)
		}
	}
	sort.Slice(prunable, func(i, j int) bool {
		if out[prunable[i]].LastSeen != out[prunable[j]].LastSeen {
			return out[prunable[i]].LastSeen < out[prunable[j]].LastSeen
		}
		return prunable[i] < prunable[j]
	})
	for _, index := range prunable {
		if len(out) <= metricsCounterProxyMaxEntries {
			break
		}
		delete(out, index)
	}
	return out
}

// metricsCounterLiveSnapshot reads the miner's live cumulative counters: the
// bandwidth registry and the control socket counters.
func metricsCounterLiveSnapshot() minerMetricsCounterSnapshot {
	snap := minerMetricsCounterSnapshot{
		Proxies: map[int]minerMetricsProxyCounters{},
		Control: minerMetricsControlCounters{
			Acked: clampToUint64(controlCmdsAcked.Load()),
			Set:   clampToUint64(controlCmdsSet.Load()),
			Get:   clampToUint64(controlCmdsGet.Load()),
			Clear: clampToUint64(controlCmdsClear.Load()),
		},
	}
	for index, bw := range controlRuntime.bandwidthSnapshot() {
		if bw == nil {
			continue
		}
		snap.Proxies[index] = minerMetricsProxyCounters{
			TotalRx:             bw.TotalRx.Load(),
			TotalTx:             bw.TotalTx.Load(),
			BillableRx:          bw.BillableRx.Load(),
			BillableTx:          bw.BillableTx.Load(),
			ContractUsedIngress: bw.ContractUsedIngress.Load(),
			ContractUsedEgress:  bw.ContractUsedEgress.Load(),
		}
	}
	return snap
}

// addMinerControlCounters adds a restored store's control counts into this
// process's counters.
func addMinerControlCounters(c minerMetricsControlCounters) {
	controlCmdsAcked.Add(int64(clampU64(c.Acked)))
	controlCmdsSet.Add(int64(clampU64(c.Set)))
	controlCmdsGet.Add(int64(clampU64(c.Get)))
	controlCmdsClear.Add(int64(clampU64(c.Clear)))
}

// clampU64 keeps a stored counter representable as the int64 the atomics use.
// Without it a corrupt or absurd store converts to a negative and subtracts.
func clampU64(v uint64) uint64 {
	if v > maxInt64Value {
		return maxInt64Value
	}
	return v
}

func clampToUint64(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// writeMetricsCounterFileAtomic replaces path with data: temp file, fsync,
// rename, then a best-effort directory fsync so the rename is durable. This is
// the write the unchanged check exists to avoid.
func writeMetricsCounterFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	removeStaleMetricsCounterTemps(dir)
	tmp, err := os.CreateTemp(dir, ".metrics-counters-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Close and remove on every return path: a panic between here and the
	// rename would otherwise leak the descriptor and, on Windows, leave the
	// temp file undeletable.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp already opens at 0600, which is the mode this store wants. A
	// filesystem that cannot chmod (CIFS, FAT) must not fail the write.
	_ = os.Chmod(tmpPath, perm)
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	// Best effort: a directory fsync is what makes the rename durable, but the
	// call is unavailable or meaningless on some platforms. The data is
	// already fsynced; failing here would report a write that did happen.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// removeStaleMetricsCounterTemps deletes temp files a killed writer left
// behind. Only files older than ten minutes are touched, so a concurrent
// process's in-progress write is never removed under it.
func removeStaleMetricsCounterTemps(dir string) {
	stale, err := filepath.Glob(filepath.Join(dir, ".metrics-counters-*.tmp"))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-10 * time.Minute)
	for _, path := range stale {
		if info, err := os.Stat(path); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
	}
}

// --- process wiring ---

// metricsCounters is the process-wide store. Its path is armed by
// startMetricsCounterPersistence and stays "" (every operation a no-op) until
// then, so tests that serve /metrics never touch the real state directory.
var metricsCounters = newMetricsCounterStore("")

// metricsCounterProxySeedFunc is installed on a new bandwidth registry so each
// proxy's counters start from the totals the previous process recorded.
func metricsCounterProxySeedFunc(index int) bandwidth.ProxySeed {
	counters, ok := metricsCounters.proxySeed(index)
	if !ok {
		return bandwidth.ProxySeed{}
	}
	return bandwidth.ProxySeed{
		TotalRx:             counters.TotalRx,
		TotalTx:             counters.TotalTx,
		BillableRx:          counters.BillableRx,
		BillableTx:          counters.BillableTx,
		ContractUsedIngress: counters.ContractUsedIngress,
		ContractUsedEgress:  counters.ContractUsedEgress,
	}
}

// startMetricsCounterPersistence arms the store at the miner's state-dir path,
// restores the previous process's totals and starts the periodic writer. It is
// called from the metrics startup path, before the /metrics listener binds, so
// the first scrape already sees the continued series. A second call (a hotswap
// takeover) restores nothing and keeps the one writer.
func startMetricsCounterPersistence() {
	path := mustStatePathQuiet(metricsCounterStoreName)
	if path == "" {
		return
	}
	armed := metricsCounters.arm(path)
	if err := metricsCounters.restore(); err != nil {
		tlog("[metrics] counters not restored from %s: %v (starting from zero)\n", path, err)
	}
	if armed {
		metricsCounters.startFlush(defaultMetricsCounterFlushInterval)
	}
}

// flushMetricsCounters writes the current totals. It is safe to call when the
// store is not armed, and safe to call concurrently with the periodic writer
// (the write is serialised and the unchanged check makes a redundant call
// free).
func flushMetricsCounters() {
	if err := metricsCounters.flush(); err != nil {
		if !errors.Is(err, errMetricsCountersNewer) {
			tlog("[metrics] counter flush: %v\n", err)
		}
	}
}

// stopMetricsCounterPersistence stops the periodic writer and writes the final
// totals, for the clean-shutdown path.
func stopMetricsCounterPersistence() {
	metricsCounters.stopFlush()
	flushMetricsCounters()
}

// catchUpMetricsCounters catches this process up to whatever another writer has
// stored. It runs at a HotSwap takeover, after the store is armed and restored,
// because the candidate registered its slots while the parent was still the
// writer. A failure is logged, never fatal: the totals then simply continue from
// what this process counted.
func catchUpMetricsCounters() {
	if err := metricsCounters.catchUp(); err != nil {
		tlog("[metrics] counter catch-up after takeover: %v\n", err)
	}
}

// catchUpMetricsCountersAfterDrain re-runs the catch-up once the draining parent
// has finished. The parent writes the store again at the end of its drain, which
// is after this process took over, so without this the last seconds of traffic
// the parent counted would stay out of this process's totals until the next
// restart. The wait is the same drain window the audit reconcile uses, and the
// run's context ends it on a shutdown.
func catchUpMetricsCountersAfterDrain(ctx context.Context) {
	timer := time.NewTimer(HotSwapDrainTimeout + auditReconcileGrace)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	if err := metricsCounters.catchUp(); err != nil {
		tlog("[metrics] counter catch-up after the parent's drain: %v\n", err)
	}
}
