package miner

// The contract seam is the one that costs money: urnet_contracts_total being
// absent from the scrape is a blind spot on the only signal that says a
// provider is still winning work. These tests pin both halves of the contract
// the rest of the seams keep:
//
//   - the series appears, with a real count, once a slot feeds it;
//   - it is absent -- not zero -- when there is no source, so "not measured"
//     can never be read as "no contracts";
//   - refusals stay absent, because the miner has no refusal source.

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sdk"
)

// fakeContractSub is the sdk.Sub the fake device hands back: Close runs the
// device's own unsubscribe so a test can prove retire really detaches.
type fakeContractSub struct {
	once  sync.Once
	close func()
}

func (self *fakeContractSub) Close() {
	self.once.Do(func() {
		if self.close != nil {
			self.close()
		}
	})
}

// fakeContractOutcomeSource is one provider device's contract feeds. It records
// the listeners the way *sdk.DeviceLocal holds them, so a test can deliver a
// contract detail exactly as the SDK would.
type fakeContractOutcomeSource struct {
	mu      sync.Mutex
	egress  sdk.ContractDetailsChangeListener
	ingress sdk.ContractDetailsChangeListener
	closes  int
}

func (self *fakeContractOutcomeSource) AddProviderEgressContractDetailsChangeListener(listener sdk.ContractDetailsChangeListener) sdk.Sub {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.egress = listener
	return &fakeContractSub{close: func() {
		self.mu.Lock()
		defer self.mu.Unlock()
		self.egress = nil
		self.closes += 1
	}}
}

func (self *fakeContractOutcomeSource) AddProviderIngressContractDetailsChangeListener(listener sdk.ContractDetailsChangeListener) sdk.Sub {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.ingress = listener
	return &fakeContractSub{close: func() {
		self.mu.Lock()
		defer self.mu.Unlock()
		self.ingress = nil
		self.closes += 1
	}}
}

// deliver hands one detail to the direction's listener, as
// providerEgressContractDetailsChanged does per contract of a snapshot.
func (self *fakeContractOutcomeSource) deliver(t *testing.T, receive bool, details sdk.ContractDetails) {
	t.Helper()
	self.mu.Lock()
	listener := self.egress
	if receive {
		listener = self.ingress
	}
	self.mu.Unlock()
	if listener == nil {
		t.Fatalf("no listener registered for receive=%v", receive)
	}
	listener.ContractDetailsChanged(&details)
}

// contractDetail builds one contract detail for an id. Ids are platform minted
// and single use, so a test names them itself.
func contractDetail(t *testing.T, static string, status string) sdk.ContractDetails {
	t.Helper()
	id, err := sdk.ParseId(static)
	if err != nil {
		t.Fatalf("ParseId(%q): %v", static, err)
	}
	return sdk.ContractDetails{ContractId: id, Status: status}
}

// withFreshContractOutcomes gives a test its own process total, so a
// registration in one test cannot leak a source into another.
func withFreshContractOutcomes(t *testing.T) *contractOutcomeTotals {
	t.Helper()
	previous := minerContractOutcomes
	totals := &contractOutcomeTotals{}
	minerContractOutcomes = totals
	t.Cleanup(func() { minerContractOutcomes = previous })
	return totals
}

// hasContractSeries reports whether the scrape carries urnet_contracts_total.
func hasContractSeries(body string) bool {
	return strings.Contains(body, "urnet_contracts_total{")
}

// With no slot ever registering a contract feed the family must be missing
// entirely: there is no data source, and a published zero would be a
// fabricated reading rather than a measurement.
func TestContractSeriesAbsentWithoutASource(t *testing.T) {
	withFreshContractOutcomes(t)
	if corpus := minerExtraMetrics(); hasContractSeries(corpus) {
		t.Fatalf("urnet_contracts_total appeared with no source:\n%s", corpus)
	}
	if contracts := metricsSeamContracts(); contracts != nil {
		t.Fatalf("the seam reported %+v with no source, want nil", contracts)
	}
	if strings.Contains(minerExtraMetrics(), "# TYPE urnet_contracts_total") {
		t.Fatal("the family's TYPE line appeared with no source")
	}
}

// A registered slot with no contract yet is a real zero: the source exists, it
// has simply counted nothing. That must reach the scrape as 0, not as absence.
func TestContractSeriesIsZeroOnceASlotRegisters(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	contracts := metricsSeamContracts()
	if contracts == nil {
		t.Fatal("the seam is nil after a slot registered")
	}
	if contracts.Acquired != 0 {
		t.Fatalf("acquired = %d, want 0 before any contract", contracts.Acquired)
	}
	corpus := minerExtraMetrics()
	for _, want := range []string{
		"# TYPE urnet_contracts_total counter",
		`urnet_contracts_total{result="acquired"} 0`,
	} {
		if !strings.Contains(corpus, want) {
			t.Errorf("scrape missing %q", want)
		}
	}
}

// The count is contracts, not detail deliveries: the SDK re-delivers an open
// contract on every stats epoch, and the same contract must count once.
func TestContractOutcomesCountEachContractOnce(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	first := contractDetail(t, "11111111-1111-4111-8111-111111111111", sdk.ContractStatusOpen)
	device.deliver(t, false, first)
	// the same open contract, re-delivered by the next stats epoch
	device.deliver(t, false, first)
	device.deliver(t, false, first)
	if acquired := metricsSeamContracts().Acquired; acquired != 1 {
		t.Fatalf("acquired = %d after one contract delivered three times, want 1", acquired)
	}

	// a second, distinct contract, on the other direction
	device.deliver(t, true, contractDetail(t, "22222222-2222-4222-8222-222222222222", sdk.ContractStatusOpen))
	if acquired := metricsSeamContracts().Acquired; acquired != 2 {
		t.Fatalf("acquired = %d after two contracts, want 2", acquired)
	}

	// the close tombstone is a final report of an already counted contract
	closed := first
	closed.Status = sdk.ContractStatusClosed
	device.deliver(t, false, closed)
	if acquired := metricsSeamContracts().Acquired; acquired != 2 {
		t.Fatalf("acquired = %d after a close tombstone, want 2", acquired)
	}

	if corpus := minerExtraMetrics(); !strings.Contains(corpus, `urnet_contracts_total{result="acquired"} 2`) {
		t.Fatalf("scrape does not carry the real count:\n%s", corpus)
	}
}

// A contract whose whole lifecycle the SDK's stats gate folded into one batch
// arrives as its close tombstone alone. It was still a contract this provider
// acquired, so it counts -- and only once.
func TestContractOutcomesCountAContractSeenOnlyClosed(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	device.deliver(t, false, contractDetail(t, "33333333-3333-4333-8333-333333333333", sdk.ContractStatusClosed))
	if acquired := metricsSeamContracts().Acquired; acquired != 1 {
		t.Fatalf("acquired = %d for a tombstone-only contract, want 1", acquired)
	}
}

// A retired slot stops feeding the total, and with no slot left the family goes
// back to absent rather than reporting the last value it held.
func TestContractOutcomesRetireStopsTheSource(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	retire := registerContractOutcomes(device)
	device.deliver(t, false, contractDetail(t, "44444444-4444-4444-8444-444444444444", sdk.ContractStatusOpen))
	if acquired := metricsSeamContracts().Acquired; acquired != 1 {
		t.Fatalf("acquired = %d, want 1", acquired)
	}

	retire()
	// idempotent: a second retire must not unbalance the source count
	retire()
	if contracts := metricsSeamContracts(); contracts != nil {
		t.Fatalf("the seam still reports %+v after every slot retired", contracts)
	}
	if corpus := minerExtraMetrics(); hasContractSeries(corpus) {
		t.Fatalf("urnet_contracts_total outlived its source:\n%s", corpus)
	}
	device.mu.Lock()
	closes := device.closes
	device.mu.Unlock()
	if closes != 2 {
		t.Fatalf("retire closed %d subscriptions, want 2 (egress and ingress)", closes)
	}
}

// Refusals have no miner-side source: a refused contract never opens, so it
// never reaches the provider contract feed. The denied label must stay absent
// rather than carry a zero that reads as "nothing was refused".
func TestContractDeniedLabelStaysAbsent(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	device.deliver(t, false, contractDetail(t, "55555555-5555-4555-8555-555555555555", sdk.ContractStatusOpen))
	contracts := metricsSeamContracts()
	if contracts == nil {
		t.Fatal("the seam is nil while a slot is registered")
	}
	if contracts.DeniedKnown {
		t.Fatal("the miner claimed a refusal source it does not have")
	}
	corpus := minerExtraMetrics()
	if strings.Contains(corpus, `urnet_contracts_total{result="denied"}`) {
		t.Fatalf("the denied label was published without a source:\n%s", corpus)
	}
	if !strings.Contains(corpus, `urnet_contracts_total{result="acquired"} 1`) {
		t.Fatalf("the acquired label is missing its count:\n%s", corpus)
	}
}

// The lifetime collector folds the process counter into the all-time store, and
// a seam with no refusal source must not touch the persisted denied total.
func TestLifetimeCollectorSkipsUnknownDenials(t *testing.T) {
	withTempHome(t)
	store := withLifetimeStore(t)
	resetLifetimeFoldState(t)
	resetProxyEarnBandwidthForTest()
	t.Cleanup(resetProxyEarnBandwidthForTest)

	acquired := int64(4)
	withLifetimeSeams(t,
		func() *seamSessions { return nil },
		func() *seamContracts { return &seamContracts{Acquired: acquired} },
	)

	now := time.Now()
	newLifetimeCollector().tick(now)
	if _, _, up, deny, _, _, _ := store.Snapshot(); up != 4 || deny != 0 {
		t.Fatalf("first fold = acquired %d denied %d, want 4 and 0", up, deny)
	}

	acquired = 7
	newLifetimeCollector().tick(now.Add(time.Minute))
	if _, _, up, deny, _, _, _ := store.Snapshot(); up != 7 || deny != 0 {
		t.Fatalf("second fold = acquired %d denied %d, want 7 and 0", up, deny)
	}
}
