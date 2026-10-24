package miner

// contract_metrics.go: the miner's contract accounting, from the provider
// contract feed.
//
// The provider counts contract outcomes from a per-proxy
// connect.ContractManager status callback (provider/contract_metrics.go,
// registerContractCallback). The miner cannot do that: it owns no connect
// client. Each slot is an sdk.DeviceLocal, the SDK builds the provider client
// inside it, and the pinned SDK exports neither that client nor its contract
// manager.
//
// What it does export is the provider's own contract feed: every provider
// device delivers ContractDetails through
// AddProviderEgressContractDetailsChangeListener and
// AddProviderIngressContractDetailsChangeListener. That feed is produced from
// the provider client's ContractManager stats callback
// (sdk/device_local.go, updateProviderContractStatsEvents), so each contract it
// names is one the platform actually granted this provider -- an acquired
// contract.
//
// The same details carry the accounting the contract is worth:
//
//	ContractId            the platform-minted, single-use contract id
//	ContractByteCount     the bytes purchased for that contract
//	ContractUsedByteCount the bytes used so far, per contract, point in time
//	Status                ContractStatusOpen, or ContractStatusClosed once as
//	                      a tombstone before the SDK evicts the contract
//
// One grant can be reported on both feeds. The pinned SDK's contract tracker
// keys the send and receive entries of a contract by one id, shares one
// sequence counter across them, and evicts a closed entry only once no sibling
// entry remains (device_local.go, deviceContractTracker and
// evictReportedClosed); connect's closeContractStats likewise closes both
// receive values for one id. So a contract is identified here by (slot, id):
// the feed says how a detail arrived, not which contract it is, and a grant
// live on both feeds is counted and sized once.
//
// so this file publishes, process-wide:
//
//	urnet_contracts_total{result="acquired"}  contracts acquired since start
//	urnet_contracts_open                      contracts open right now
//	urnet_contract_limit_bytes                purchased bytes over open contracts
//	urnet_contract_used_bytes                 used bytes over open contracts
//	urnet_contract_utilization                mean used/purchased, open contracts, percent
//
// contractStatsSnapshot exposes the same reading to the [profit] line:
// cumulative acquired for contracts=, and the open book for avg_util=%d%% (or
// n/a). readProfitSample calls it once per heartbeat, so the line and the
// scrape render one snapshot rather than two reads that could disagree.
//
// ContractUsedByteCount is a per-contract running figure that the feed
// republishes each stats epoch; it is NOT the cumulative settled bytes this
// process has transferred (that is urnet_lifetime_billable_bytes_total). A
// contract that closes stops contributing, so urnet_contract_used_bytes can go
// down -- it is a gauge over the open set, not a counter.
//
// Refusals are NOT in that feed, and the miner leaves the denied series absent
// rather than reporting a zero it cannot measure. A refused contract never
// opens, so the manager never emits a stats event for it. The only refusal
// signal the SDK exports is DeviceLocal.GetContractStatus, a windowed
// Premium/InsufficientBalance/NoPermission summary with no count, and the
// per-contract status callback the provider uses
// (ContractManager.AddContractStatusCallback) is unreachable without the
// client. metrics_seams.go states the same limit at the seam.

import (
	"math"
	"sync"

	"github.com/urnetwork/sdk"
)

// contractOutcomeSource is the slice of *sdk.DeviceLocal this needs: the two
// provider contract feeds. An interface so a test can drive it without a
// device, the same way proxyTransportState stands in for the transport watch.
type contractOutcomeSource interface {
	AddProviderEgressContractDetailsChangeListener(listener sdk.ContractDetailsChangeListener) sdk.Sub
	AddProviderIngressContractDetailsChangeListener(listener sdk.ContractDetailsChangeListener) sdk.Sub
}

var _ contractOutcomeSource = (*sdk.DeviceLocal)(nil)

// contractFeed names one of the two provider contract feeds. A feed is part of
// how a detail was delivered, never part of the contract's identity: the same
// platform grant can be live on both feeds at once (see the file comment), so
// keying by feed would count that grant twice.
type contractFeed uint8

const (
	contractFeedEgress contractFeed = iota
	contractFeedIngress
)

// bit is the contractOpenState.open bit for this feed.
func (self contractFeed) bit() uint8 { return 1 << self }

// contractKey identifies one open contract. The slot is the registration that
// delivered it: two slots are two provider devices, and an id is only unique
// within the platform contract that minted it, so the accounting must not merge
// them. The feed is deliberately absent: one grant can be live on both feeds
// and is still one contract.
type contractKey struct {
	slot int64
	id   string
}

// contractOpenState is what one open contract contributes to the gauges. A
// grant can be open on both feeds at once, so the state remembers which feeds
// still report it open and folds their byte reports into one reading.
type contractOpenState struct {
	limitBytes int64
	usedBytes  int64
	// open is the bitmask of contractFeed values still reporting this
	// contract open. One feed's tombstone does not close a contract the other
	// feed still holds open.
	open uint8
}

// merge folds one feed's report of the contract into the reading. A grant live
// on both feeds is one contract, so its purchased size is taken once and the
// larger of the two used readings wins: adding them would count the grant
// twice, and a lower or stale report must not subtract from the open book.
func (self *contractOpenState) merge(feed contractFeed, details *sdk.ContractDetails) {
	limit := details.ContractByteCount
	if limit < 0 {
		// A negative count is not a reading. Clamp it so one malformed detail
		// cannot subtract from a sum over every open contract.
		limit = 0
	}
	if self.limitBytes < limit {
		self.limitBytes = limit
	}
	used := details.ContractUsedByteCount
	if used < 0 {
		used = 0
	}
	if self.usedBytes < used {
		self.usedBytes = used
	}
	self.open |= feed.bit()
}

// contractClosedMemory bounds the record of closed contracts. Ids are platform
// minted and single use, so this only has to outlive the SDK's own reporting:
// enough recent ids to swallow a duplicate tombstone, the second feed's
// tombstone, or a late detail that lands after the SDK evicted the contract and
// with it the sequence guard. A record that only ever grew would leak for the
// life of the process.
const contractClosedMemory = 4096

// contractClosedSet remembers recently closed contracts, oldest evicted first.
type contractClosedSet struct {
	index map[contractKey]struct{}
	ring  []contractKey
	next  int
}

func (self *contractClosedSet) has(key contractKey) bool {
	_, ok := self.index[key]
	return ok
}

// add remembers one closed contract, evicting the oldest when full.
func (self *contractClosedSet) add(key contractKey) {
	if self.index == nil {
		self.index = map[contractKey]struct{}{}
	}
	if _, ok := self.index[key]; ok {
		return
	}
	if len(self.ring) < contractClosedMemory {
		self.ring = append(self.ring, key)
	} else {
		delete(self.index, self.ring[self.next])
		self.ring[self.next] = key
		self.next = (self.next + 1) % contractClosedMemory
	}
	self.index[key] = struct{}{}
}

// forget drops everything remembered for one slot. A retired slot's ids can
// never be reported again, so keeping them would only hold memory.
//
// The survivors keep their order: the ring is walked from its oldest entry
// rather than rebuilt from the index map. Map iteration would put the survivors
// in random order, so the next eviction would take an arbitrary id instead of
// the oldest -- and forgetting a recent id early lets a late duplicate
// tombstone be counted as a new contract.
func (self *contractClosedSet) forget(slot int64) {
	if len(self.index) == 0 {
		return
	}
	old := self.ring
	kept := make([]contractKey, 0, len(old))
	for i := 0; i < len(old); i += 1 {
		key := old[(self.next+i)%len(old)]
		if key.slot == slot {
			delete(self.index, key)
			continue
		}
		kept = append(kept, key)
	}
	self.ring = kept
	self.next = 0
}

// contractStats is one consistent read of the whole process accounting, so a
// scrape cannot mix a count from before a close with bytes from after it.
//
// The two halves answer different questions and must not be swapped:
//
//   - Acquired is cumulative over the process lifetime: every contract the
//     platform established for this provider since start, open or since
//     closed. It is what the provider's [profit] line calls contracts= and
//     what urnet_contracts_total{result="acquired"} publishes.
//   - Open and the Open* byte gauges are the open book right now. They are
//     point-in-time readings over the contracts still open, so they fall when
//     a contract closes; they are not lifetime totals and must not be used
//     where a cumulative count is meant.
type contractStats struct {
	Acquired int64
	Open     int
	// OpenLimitBytes is the bytes purchased across the contracts open now.
	OpenLimitBytes int64
	// OpenUsedBytes is the bytes used so far on those contracts, as the feed
	// last reported each one. Per-contract and point-in-time: it is NOT the
	// cumulative settled bytes this process has transferred (that is
	// urnet_lifetime_billable_bytes_total), and it can go down when a contract
	// closes.
	OpenUsedBytes int64
	// AvgUtilPct is the mean used/purchased over the open contracts that
	// report a positive purchased size, as a percentage in [0, 100]. NaN when
	// that mean is undefined, so "no data" never renders as 0%.
	AvgUtilPct float64
}

// contractOutcomeTotals is the process-wide accounting: contracts acquired
// since this process started, and the contracts open right now, summed over
// every provider slot. One lock covers both so a reading is a snapshot rather
// than a smear of two epochs.
type contractOutcomeTotals struct {
	mu       sync.Mutex
	slots    int64
	acquired int64
	// sourced records that a slot has ever registered a feed. It is not a
	// live-slot count: the cumulative counter is process-lifetime, so it must
	// stay published after every slot retires rather than flap to absent.
	sourced bool
	// active is the set of slots whose feeds may still deliver. Slot ids are
	// never reused, so a detail from any other slot is stale by construction.
	active map[int64]struct{}
	open   map[contractKey]contractOpenState
	// closed remembers recently closed contracts so a repeated or late report
	// cannot be counted as a new acquisition.
	closed contractClosedSet
}

var minerContractOutcomes = &contractOutcomeTotals{}

// register reserves this slot's id, marks it active, and records that this
// process has a contract source at all.
func (self *contractOutcomeTotals) register() (slot int64) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.slots += 1
	self.sourced = true
	if self.active == nil {
		self.active = map[int64]struct{}{}
	}
	self.active[self.slots] = struct{}{}
	return self.slots
}

// retire drops a slot's feed and everything it had open. A retired slot is no
// longer part of what this process is providing, so leaving its contracts in
// the open set would keep counting work from a device that is gone.
//
// Marking the slot inactive is what makes the drop final. sdk.Sub.Close is only
// a Remove from the device's listener registry while the device dispatches over
// a snapshot of it (device_local.go, the provider contract details listeners),
// so a callback already in flight lands here after Close has returned. Without
// the active check observe would re-insert that contract into the open set
// after this sweep, and since a slot id is never reused nothing would ever
// retire it again -- a permanent ghost in the open book.
func (self *contractOutcomeTotals) retire(slot int64) {
	self.mu.Lock()
	defer self.mu.Unlock()
	delete(self.active, slot)
	for key := range self.open {
		if key.slot == slot {
			delete(self.open, key)
		}
	}
	self.closed.forget(slot)
}

// observe folds one contract detail into the accounting. See
// contractOutcomeDirection.ContractDetailsChanged for the id gate.
func (self *contractOutcomeTotals) observe(slot int64, feed contractFeed, id string, details *sdk.ContractDetails) {
	key := contractKey{slot: slot, id: id}
	self.mu.Lock()
	defer self.mu.Unlock()
	if _, active := self.active[slot]; !active {
		// A callback from a slot that has retired (see retire): it must not
		// resurrect the slot's contract into the open book, and it is not
		// work this process is providing any more.
		return
	}
	if self.closed.has(key) {
		// Already closed: a duplicate tombstone, the second feed's tombstone
		// for the same grant, or a detail that arrived after the SDK evicted
		// the contract. The SDK's sequence guard is deleted with the contract,
		// so nothing upstream filters these by the time they reach here.
		return
	}
	state, isOpen := self.open[key]
	if details.Status == sdk.ContractStatusClosed {
		if !isOpen {
			// The SDK's stats gate can fold a whole lifecycle into one batch,
			// so the first report for a contract may be its tombstone. It was
			// still a contract this provider acquired.
			self.acquired += 1
			self.closed.add(key)
			return
		}
		// The tombstone is this feed's final report. The same grant can be
		// open on the other feed, in which case it is still one open contract.
		state.open &^= feed.bit()
		if state.open != 0 {
			self.open[key] = state
			return
		}
		delete(self.open, key)
		self.closed.add(key)
		return
	}
	if isOpen {
		// Still open, re-reported by the next stats epoch (or by the other
		// feed). Contract byte counts are per-contract running figures: the
		// count of open contracts does not change, the bytes do.
		state.merge(feed, details)
		self.open[key] = state
		return
	}
	self.acquired += 1
	if self.open == nil {
		self.open = map[contractKey]contractOpenState{}
	}
	var opened contractOpenState
	opened.merge(feed, details)
	self.open[key] = opened
}

// reading returns one consistent snapshot of the process accounting, plus
// whether a slot has ever registered a feed. That flag is what lets the scrape
// tell "no contract acquired yet" (a real 0) from "this process is not
// providing, so it has nothing to report" (absent), and it stays set after
// every slot retires: the cumulative counter is process-lifetime, so removing
// the family on the last retire would make a scrape see the counter flap away
// and come back. Only "never registered" is absent.
func (self *contractOutcomeTotals) reading() (stats contractStats, sourced bool) {
	self.mu.Lock()
	defer self.mu.Unlock()
	stats.Acquired = self.acquired
	var utilSum float64
	var utilCount int
	for _, state := range self.open {
		stats.Open += 1
		stats.OpenLimitBytes += state.limitBytes
		stats.OpenUsedBytes += state.usedBytes
		// A contract with no purchased bytes has no utilization to average:
		// counting it as 0% used would drag the mean down with a contract that
		// was never sized.
		if 0 < state.limitBytes {
			utilSum += float64(state.usedBytes) / float64(state.limitBytes)
			utilCount += 1
		}
	}
	stats.AvgUtilPct = math.NaN()
	if 0 < utilCount {
		// A percentage, not a ratio: the [profit] line renders this as
		// avg_util=<pct>%, and the gauge is published with the same unit.
		stats.AvgUtilPct = 100 * utilSum / float64(utilCount)
	}
	return stats, self.sourced
}

// contractOutcomeDirection delivers one provider feed's contract details into
// the process accounting. The id gate lives in contractOutcomeTotals.observe: a
// contract is counted once, on its first detail, and dropped when its close
// tombstone arrives. That keeps the open set bounded by the contracts open
// right now instead of by every contract the process ever held -- contract ids
// are platform minted and single use, so a set that only ever grew would leak
// for the life of the process. Recently closed ids are remembered separately,
// also bounded, so a repeated close cannot be mistaken for a new contract.
type contractOutcomeDirection struct {
	totals *contractOutcomeTotals
	slot   int64
	feed   contractFeed
}

// ContractDetailsChanged records one contract detail delivery
// (sdk.ContractDetailsChangeListener). The same contract is delivered again on
// every stats epoch while it is open, so this gate is what makes the result a
// count of contracts rather than a count of samples.
func (self *contractOutcomeDirection) ContractDetailsChanged(details *sdk.ContractDetails) {
	if details == nil || details.ContractId == nil {
		return
	}
	self.totals.observe(self.slot, self.feed, details.ContractId.String(), details)
}

// contractStatsSnapshot reports the process-wide contract accounting: how many
// contracts the platform has established since start (acquired, the same claim
// as the provider's contracts= field), and the open book right now -- how many
// contracts are open, the bytes purchased and used across them, and the mean
// utilization as a percentage.
//
// ok says whether this process has a contract source at all (a slot has
// registered a feed), not whether anything is open. Acquired is a real reading
// even when it is 0 or every contract has closed, so a process that acquired
// and then closed all of its contracts still reports ok with its count. The
// open half can be empty while ok is true; AvgUtilPct is then NaN and a
// renderer must print "n/a", never "0%", which would claim nothing was used
// rather than that nothing is known. AvgUtilPct is likewise NaN when open
// contracts exist but none reports a positive purchased size.
//
// It exists so the [profit] line can render contracts= and avg_util= at
// assembly time from the same numbers the scrape publishes, rather than
// re-deriving them. Acquired and Open are separate on purpose: contracts= is
// cumulative and must not be filled from Open.
func contractStatsSnapshot() (stats contractStats, ok bool) {
	return minerContractOutcomes.reading()
}

// registerContractOutcomes attaches the process contract accounting to one
// slot's provider contract feeds and returns the teardown. Call it after the
// device close has been deferred so the listeners stop while the device is
// still live, and defer the teardown from the same goroutine that registered.
//
// Closing the subscriptions is not enough on its own: the SDK dispatches over a
// snapshot of its listener registry, so a callback already in flight still
// arrives after Close. retire marks the slot inactive under the same lock the
// callback takes, so the late detail is dropped instead of re-entering the open
// book (see contractOutcomeTotals.retire).
//
// Every slot registers, including the direct identity. The provider's
// registerContractCallback skips the direct proxy (it runs only when
// proxySettings != nil), but the miner's direct slot is a full provider device
// with its own contract feed, and leaving it out would understate what this
// process acquired.
func registerContractOutcomes(device contractOutcomeSource) (retire func()) {
	if device == nil {
		return func() {}
	}
	totals := minerContractOutcomes
	slot := totals.register()
	egress := &contractOutcomeDirection{totals: totals, slot: slot, feed: contractFeedEgress}
	ingress := &contractOutcomeDirection{totals: totals, slot: slot, feed: contractFeedIngress}
	subs := []sdk.Sub{
		device.AddProviderEgressContractDetailsChangeListener(egress),
		device.AddProviderIngressContractDetailsChangeListener(ingress),
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			for _, sub := range subs {
				if sub != nil {
					sub.Close()
				}
			}
			totals.retire(slot)
		})
	}
}

func init() {
	metricsSeamContracts = func() *seamContracts {
		stats, sourced := minerContractOutcomes.reading()
		if !sourced {
			// No slot has ever registered a provider contract feed: there is
			// no data source, which is not the same reading as zero contracts.
			return nil
		}
		return &seamContracts{
			Acquired:       stats.Acquired,
			Open:           stats.Open,
			OpenLimitBytes: stats.OpenLimitBytes,
			OpenUsedBytes:  stats.OpenUsedBytes,
			AvgUtilPct:     stats.AvgUtilPct,
		}
	}
}
