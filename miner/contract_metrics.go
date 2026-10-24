package miner

// contract_metrics.go: urnet_contracts_total for the miner.
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
// contract. This file counts those, process-wide, and publishes them as
// urnet_contracts_total{result="acquired"}.
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

// contractOutcomeTotals is the process-wide total: contracts acquired since
// this process started, summed over every provider slot. sources counts the
// live registrations, which is what lets the scrape tell "no contract acquired
// yet" (a real 0) from "this process is not providing, so it has nothing to
// report" (absent).
type contractOutcomeTotals struct {
	mu       sync.Mutex
	acquired int64
	sources  int
}

var minerContractOutcomes = &contractOutcomeTotals{}

func (self *contractOutcomeTotals) add(acquired int64) {
	if acquired == 0 {
		return
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	self.acquired += acquired
}

func (self *contractOutcomeTotals) register() {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.sources += 1
}

func (self *contractOutcomeTotals) retire() {
	self.mu.Lock()
	defer self.mu.Unlock()
	if 0 < self.sources {
		self.sources -= 1
	}
}

// snapshot returns the process total and whether any slot is feeding it.
func (self *contractOutcomeTotals) snapshot() (acquired int64, ok bool) {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.acquired, 0 < self.sources
}

// contractOutcomeDirection counts the contracts one direction's provider feed
// reports. open holds the ids counted but not yet reported closed: a contract
// is counted once, on its first detail, and dropped when its close tombstone
// arrives. That keeps the set bounded by the contracts open right now instead
// of by every contract the process ever held -- contract ids are platform
// minted and single use, so a set that only ever grew would leak for the life
// of the process.
type contractOutcomeDirection struct {
	totals *contractOutcomeTotals
	mu     sync.Mutex
	open   map[string]struct{}
}

// ContractDetailsChanged counts one contract detail delivery
// (sdk.ContractDetailsChangeListener). The same contract is delivered again on
// every stats epoch while it is open, so this gate is what makes the result a
// count of contracts rather than a count of samples.
func (self *contractOutcomeDirection) ContractDetailsChanged(details *sdk.ContractDetails) {
	if details == nil || details.ContractId == nil {
		return
	}
	id := details.ContractId.String()
	self.mu.Lock()
	defer self.mu.Unlock()
	if _, ok := self.open[id]; ok {
		if details.Status == sdk.ContractStatusClosed {
			delete(self.open, id)
		}
		return
	}
	// Not counted yet. Either this is the contract's first open report, or the
	// SDK's stats gate folded the whole lifecycle into one batch and only the
	// close tombstone is delivered. A tombstone is delivered exactly once and
	// the tracker evicts the contract after it, so counting it here cannot
	// double count, and it is not remembered either.
	self.totals.add(1)
	if details.Status != sdk.ContractStatusClosed {
		if self.open == nil {
			self.open = map[string]struct{}{}
		}
		self.open[id] = struct{}{}
	}
}

// registerContractOutcomes attaches the process contract counter to one slot's
// provider contract feeds and returns the teardown. Call it after the device
// close has been deferred so the listeners stop while the device is still live,
// and defer the teardown from the same goroutine that registered.
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
	egress := &contractOutcomeDirection{totals: minerContractOutcomes}
	ingress := &contractOutcomeDirection{totals: minerContractOutcomes}
	minerContractOutcomes.register()
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
			minerContractOutcomes.retire()
		})
	}
}

func init() {
	metricsSeamContracts = func() *seamContracts {
		acquired, ok := minerContractOutcomes.snapshot()
		if !ok {
			// No slot has ever registered a provider contract feed: there is
			// no data source, which is not the same reading as zero contracts.
			return nil
		}
		return &seamContracts{Acquired: acquired}
	}
}
