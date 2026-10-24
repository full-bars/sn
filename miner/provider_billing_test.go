package miner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sdk"

	"github.com/urfoundation/sn/miner/bandwidth"
)

// Tests for provider_billing.go: the sampler that keeps a slot's billable and
// settled counters moving from the SDK's own provider accounting.
//
// The two families are a cross-check, so the tests pin the DIRECTION mapping as
// hard as the arithmetic: if the sampler swapped ingress and egress, both
// counters would still look plausible and the drift comparison would be
// meaningless.

// fakeBillingSource is one slot's device readings, settable from a test.
type fakeBillingSource struct {
	mu      sync.Mutex
	packet  *sdk.PacketStats
	ingress *sdk.ContractStats
	egress  *sdk.ContractStats

	ingressDetails *sdk.ContractDetailsList
	egressDetails  *sdk.ContractDetailsList
}

func (f *fakeBillingSource) GetProviderPacketStats() *sdk.PacketStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.packet
}

func (f *fakeBillingSource) GetProviderIngressContractStats() *sdk.ContractStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ingress
}

func (f *fakeBillingSource) GetProviderEgressContractStats() *sdk.ContractStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.egress
}

func (f *fakeBillingSource) GetProviderIngressContractDetails() *sdk.ContractDetailsList {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ingressDetails
}

func (f *fakeBillingSource) GetProviderEgressContractDetails() *sdk.ContractDetailsList {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.egressDetails
}

// setContracts replaces both contract feeds with the given per-direction lists.
// A nil list is the SDK's "no provider" reading.
func (f *fakeBillingSource) setContracts(ingress, egress *sdk.ContractDetailsList) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ingressDetails = ingress
	f.egressDetails = egress
}

// contractDetails builds one contract feed. Each entry is an open contract
// unless closed is true, in which case it is the one-shot tombstone the SDK
// reports before evicting it.
func contractDetails(ids []*sdk.Id, closed bool) *sdk.ContractDetailsList {
	list := sdk.NewContractDetailsList()
	for _, id := range ids {
		status := sdk.ContractStatusOpen
		if closed {
			status = sdk.ContractStatusClosed
		}
		list.Add(&sdk.ContractDetails{ContractId: id, Status: status})
	}
	return list
}

func (f *fakeBillingSource) set(relayIngress, relayEgress, contractIngress, contractEgress int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.packet = &sdk.PacketStats{
		RemoteIngressByteCount: relayIngress,
		RemoteEgressByteCount:  relayEgress,
	}
	f.ingress = &sdk.ContractStats{ContractUsedByteCount: contractIngress}
	f.egress = &sdk.ContractStats{ContractUsedByteCount: contractEgress}
}

// TestProxyBillingSamplerFoldsGrowthInTheProvidersDirections pins one known
// counter state to exact values, including which SDK reading feeds which
// counter.
func TestProxyBillingSamplerFoldsGrowthInTheProvidersDirections(t *testing.T) {
	bw := &bandwidth.ProxyBandwidth{}
	src := &fakeBillingSource{}
	sampler := &proxyBillingSampler{bw: bw}

	src.set(1_000, 2_000, 900, 1_800)
	sampler.sample(src)
	// RemoteIngressByteCount is traffic received from the tunnel, which
	// RelayStatsObserver charges to BillableTx; RemoteEgressByteCount is
	// BillableRx. The receive-side contract total is ContractUsedIngress.
	if got := bw.BillableTx.Load(); got != 1_000 {
		t.Errorf("billable tx (relay ingress) = %d, want 1000", got)
	}
	if got := bw.BillableRx.Load(); got != 2_000 {
		t.Errorf("billable rx (relay egress) = %d, want 2000", got)
	}
	if got := bw.ContractUsedIngress.Load(); got != 900 {
		t.Errorf("contract ingress = %d, want 900", got)
	}
	if got := bw.ContractUsedEgress.Load(); got != 1_800 {
		t.Errorf("contract egress = %d, want 1800", got)
	}

	// The readings are cumulative: the second sample adds only the growth.
	src.set(1_500, 2_750, 1_400, 2_600)
	sampler.sample(src)
	if got := bw.BillableTx.Load(); got != 1_500 {
		t.Errorf("billable tx after growth = %d, want 1500", got)
	}
	if got := bw.BillableRx.Load(); got != 2_750 {
		t.Errorf("billable rx after growth = %d, want 2750", got)
	}
	if got := bw.ContractUsedIngress.Load(); got != 1_400 {
		t.Errorf("contract ingress after growth = %d, want 1400", got)
	}
	if got := bw.ContractUsedEgress.Load(); got != 2_600 {
		t.Errorf("contract egress after growth = %d, want 2600", got)
	}

	// A repeated reading is not new traffic.
	sampler.sample(src)
	if got := bw.BillableTx.Load(); got != 1_500 {
		t.Errorf("repeated reading counted again: billable tx = %d, want 1500", got)
	}
}

// TestProxyBillingSamplerRebaselinesOnDrop pins the closed-contract and rebuilt
// provider cases: the summed reading goes DOWN (the engine forgets a closed
// contract, a rebuilt provider restarts its accounting), and the counter must
// neither fall nor swallow the next contract's bytes.
func TestProxyBillingSamplerRebaselinesOnDrop(t *testing.T) {
	bw := &bandwidth.ProxyBandwidth{}
	src := &fakeBillingSource{}
	sampler := &proxyBillingSampler{bw: bw}

	src.set(10_000, 20_000, 5_000, 6_000)
	sampler.sample(src)

	// The contract that held 5_000 ingress bytes was evicted after its final
	// event: the summed reading drops to 200. The counter keeps the 5_000.
	src.set(10_000, 20_000, 200, 6_000)
	sampler.sample(src)
	if got := bw.ContractUsedIngress.Load(); got != 5_000 {
		t.Fatalf("a dropped contract reading subtracted from the counter: %d, want 5000", got)
	}

	// The next contract grows to 300. Its 100 new bytes must be counted, which
	// only happens because the sampler followed the reading down.
	src.set(10_000, 20_000, 300, 6_000)
	sampler.sample(src)
	if got := bw.ContractUsedIngress.Load(); got != 5_100 {
		t.Fatalf("growth after a drop was swallowed: %d, want 5100", got)
	}

	// A rebuilt provider resets every reading to zero, then counts up again.
	src.set(40, 80, 10, 20)
	sampler.sample(src)
	if got := bw.BillableTx.Load(); got != 10_000 {
		t.Fatalf("billable tx after a rebuild subtracted bytes: %d, want 10000", got)
	}
	src.set(140, 180, 110, 120)
	sampler.sample(src)
	if got := bw.BillableTx.Load(); got != 10_100 {
		t.Fatalf("billable tx after rebuild growth = %d, want 10100", got)
	}
	if got := bw.ContractUsedEgress.Load(); got != 6_100 {
		t.Fatalf("contract egress after rebuild growth = %d, want 6100", got)
	}
}

// TestProxyBillingSamplerKeepsBaselineAcrossNoDataSentinel pins the SDK's
// negative "no data" sentinel against the re-baseline rule. A negative is not a
// drop: the last real reading is still the mark, so when the cumulative figure
// returns it must not be added to the counter a second time.
func TestProxyBillingSamplerKeepsBaselineAcrossNoDataSentinel(t *testing.T) {
	bw := &bandwidth.ProxyBandwidth{}
	src := &fakeBillingSource{}
	sampler := &proxyBillingSampler{bw: bw}

	src.set(10_000, 20_000, 5_000, 6_000)
	sampler.sample(src)
	if got := bw.BillableTx.Load(); got != 10_000 {
		t.Fatalf("billable tx baseline = %d, want 10000", got)
	}

	// One tick of "no data" on the relay counter. The counter must hold its
	// value, and the other counters must keep folding.
	src.set(-1, 20_500, 5_000, 6_000)
	sampler.sample(src)
	if got := bw.BillableTx.Load(); got != 10_000 {
		t.Errorf("a no-data sentinel changed the counter: %d, want 10000", got)
	}
	if got := bw.BillableRx.Load(); got != 20_500 {
		t.Errorf("the other direction stopped folding: %d, want 20500", got)
	}

	// The real cumulative figure returns at its former level: no new bytes,
	// so nothing may be added.
	src.set(10_000, 20_500, 5_000, 6_000)
	sampler.sample(src)
	if got := bw.BillableTx.Load(); got != 10_000 {
		t.Fatalf("the sentinel zeroed the baseline and recounted the lifetime: %d, want 10000", got)
	}

	// Growth after the sentinel is counted exactly once.
	src.set(10_500, 20_500, 5_000, 6_000)
	sampler.sample(src)
	if got := bw.BillableTx.Load(); got != 10_500 {
		t.Fatalf("growth after the sentinel = %d, want 10500", got)
	}
}

// TestProxyBillingSamplerIgnoresMissingReadings pins that a device with no
// provider (the SDK returns nil readings) adds nothing rather than a zero.
func TestProxyBillingSamplerIgnoresMissingReadings(t *testing.T) {
	bw := &bandwidth.ProxyBandwidth{}
	sampler := &proxyBillingSampler{bw: bw}
	sampler.sample(&fakeBillingSource{})

	if got := bw.BillableRx.Load() + bw.BillableTx.Load(); got != 0 {
		t.Fatalf("missing readings produced %d bytes", got)
	}
	// A nil source, nil counter and a negative reading are all no-ops, never a
	// wrap to an enormous delta.
	(&proxyBillingSampler{}).sample(&fakeBillingSource{})
	sampler.sample(nil)
	src := &fakeBillingSource{}
	src.set(-1, -2, -3, -4)
	sampler.sample(src)
	if got := bw.BillableRx.Load() + bw.BillableTx.Load() + bw.ContractUsedIngress.Load() + bw.ContractUsedEgress.Load(); got != 0 {
		t.Fatalf("negative readings produced %d bytes", got)
	}
}

// TestWatchProxyBillingSamplesUntilStopped pins the watch itself: it samples on
// the cadence, and stop both ends the sampling and waits for the goroutine.
func TestWatchProxyBillingSamplesUntilStopped(t *testing.T) {
	bw := &bandwidth.ProxyBandwidth{}
	src := &fakeBillingSource{}
	src.set(1_024, 2_048, 512, 1_024)

	stop := watchProxyBilling(context.Background(), bw, src, 5*time.Millisecond)
	if stop == nil {
		t.Fatal("watch returned no stop")
	}

	deadline := time.Now().Add(2 * time.Second)
	for bw.BillableTx.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("watch never sampled the device")
		}
		time.Sleep(time.Millisecond)
	}
	stop()

	settled := bw.BillableTx.Load()
	time.Sleep(20 * time.Millisecond)
	if got := bw.BillableTx.Load(); got != settled {
		t.Fatalf("watch kept sampling after stop: %d -> %d", settled, got)
	}

	// A nil device or counter is a watch that never starts, not a panic.
	watchProxyBilling(context.Background(), nil, src, time.Millisecond)()
	watchProxyBilling(context.Background(), bw, nil, time.Millisecond)()
}

// TestProxyBillingSamplerCountsOpenContractsAsClients pins the miner's client
// count: the platform's open provider contracts, not the node's own dials.
//
// This is what makes `clients` (and therefore `serving`, `idle`, `earning` and
// the h3 relauncher's "is this identity busy" test) mean a client and not an
// authentication. Before it, the only session-counting wrap in the miner was
// the network space's control-plane settings (see networkSpaceConnectSettings),
// so a slot counted itself as soon as it authenticated and reported clients=1
// with nobody attached.
func TestProxyBillingSamplerCountsOpenContractsAsClients(t *testing.T) {
	bw := &bandwidth.ProxyBandwidth{}
	src := &fakeBillingSource{}
	sampler := &proxyBillingSampler{bw: bw}

	// No provider: no contracts, no clients.
	sampler.sample(src)
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("clients with no provider = %d, want 0", got)
	}

	ingress := sdk.NewId()
	egress := sdk.NewId()
	src.setContracts(contractDetails([]*sdk.Id{ingress}, false), contractDetails([]*sdk.Id{egress}, false))
	sampler.sample(src)
	if got := bw.Clients.Load(); got != 2 {
		t.Fatalf("clients with one open contract each way = %d, want 2", got)
	}

	// The feeds are re-read every sample: the same open contracts must not be
	// counted twice.
	sampler.sample(src)
	sampler.sample(src)
	if got := bw.Clients.Load(); got != 2 {
		t.Fatalf("clients after repeated samples = %d, want 2 (contracts counted once each)", got)
	}

	// A closed contract is the one-shot tombstone before eviction: it is not a
	// live client even while it is still listed.
	src.setContracts(contractDetails([]*sdk.Id{ingress}, true), contractDetails([]*sdk.Id{egress}, true))
	sampler.sample(src)
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("clients with only closed tombstones = %d, want 0", got)
	}

	// A second flow opens on the ingress side only.
	next := sdk.NewId()
	src.setContracts(contractDetails([]*sdk.Id{next}, false), nil)
	sampler.sample(src)
	if got := bw.Clients.Load(); got != 1 {
		t.Fatalf("clients after a new flow = %d, want 1", got)
	}

	// The slot's watch stops: the counter must be left clean, because the
	// registry hands the same counter to the next slot at this identity index.
	sampler.releaseClients()
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("clients after release = %d, want 0", got)
	}
}

// TestWatchProxyBillingCountsClientsAndReleasesOnStop pins the wiring a slot
// actually gets: the same watch that folds the byte counters maintains the
// client count, and stopping it (a slot going away) releases the clients it
// counted.
func TestWatchProxyBillingCountsClientsAndReleasesOnStop(t *testing.T) {
	bw := &bandwidth.ProxyBandwidth{}
	src := &fakeBillingSource{}
	id := sdk.NewId()
	src.setContracts(contractDetails([]*sdk.Id{id}, false), nil)

	stop := watchProxyBilling(context.Background(), bw, src, 5*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for bw.Clients.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the watch never counted the slot's open contract as a client")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("clients after the slot's watch stopped = %d, want 0", got)
	}
}
