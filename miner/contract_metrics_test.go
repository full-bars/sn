package miner

// The contract seam is the one that costs money: urnet_contracts_total being
// absent from the scrape is a blind spot on the only signal that says a
// provider is still winning work. These tests pin the contract the rest of the
// seams keep:
//
//   - the series appears, with a real count, once a slot feeds it;
//   - it is absent -- not zero -- when there is no source, so "not measured"
//     can never be read as "no contracts";
//   - it stays present after the last slot retires, because the count is
//     process-lifetime and must not flap away and back;
//   - the open book (count, purchased bytes, used bytes, utilization) follows
//     the open set, and disappears entirely when nothing is open;
//   - no denied series exists anywhere, because the miner has no denial source.

import (
	"math"
	"reflect"
	"strconv"
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

// listener hands back the currently registered listener for one feed, so a
// test can keep it across retire and deliver the way the SDK's in-flight
// dispatch does (over a snapshot of the registry, after Close has returned).
func (self *fakeContractOutcomeSource) listener(receive bool) sdk.ContractDetailsChangeListener {
	self.mu.Lock()
	defer self.mu.Unlock()
	if receive {
		return self.ingress
	}
	return self.egress
}

// deliver hands one detail to the direction's listener, as
// providerEgressContractDetailsChanged does per contract of a snapshot.
func (self *fakeContractOutcomeSource) deliver(t *testing.T, receive bool, details sdk.ContractDetails) {
	t.Helper()
	listener := self.listener(receive)
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

// contractOpenDetail builds one open-contract detail with the byte accounting
// the SDK's stats callback carries.
func contractOpenDetail(t *testing.T, static string, limitBytes, usedBytes int64) sdk.ContractDetails {
	t.Helper()
	details := contractDetail(t, static, sdk.ContractStatusOpen)
	details.ContractByteCount = limitBytes
	details.ContractUsedByteCount = usedBytes
	return details
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

// contractGauge reads one unlabeled gauge out of the scrape.
func contractGauge(t *testing.T, body string, name string) (float64, bool) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(line, name+" ")
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			t.Fatalf("%s = %q: %v", name, rest, err)
		}
		return value, true
	}
	return 0, false
}

// hasContractFamily reports whether any part of a family (HELP, TYPE or a
// sample) is in the scrape.
func hasContractFamily(body string, name string) bool {
	if strings.Contains(body, "# TYPE "+name+" ") {
		return true
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+" ") {
			return true
		}
	}
	return false
}

// wantContractStats asserts a whole reading, with a small tolerance on the mean.
func wantContractStats(t *testing.T, label string, got contractStats, want contractStats) {
	t.Helper()
	if got.Acquired != want.Acquired {
		t.Errorf("%s: acquired = %d, want %d", label, got.Acquired, want.Acquired)
	}
	if got.Open != want.Open {
		t.Errorf("%s: open = %d, want %d", label, got.Open, want.Open)
	}
	if got.OpenLimitBytes != want.OpenLimitBytes {
		t.Errorf("%s: open limit bytes = %d, want %d", label, got.OpenLimitBytes, want.OpenLimitBytes)
	}
	if got.OpenUsedBytes != want.OpenUsedBytes {
		t.Errorf("%s: open used bytes = %d, want %d", label, got.OpenUsedBytes, want.OpenUsedBytes)
	}
	switch {
	case math.IsNaN(want.AvgUtilPct):
		if !math.IsNaN(got.AvgUtilPct) {
			t.Errorf("%s: avg util = %v, want NaN", label, got.AvgUtilPct)
		}
	case math.Abs(got.AvgUtilPct-want.AvgUtilPct) > 1e-9:
		t.Errorf("%s: avg util = %v%%, want %v%%", label, got.AvgUtilPct, want.AvgUtilPct)
	}
}

// wantContractGauge asserts one gauge sample equals want.
func wantContractGauge(t *testing.T, body string, name string, want float64) {
	t.Helper()
	got, ok := contractGauge(t, body, name)
	if !ok {
		t.Errorf("scrape is missing %s", name)
		return
	}
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
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
	if _, ok := contractStatsSnapshot(); ok {
		t.Fatal("the snapshot claimed a reading with no source")
	}
}

// A registered slot with no contract yet is a real zero: the source exists, it
// has simply counted nothing. That must reach the scrape as 0, not as absence.
// The open book has no current reading, so it stays absent.
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
	for _, name := range []string{
		"urnet_contracts_open",
		"urnet_contract_limit_bytes",
		"urnet_contract_used_bytes",
		"urnet_contract_utilization",
	} {
		if hasContractFamily(corpus, name) {
			t.Errorf("%s appeared with no open contract:\n%s", name, corpus)
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

// One platform grant can be live on both provider feeds: the pinned SDK keys
// the send and receive entries of a contract by one id, shares one sequence
// counter across them, and evicts only once no sibling entry remains. It is one
// contract, so it must be counted once and its purchased bytes added once --
// and a tombstone on one feed must not close it while the other still holds it.
func TestContractSameGrantOnBothFeedsCountsOnce(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	id := "6a6a6a6a-6a6a-4a6a-8a6a-6a6a6a6a6a6a"
	// The send side reports 250 used, the receive side 600: one grant, one
	// reading, and the larger used figure wins.
	device.deliver(t, false, contractOpenDetail(t, id, 1000, 250))
	device.deliver(t, true, contractOpenDetail(t, id, 1000, 600))

	stats, ok := contractStatsSnapshot()
	if !ok {
		t.Fatal("the snapshot lost its reading with a contract open on both feeds")
	}
	wantContractStats(t, "both feeds", stats, contractStats{
		Acquired:       1,
		Open:           1,
		OpenLimitBytes: 1000,
		OpenUsedBytes:  600,
		AvgUtilPct:     60,
	})
	corpus := minerExtraMetrics()
	if !strings.Contains(corpus, `urnet_contracts_total{result="acquired"} 1`) {
		t.Fatalf("one grant on both feeds was counted twice:\n%s", corpus)
	}
	wantContractGauge(t, corpus, "urnet_contracts_open", 1)
	wantContractGauge(t, corpus, "urnet_contract_limit_bytes", 1000)
	wantContractGauge(t, corpus, "urnet_contract_used_bytes", 600)

	// A tombstone on one feed is that feed's final report; the grant is still
	// open on the other, so it stays in the open book and is not recounted.
	device.deliver(t, false, contractDetail(t, id, sdk.ContractStatusClosed))
	stats, ok = contractStatsSnapshot()
	if !ok || stats.Acquired != 1 || stats.Open != 1 || stats.OpenLimitBytes != 1000 {
		t.Fatalf("one feed's tombstone closed or recounted the grant: %+v (ok=%v)", stats, ok)
	}

	// The second tombstone closes it, once.
	device.deliver(t, true, contractDetail(t, id, sdk.ContractStatusClosed))
	stats, ok = contractStatsSnapshot()
	if !ok {
		t.Fatal("the snapshot lost its reading while the source is live")
	}
	wantContractStats(t, "both feeds closed", stats, contractStats{
		Acquired:   1,
		AvgUtilPct: math.NaN(),
	})
	if hasContractFamily(minerExtraMetrics(), "urnet_contracts_open") {
		t.Fatalf("the closed grant stayed in the open book:\n%s", minerExtraMetrics())
	}
}

// A contract whose whole lifecycle the SDK's stats gate folded into one batch
// arrives as its close tombstone alone. It was still a contract this provider
// acquired, so it counts -- and only once -- but it is never part of the open
// book.
func TestContractOutcomesCountAContractSeenOnlyClosed(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	tombstone := contractDetail(t, "33333333-3333-4333-8333-333333333333", sdk.ContractStatusClosed)
	tombstone.ContractByteCount = 5000
	tombstone.ContractUsedByteCount = 4000
	device.deliver(t, false, tombstone)

	stats, ok := contractStatsSnapshot()
	if stats.Acquired != 1 {
		t.Fatalf("acquired = %d for a tombstone-only contract, want 1", stats.Acquired)
	}
	if !ok {
		t.Fatal("the snapshot lost its reading while the source is live")
	}
	if stats.Open != 0 || stats.OpenLimitBytes != 0 || stats.OpenUsedBytes != 0 {
		t.Fatalf("a closed contract entered the open book: %+v", stats)
	}
	if !math.IsNaN(stats.AvgUtilPct) {
		t.Fatalf("avg util = %v with nothing open, want NaN", stats.AvgUtilPct)
	}
	if corpus := minerExtraMetrics(); hasContractFamily(corpus, "urnet_contracts_open") {
		t.Fatalf("a tombstone-only contract produced an open reading:\n%s", corpus)
	}
}

// A retired slot stops feeding the open book, and the contracts it had open
// leave with it. The cumulative counter does not: it is process-lifetime, so
// the series stays present with the last count rather than flapping away and
// back when the next slot registers.
func TestContractOutcomesRetireDropsTheOpenBookOnly(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	retire := registerContractOutcomes(device)
	device.deliver(t, false, contractOpenDetail(t, "44444444-4444-4444-8444-444444444444", 1000, 500))
	if acquired := metricsSeamContracts().Acquired; acquired != 1 {
		t.Fatalf("acquired = %d, want 1", acquired)
	}
	if stats, ok := contractStatsSnapshot(); !ok || stats.Open != 1 {
		t.Fatalf("open = %d ok = %v before retire, want 1 and true", stats.Open, ok)
	}

	retire()
	// idempotent: a second retire must not double-count anything
	retire()

	// The counter is process-lifetime, so it stays present after the last slot
	// retires. Only "never registered" is absent (TestContractSeriesAbsent-
	// WithoutASource).
	contracts := metricsSeamContracts()
	if contracts == nil {
		t.Fatal("the seam went absent after the last slot retired, so the cumulative counter would flap")
	}
	if contracts.Acquired != 1 {
		t.Fatalf("acquired = %d after retire, want 1", contracts.Acquired)
	}
	if contracts.Open != 0 {
		t.Fatalf("open = %d after retire, want 0", contracts.Open)
	}
	corpus := minerExtraMetrics()
	if !strings.Contains(corpus, `urnet_contracts_total{result="acquired"} 1`) {
		t.Fatalf("the cumulative count outlived nothing, it is process-lifetime:\n%s", corpus)
	}
	if hasContractFamily(corpus, "urnet_contracts_open") {
		t.Fatalf("the open book outlived its source:\n%s", corpus)
	}
	stats, ok := contractStatsSnapshot()
	if !ok {
		t.Fatal("the snapshot claims no reading although a slot has registered")
	}
	if stats.Open != 0 || !math.IsNaN(stats.AvgUtilPct) {
		t.Fatalf("the snapshot kept a stale open book: %+v", stats)
	}
	device.mu.Lock()
	closes := device.closes
	device.mu.Unlock()
	if closes != 2 {
		t.Fatalf("retire closed %d subscriptions, want 2 (egress and ingress)", closes)
	}
}

// Closing a subscription is not enough to stop a slot: the SDK dispatches over
// a snapshot of its listener registry, so a callback already in flight lands
// after Close. A retired slot's id is never reused, so a revived contract would
// never be swept again -- the callback has to be dropped at the accounting.
func TestContractRetiredSlotDropsALateCallback(t *testing.T) {
	withFreshContractOutcomes(t)
	devices := []*fakeContractOutcomeSource{{}, {}}
	retires := []func(){}
	for _, device := range devices {
		retires = append(retires, registerContractOutcomes(device))
	}
	defer retires[1]()

	// The first slot acquires a contract, then retires while the test still
	// holds the egress listener the SDK dispatched over.
	devices[0].deliver(t, false, contractOpenDetail(t, "7a7a7a7a-7a7a-4a7a-8a7a-7a7a7a7a7a7a", 1000, 250))
	inFlight := devices[0].listener(false)
	if inFlight == nil {
		t.Fatal("no egress listener to keep across retire")
	}
	retires[0]()
	inFlight.ContractDetailsChanged(&sdk.ContractDetails{
		ContractId:            contractDetail(t, "7b7b7b7b-7b7b-4b7b-8b7b-7b7b7b7b7b7b", sdk.ContractStatusOpen).ContractId,
		Status:                sdk.ContractStatusOpen,
		ContractByteCount:     9000,
		ContractUsedByteCount: 1,
	})

	// The second slot is still live, so the family is published: the ghost
	// would be visible here as open=1 limit=9000.
	stats, ok := contractStatsSnapshot()
	if !ok {
		t.Fatal("the live slot lost its reading")
	}
	if stats.Open != 0 || stats.OpenLimitBytes != 0 || stats.Acquired != 1 {
		t.Fatalf("a retired slot's in-flight callback re-entered the accounting: %+v", stats)
	}
	corpus := minerExtraMetrics()
	if !strings.Contains(corpus, `urnet_contracts_total{result="acquired"} 1`) {
		t.Fatalf("the late callback was counted as an acquisition:\n%s", corpus)
	}
	if hasContractFamily(corpus, "urnet_contracts_open") {
		t.Fatalf("a retired slot's contract is still in the open book:\n%s", corpus)
	}
}

// A close is remembered, so a repeated tombstone -- a duplicate delivery, the
// second feed's, or one that arrives after the SDK evicted the contract and
// with it its sequence guard -- cannot count the contract again.
func TestContractRepeatedCloseDoesNotRecount(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	id := "8c8c8c8c-8c8c-4c8c-8c8c-8c8c8c8c8c8c"
	device.deliver(t, false, contractOpenDetail(t, id, 1000, 500))
	device.deliver(t, false, contractDetail(t, id, sdk.ContractStatusClosed))
	if acquired := metricsSeamContracts().Acquired; acquired != 1 {
		t.Fatalf("acquired = %d after one close, want 1", acquired)
	}

	// replay the tombstone, then a stale open report and its close
	device.deliver(t, false, contractDetail(t, id, sdk.ContractStatusClosed))
	device.deliver(t, true, contractDetail(t, id, sdk.ContractStatusClosed))
	device.deliver(t, false, contractOpenDetail(t, id, 1000, 900))

	stats, ok := contractStatsSnapshot()
	if !ok {
		t.Fatal("the snapshot lost its reading while the source is live")
	}
	if stats.Acquired != 1 {
		t.Fatalf("acquired = %d after a repeated close, want 1", stats.Acquired)
	}
	if stats.Open != 0 || stats.OpenLimitBytes != 0 {
		t.Fatalf("a closed contract reopened: %+v", stats)
	}
	if corpus := minerExtraMetrics(); !strings.Contains(corpus, `urnet_contracts_total{result="acquired"} 1`) {
		t.Fatalf("the replayed tombstone was counted again:\n%s", corpus)
	}
}

// The closed-contract memory is bounded: contract ids are single use, so it only
// has to outlive the SDK's own reporting, and it must not grow for the life of
// the process.
func TestContractClosedMemoryIsBounded(t *testing.T) {
	var closed contractClosedSet
	for i := 0; i < contractClosedMemory+10; i++ {
		closed.add(contractKey{slot: 1, id: strconv.Itoa(i)})
	}
	if len(closed.index) != contractClosedMemory {
		t.Fatalf("closed memory holds %d ids, want %d", len(closed.index), contractClosedMemory)
	}
	if len(closed.ring) != contractClosedMemory {
		t.Fatalf("closed ring holds %d ids, want %d", len(closed.ring), contractClosedMemory)
	}
	if closed.has(contractKey{slot: 1, id: "0"}) {
		t.Fatal("the oldest closed id outlived the bound")
	}
	if !closed.has(contractKey{slot: 1, id: strconv.Itoa(contractClosedMemory + 9)}) {
		t.Fatal("the newest closed id was not remembered")
	}

	// A retired slot forgets everything it closed.
	closed.add(contractKey{slot: 2, id: "other"})
	closed.forget(1)
	if len(closed.index) != 1 || !closed.has(contractKey{slot: 2, id: "other"}) {
		t.Fatalf("forget left %d ids: %+v", len(closed.index), closed.index)
	}
}

// TestContractClosedSetForgetKeepsEvictionOrder pins the ring's order across a
// slot retirement. forget used to rebuild the ring by iterating the index map,
// which left the survivors in random order, so the next eviction took an
// arbitrary id instead of the oldest one.
func TestContractClosedSetForgetKeepsEvictionOrder(t *testing.T) {
	var closed contractClosedSet
	for i := 0; i < contractClosedMemory; i += 1 {
		key := contractKey{slot: 1, id: strconv.Itoa(i)}
		if i%2 == 1 {
			key.slot = 2
		}
		closed.add(key)
	}
	closed.forget(2)

	want := make([]contractKey, 0, contractClosedMemory/2)
	for i := 0; i < contractClosedMemory; i += 2 {
		want = append(want, contractKey{slot: 1, id: strconv.Itoa(i)})
	}
	if !reflect.DeepEqual(closed.ring, want) {
		first := 0
		for first < len(want) && first < len(closed.ring) && closed.ring[first] == want[first] {
			first += 1
		}
		got := closed.ring[first:]
		if len(got) > 4 {
			got = got[:4]
		}
		expect := want[first:]
		if len(expect) > 4 {
			expect = expect[:4]
		}
		t.Fatalf("forget reordered the ring at %d of %d: got %+v, want %+v", first, len(want), got, expect)
	}

	// Refill the ring and add one past capacity: the eviction must take the
	// oldest surviving id, not one that forget shuffled to the front.
	for i := 0; i < contractClosedMemory/2; i += 1 {
		closed.add(contractKey{slot: 3, id: "fill-" + strconv.Itoa(i)})
	}
	closed.add(contractKey{slot: 3, id: "after"})
	if closed.has(contractKey{slot: 1, id: "0"}) {
		t.Fatal("eviction took a later id: the oldest surviving id outlived the bound")
	}
	if !closed.has(contractKey{slot: 1, id: "2"}) {
		t.Fatal("eviction took more than the oldest surviving id")
	}
}

// The open book is the contracts open right now: the count, the bytes purchased
// across them and the bytes used so far, with utilization as the mean of
// used/purchased per contract.
func TestContractOpenGaugesFromFeed(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	// 250/1000 on egress, 1500/3000 on ingress: mean utilization 37.5%.
	device.deliver(t, false, contractOpenDetail(t, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", 1000, 250))
	device.deliver(t, true, contractOpenDetail(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", 3000, 1500))

	stats, ok := contractStatsSnapshot()
	if !ok {
		t.Fatal("the snapshot has no reading with two open contracts")
	}
	wantContractStats(t, "two open", stats, contractStats{
		Acquired:       2,
		Open:           2,
		OpenLimitBytes: 4000,
		OpenUsedBytes:  1750,
		AvgUtilPct:     37.5,
	})

	corpus := minerExtraMetrics()
	wantContractGauge(t, corpus, "urnet_contracts_open", 2)
	wantContractGauge(t, corpus, "urnet_contract_limit_bytes", 4000)
	wantContractGauge(t, corpus, "urnet_contract_used_bytes", 1750)
	wantContractGauge(t, corpus, "urnet_contract_utilization", 37.5)
	for _, want := range []string{
		"# TYPE urnet_contracts_open gauge",
		"# TYPE urnet_contract_limit_bytes gauge",
		"# TYPE urnet_contract_used_bytes gauge",
		"# TYPE urnet_contract_utilization gauge",
	} {
		if !strings.Contains(corpus, want) {
			t.Errorf("scrape missing %q", want)
		}
	}
	// The cumulative count is not the open count, and the profit line's
	// contracts= field reads this one.
	if !strings.Contains(corpus, `urnet_contracts_total{result="acquired"} 2`) {
		t.Fatalf("scrape lost the cumulative contract count:\n%s", corpus)
	}
}

// An open contract is re-reported every stats epoch with a running used-byte
// count. The bytes move; the contract is still one contract, and it is still
// one open contract.
func TestContractOpenBookRefreshesWithoutRecounting(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	first := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	device.deliver(t, false, contractOpenDetail(t, first, 1000, 250))
	device.deliver(t, true, contractOpenDetail(t, "dddddddd-dddd-4ddd-8ddd-dddddddddddd", 3000, 1500))
	// the epoch advances and the first contract reports more usage
	device.deliver(t, false, contractOpenDetail(t, first, 1000, 750))

	stats, _ := contractStatsSnapshot()
	wantContractStats(t, "refreshed", stats, contractStats{
		Acquired:       2,
		Open:           2,
		OpenLimitBytes: 4000,
		OpenUsedBytes:  2250,
		AvgUtilPct:     62.5,
	})
	corpus := minerExtraMetrics()
	wantContractGauge(t, corpus, "urnet_contracts_open", 2)
	wantContractGauge(t, corpus, "urnet_contract_used_bytes", 2250)
	wantContractGauge(t, corpus, "urnet_contract_utilization", 62.5)
}

// The close tombstone is the contract's last report: it leaves the open set
// before the next reading, so neither its count nor its final bytes are
// carried into the gauges. Its cumulative-acquired contribution stays.
func TestContractTombstoneLeavesTheOpenBook(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	closing := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	device.deliver(t, false, contractOpenDetail(t, closing, 1000, 250))
	device.deliver(t, false, contractOpenDetail(t, "ffffffff-ffff-4fff-8fff-ffffffffffff", 3000, 1500))
	if stats, _ := contractStatsSnapshot(); stats.Open != 2 {
		t.Fatalf("open = %d before the close, want 2", stats.Open)
	}

	// The tombstone carries the contract's final byte counts. Counting them
	// would leave limit at 4000 and used at 2500.
	tombstone := contractDetail(t, closing, sdk.ContractStatusClosed)
	tombstone.ContractByteCount = 1000
	tombstone.ContractUsedByteCount = 1000
	device.deliver(t, false, tombstone)

	stats, ok := contractStatsSnapshot()
	if !ok {
		t.Fatal("the snapshot lost its reading while one contract is still open")
	}
	wantContractStats(t, "after close", stats, contractStats{
		Acquired:       2,
		Open:           1,
		OpenLimitBytes: 3000,
		OpenUsedBytes:  1500,
		AvgUtilPct:     50,
	})
	corpus := minerExtraMetrics()
	wantContractGauge(t, corpus, "urnet_contracts_open", 1)
	wantContractGauge(t, corpus, "urnet_contract_limit_bytes", 3000)
	wantContractGauge(t, corpus, "urnet_contract_used_bytes", 1500)
	wantContractGauge(t, corpus, "urnet_contract_utilization", 50)
}

// A contract with no purchased bytes has no utilization. It must be left out of
// the mean rather than counted as 0% used, and with nothing sized the mean is
// undefined: the gauge is absent, not zero.
func TestContractUtilizationSkipsUnsizedContracts(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	device.deliver(t, false, contractOpenDetail(t, "12345678-1234-4234-8234-1234567890ab", 0, 50))
	stats, ok := contractStatsSnapshot()
	if !ok {
		t.Fatal("an unsized but open contract is still an open contract")
	}
	wantContractStats(t, "unsized only", stats, contractStats{
		Acquired:       1,
		Open:           1,
		OpenLimitBytes: 0,
		OpenUsedBytes:  50,
		AvgUtilPct:     math.NaN(),
	})
	corpus := minerExtraMetrics()
	wantContractGauge(t, corpus, "urnet_contracts_open", 1)
	wantContractGauge(t, corpus, "urnet_contract_limit_bytes", 0)
	wantContractGauge(t, corpus, "urnet_contract_used_bytes", 50)
	if hasContractFamily(corpus, "urnet_contract_utilization") {
		t.Fatalf("utilization was published with no sized contract:\n%s", corpus)
	}

	// One sized contract at 50%: the unsized contract is skipped, not averaged
	// in as 0%, so the mean is 50% and not 25%.
	device.deliver(t, true, contractOpenDetail(t, "87654321-4321-4321-8321-ba0987654321", 1000, 500))
	stats, _ = contractStatsSnapshot()
	wantContractStats(t, "one sized", stats, contractStats{
		Acquired:       2,
		Open:           2,
		OpenLimitBytes: 1000,
		OpenUsedBytes:  550,
		AvgUtilPct:     50,
	})
	corpus = minerExtraMetrics()
	wantContractGauge(t, corpus, "urnet_contract_utilization", 50)
}

// Closing the last open contract removes the whole open-book family. "Nothing
// is open" is not a measurement this process can make while a contract may
// still be granted, so it must not read as zero -- but the cumulative count
// stays, because that number is real.
func TestContractOpenFamilyAbsentWithNoOpenContracts(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	id := "99999999-9999-4999-8999-999999999999"
	device.deliver(t, false, contractOpenDetail(t, id, 1000, 500))
	if stats, ok := contractStatsSnapshot(); !ok || stats.Open != 1 {
		t.Fatalf("open = %d ok = %v, want 1 and true", stats.Open, ok)
	}

	device.deliver(t, false, contractDetail(t, id, sdk.ContractStatusClosed))

	if stats, ok := contractStatsSnapshot(); !ok {
		t.Fatalf("the snapshot lost its reading with nothing open: %+v", stats)
	} else if stats.Open != 0 || stats.Acquired != 1 {
		t.Fatalf("the snapshot after the last close = %+v, want acquired 1 open 0", stats)
	}
	corpus := minerExtraMetrics()
	for _, name := range []string{
		"urnet_contracts_open",
		"urnet_contract_limit_bytes",
		"urnet_contract_used_bytes",
		"urnet_contract_utilization",
	} {
		if hasContractFamily(corpus, name) {
			t.Errorf("%s outlived the last open contract:\n%s", name, corpus)
		}
	}
	if !strings.Contains(corpus, `urnet_contracts_total{result="acquired"} 1`) {
		t.Fatalf("the cumulative count went away with the open book:\n%s", corpus)
	}
}

// The snapshot the [profit] line renders from must be the same reading the
// scrape publishes: one lock, one instant, no re-derivation drift.
func TestContractStatsSnapshotAgreesWithGauges(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	device.deliver(t, false, contractOpenDetail(t, "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f", 2000, 500))
	device.deliver(t, true, contractOpenDetail(t, "1e1e1e1e-1e1e-4e1e-8e1e-1e1e1e1e1e1e", 8000, 6000))
	device.deliver(t, false, contractOpenDetail(t, "2d2d2d2d-2d2d-4d2d-8d2d-2d2d2d2d2d2d", 0, 25))

	stats, ok := contractStatsSnapshot()
	if !ok {
		t.Fatal("the snapshot has no reading")
	}
	corpus := minerExtraMetrics()
	wantContractGauge(t, corpus, "urnet_contracts_open", float64(stats.Open))
	wantContractGauge(t, corpus, "urnet_contract_limit_bytes", float64(stats.OpenLimitBytes))
	wantContractGauge(t, corpus, "urnet_contract_used_bytes", float64(stats.OpenUsedBytes))
	wantContractGauge(t, corpus, "urnet_contract_utilization", stats.AvgUtilPct)
	if !strings.Contains(corpus, `urnet_contracts_total{result="acquired"} `+strconv.FormatInt(stats.Acquired, 10)) {
		t.Fatalf("the cumulative count disagrees with the snapshot:\n%s", corpus)
	}
	// (0.25 + 0.75) / 2 = 50%: the unsized contract contributes to bytes only.
	if math.Abs(stats.AvgUtilPct-50) > 1e-9 {
		t.Fatalf("avg util = %v%%, want 50%%", stats.AvgUtilPct)
	}
}

// No denied series exists, in any family. A refused contract never opens, so it
// never reaches the provider contract feed, and the pinned SDK exports no denial
// event; a 0 would read as "nothing was refused" and an "unknown" on every line
// would read as "checked, cannot tell".
func TestContractDeniedIsNeverPublished(t *testing.T) {
	withFreshContractOutcomes(t)
	device := &fakeContractOutcomeSource{}
	defer registerContractOutcomes(device)()

	device.deliver(t, false, contractOpenDetail(t, "55555555-5555-4555-8555-555555555555", 1000, 500))
	corpus := minerExtraMetrics()
	for _, forbidden := range []string{
		`urnet_contracts_total{result="denied"}`,
		`urnet_lifetime_contracts_total{result="denied"}`,
		"denied=",
	} {
		if strings.Contains(corpus, forbidden) {
			t.Fatalf("scrape carries %q with no denial source:\n%s", forbidden, corpus)
		}
	}
	if !strings.Contains(corpus, `urnet_contracts_total{result="acquired"} 1`) {
		t.Fatalf("the acquired label is missing its count:\n%s", corpus)
	}
}

// The lifetime collector folds the process counter into the all-time store, and
// never moves the persisted denied total, because the miner has no source for
// one.
func TestLifetimeCollectorLeavesDeniedUntouched(t *testing.T) {
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
