package miner

// provider_billing.go: the two independent measurements of one slot's relayed
// traffic, folded into the slot's counters.
//
// bandwidth.ProxyBandwidth carries both, and they answer different questions:
//
//   - BillableRx/BillableTx are the bytes the relay counted on its own packet
//     path (RelayStatsObserver's contract).
//   - ContractUsedIngress/ContractUsedEgress are the bytes the platform's
//     contracts have used, summed from the engine's per-contract accounting
//     (ContractStatsObserver's contract). This is the number the platform
//     settles on, and the reason it is kept beside billable is so drift
//     between counted and settled is measurable instead of invisible.
//
// The legacy provider feeds both from connect callbacks it can subscribe to
// (provider/provide.go: RemoteUserNatProvider.TryAddPacketStatsCallback with
// RelayStatsObserver, and ContractManager().AddContractStatsCallback with
// ContractStatsObserver). The miner does not own that engine: sdk.DeviceLocal
// builds the provider inside the slot, keeps the same accounting, and exposes
// the running totals as cumulative readings — GetProviderPacketStats,
// GetProviderIngressContractStats, GetProviderEgressContractStats. The pinned
// SDK exports no callback seam for them, so the miner samples the readings and
// folds each slot's growth into the same counters, with the same high-water
// rule the two observers use.
//
// That wiring is not cosmetic: before it, nothing in the miner ever added to
// BillableRx/BillableTx or ContractUsed*, so both published series were a
// structural zero while the node relayed traffic. A zero that means "not
// measured" is exactly what /metrics must not report as a measurement.
//
// Direction mapping, taken from the two observers the provider uses so the
// miner's series mean the same thing as the fleet's:
//
//   - relay RemoteIngressByteCount (traffic received from the tunnel) is
//     BillableTx, relay RemoteEgressByteCount is BillableRx — the split
//     RelayStatsObserver applies, inverted on purpose against the field names.
//   - the receive-side contract total is ContractUsedIngress, the send-side one
//     ContractUsedEgress — ContractStatsObserver's `event.Receive` split.

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/urnetwork/sdk"

	"github.com/urfoundation/sn/provider/bandwidth"
)

const (
	// providerBillingSampleInterval is how often a live slot reads its device's
	// billing readings. The provider's callbacks fire about once a second; the
	// readings are cumulative, so a slower sample delays a byte, it never loses
	// one. It matches the transport-health cadence, so one interval covers both
	// per-slot watches.
	providerBillingSampleInterval = 5 * time.Second
)

// slotBillingSource is the slice of *sdk.DeviceLocal the sampler reads. An
// interface so a test can drive it without a live provider.
//
// The contract DETAIL feeds are read for the slot's client count, the stats
// feeds for its settled bytes; both come from the same device under the same
// lock, so one sample sees one provider generation.
type slotBillingSource interface {
	GetProviderPacketStats() *sdk.PacketStats
	GetProviderIngressContractStats() *sdk.ContractStats
	GetProviderEgressContractStats() *sdk.ContractStats
	GetProviderIngressContractDetails() *sdk.ContractDetailsList
	GetProviderEgressContractDetails() *sdk.ContractDetailsList
}

// proxyBillingSampler folds one slot's cumulative device readings into the
// slot's ProxyBandwidth. It keeps the last reading of each figure, so only the
// growth since the last sample is added, and it keeps the set of the slot's
// open provider contracts, so the client count is a live set rather than a
// reading to diff.
type proxyBillingSampler struct {
	bw *bandwidth.ProxyBandwidth

	relayIngress, relayEgress       uint64
	contractIngress, contractEgress uint64
	// contracts is the set of contract ids this sampler has counted as live
	// client sessions. A contract that leaves the feeds is released exactly
	// once, which is what keeps bw.Clients from drifting.
	contracts map[string]struct{}
}

// sample adds this reading's growth to the slot's counters. A device with no
// provider (the SDK returns nil for a device whose provider is disabled) has
// nothing to report, and adds nothing.
func (s *proxyBillingSampler) sample(src slotBillingSource) {
	if s == nil || s.bw == nil || src == nil {
		return
	}
	if stats := src.GetProviderPacketStats(); stats != nil {
		s.fold(&s.relayIngress, nonNegative(stats.RemoteIngressByteCount), &s.bw.BillableTx)
		s.fold(&s.relayEgress, nonNegative(stats.RemoteEgressByteCount), &s.bw.BillableRx)
	}
	if stats := src.GetProviderIngressContractStats(); stats != nil {
		s.fold(&s.contractIngress, nonNegative(stats.ContractUsedByteCount), &s.bw.ContractUsedIngress)
	}
	if stats := src.GetProviderEgressContractStats(); stats != nil {
		s.fold(&s.contractEgress, nonNegative(stats.ContractUsedByteCount), &s.bw.ContractUsedEgress)
	}
	s.sampleClients(src)
}

// sampleClients sets the slot's live client count from the provider contracts
// the platform has open on it, and returns nothing.
//
// This is the miner's client-arrival signal. The provider counts live
// relay-egress connections, because it owns that dial; the miner's only dial
// seam (DeviceLocalSettings.ProviderDialContextSettings) is shared between the
// provider carrier and the exit NAT (sdk device_local.go), so a session wrap
// there would charge the node's own control dials as clients — the very
// misreading this replaces. An OPEN contract is the platform saying "a client
// flow is matched to this slot", and it is reported Closed once when the flow
// ends and then evicted, so the open set is exactly the clients being served.
//
// Both directions are counted. The send and receive contracts of one flow are
// deliberately unpaired in this protocol (sdk device.go, ContractDetails), so a
// flow can hold one contract per direction; the count is therefore contracts,
// not distinct users — the same caveat the provider's dial count carries.
//
// A nil feed means the device has no provider (not providing yet, or shutting
// down): it reports no clients, which is what releases everything the sampler
// previously counted.
func (s *proxyBillingSampler) sampleClients(src slotBillingSource) {
	if s == nil || s.bw == nil {
		return
	}
	open := map[string]struct{}{}
	for _, details := range []*sdk.ContractDetailsList{
		src.GetProviderIngressContractDetails(),
		src.GetProviderEgressContractDetails(),
	} {
		if details == nil {
			continue
		}
		for i := 0; i < details.Len(); i += 1 {
			contract := details.Get(i)
			if contract == nil || contract.ContractId == nil || contract.Status != sdk.ContractStatusOpen {
				continue
			}
			open[contract.ContractId.String()] = struct{}{}
		}
	}

	now := time.Now()
	for id := range open {
		if _, ok := s.contracts[id]; ok {
			continue
		}
		s.bw.Clients.Add(1)
		s.bw.AddSession(id, now)
	}
	for id := range s.contracts {
		if _, ok := open[id]; ok {
			continue
		}
		s.bw.Clients.Add(-1)
		s.bw.RemoveSession(id)
	}
	s.contracts = open
}

// releaseClients ends every session this sampler counted. Called when the slot's
// watch stops, so a closed slot leaves no phantom clients on a counter the next
// slot at that proxy index would inherit (the registry reuses the entry).
func (s *proxyBillingSampler) releaseClients() {
	if s == nil || s.bw == nil {
		return
	}
	for id := range s.contracts {
		s.bw.Clients.Add(-1)
		s.bw.RemoveSession(id)
	}
	s.contracts = nil
}

// fold adds the bytes a cumulative reading grew by since the last one, and
// re-baselines on a reading that went DOWN.
//
// A drop is expected, not an error. The engine reports a closed contract one
// final time and then forgets it, so its bytes leave the summed reading; a
// provider rebuilt inside the slot (an h3 relaunch) starts its accounting at
// zero. Either way the bytes below the mark were already counted, so nothing is
// added — but the mark MUST follow the reading down, or every byte of the new
// baseline would be swallowed until the reading passed the old peak. The cost of
// that re-baseline is bounded by the sample interval: growth that lands in the
// same sample as the drop is not separable from it and is lost with it.
//
// The high-water rule the provider's observers use (`current <= high: add 0`)
// is deliberately not reused for the contract figures: it never lowers the mark,
// so on a churning slot the next contracts' bytes would be undercounted instead
// of bounded by one interval.
func (s *proxyBillingSampler) fold(last *uint64, reading uint64, counter *atomic.Uint64) {
	if reading > *last {
		counter.Add(reading - *last)
	}
	*last = reading
}

// nonNegative clamps an SDK byte count to zero. The pinch is a negative sentinel
// for "no data" on some counters; a negative can never be a delta.
func nonNegative(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// watchProxyBilling samples one slot's billing readings on a cadence and returns
// a stop that cancels the watch and waits for it to exit. A caller that defers
// stop before it defers the device close cannot have the close itself read as a
// drop in the last sample.
//
// It is also the slot's client-count watch: the sampler maintains bw.Clients
// from the open provider contracts (sampleClients), and the stop releases them,
// so a slot that exits counts nothing after it is gone.
func watchProxyBilling(ctx context.Context, bw *bandwidth.ProxyBandwidth, src slotBillingSource, interval time.Duration) (stop func()) {
	if bw == nil || src == nil {
		return func() {}
	}
	if interval <= 0 {
		interval = providerBillingSampleInterval
	}
	sampler := &proxyBillingSampler{bw: bw}
	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			// A slot on its way out is closed while this watch still holds a
			// reference to its device; stop reading before the close, not
			// after it.
			if watchCtx.Err() != nil {
				return
			}
			sampler.sample(src)
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
		sampler.releaseClients()
	}
}
