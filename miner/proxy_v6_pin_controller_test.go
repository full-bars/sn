package miner

// The pin controller with an injected clock, a scripted probe and a fake
// device. The scheduler runs for real; time moves only when a test moves it,
// and every wait is a poll of at most two seconds.

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

type fakePinDevice struct {
	mu       sync.Mutex
	enabled  []bool
	observer sdk.PinnedIpv6DialObserver
}

func (self *fakePinDevice) SetPinnedIpv6Enabled(enabled bool) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.enabled = append(self.enabled, enabled)
}

func (self *fakePinDevice) SetPinnedIpv6DialObserver(observer sdk.PinnedIpv6DialObserver) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.observer = observer
}

func (self *fakePinDevice) currentObserver() sdk.PinnedIpv6DialObserver {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.observer
}

func (self *fakePinDevice) calls() []bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	return append([]bool(nil), self.enabled...)
}

func (self *fakePinDevice) pinOn() bool {
	calls := self.calls()
	return len(calls) > 0 && calls[len(calls)-1]
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (self *testClock) Now() time.Time {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.now
}

func (self *testClock) Advance(d time.Duration) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.now = self.now.Add(d)
}

// scriptedProbe answers each round with the next verdict and counts rounds.
type scriptedProbe struct {
	mu       sync.Mutex
	verdicts []v6Verdict
	rounds   int
}

func (self *scriptedProbe) run(ctx context.Context, settings v6ProbeSettings) v6ProbeRound {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.rounds++
	verdict := v6Unknown
	if len(self.verdicts) > 0 {
		verdict = self.verdicts[0]
		self.verdicts = self.verdicts[1:]
	}
	return v6ProbeRound{verdict: verdict}
}

func (self *scriptedProbe) count() int {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.rounds
}

type pinFixture struct {
	clock      *testClock
	probe      *scriptedProbe
	controller *v6PinController
	path       string
	hostV6     bool
	hostMu     sync.Mutex
	stop       func()
}

func newPinFixture(t *testing.T, path string, verdicts ...v6Verdict) *pinFixture {
	t.Helper()
	fixture := &pinFixture{
		clock:  &testClock{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)},
		probe:  &scriptedProbe{verdicts: verdicts},
		path:   path,
		hostV6: true,
	}
	ledger := loadV6Ledger(path, fixture.clock.Now)
	persister := newV6LedgerPersister(path, ledger, true)
	persister.now = fixture.clock.Now
	controller := newV6PinController(ledger, persister)
	controller.now = fixture.clock.Now
	controller.runProbe = fixture.probe.run
	controller.hostV6 = func() bool {
		fixture.hostMu.Lock()
		defer fixture.hostMu.Unlock()
		return fixture.hostV6
	}
	controller.log = func(string, ...any) {}
	fixture.controller = controller
	fixture.stop = controller.start(context.Background())
	t.Cleanup(func() { fixture.stop() })
	return fixture
}

func (self *pinFixture) advance(d time.Duration) {
	self.clock.Advance(d)
	self.controller.signal()
}

func waitForPin(t *testing.T, what string, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func testProxy(address string) *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: address}
}

func (self *pinFixture) register(t *testing.T, proxySettings *connect.ProxySettings, device *fakePinDevice) func() {
	t.Helper()
	return self.controller.register(
		context.Background(), proxySettings, "test slot", device,
		"wss://connect-v6.test.invalid", "wss://connect-v4.test.invalid",
	)
}

func TestV6PinYesEnablesThePin(t *testing.T) {
	fixture := newPinFixture(t, testV6Path(t), v6Yes)
	device := &fakePinDevice{}
	defer fixture.register(t, testProxy("198.51.100.9:1080"), device)()
	waitForPin(t, "the pin to come on", device.pinOn)
	if device.currentObserver() == nil {
		t.Fatal("the dial observer was not installed")
	}
	// the next probe is a refresh before the yes expires, not sooner
	fixture.advance(v6YesTTL - v6YesRefreshLead - time.Minute)
	time.Sleep(20 * time.Millisecond)
	if fixture.probe.count() != 1 {
		t.Fatalf("probe rounds = %d before the refresh is due, want 1", fixture.probe.count())
	}
	fixture.advance(time.Minute)
	waitForPin(t, "the refresh probe", func() bool { return fixture.probe.count() == 2 })
}

func TestV6PinNoKeepsOneTransport(t *testing.T) {
	fixture := newPinFixture(t, testV6Path(t), v6No, v6No)
	device := &fakePinDevice{}
	defer fixture.register(t, testProxy("198.51.100.10:1080"), device)()
	waitForPin(t, "the first probe", func() bool { return fixture.probe.count() == 1 })
	time.Sleep(20 * time.Millisecond)
	if calls := device.calls(); len(calls) != 0 {
		t.Fatalf("a no proxy's device was told %v; it must pay nothing", calls)
	}
	fixture.advance(v6NoTTL - time.Second)
	time.Sleep(20 * time.Millisecond)
	if fixture.probe.count() != 1 {
		t.Fatal("a live no was re-probed early")
	}
	fixture.advance(time.Second)
	waitForPin(t, "the re-probe when no expires", func() bool { return fixture.probe.count() == 2 })
}

func TestV6PinUnknownBacksOffAndChangesNothing(t *testing.T) {
	fixture := newPinFixture(t, testV6Path(t), v6Unknown, v6Unknown, v6Unknown)
	device := &fakePinDevice{}
	defer fixture.register(t, testProxy("198.51.100.11:1080"), device)()
	waitForPin(t, "the first probe", func() bool { return fixture.probe.count() == 1 })
	for i, retry := range []time.Duration{5 * time.Minute, 10 * time.Minute} {
		fixture.advance(retry - time.Second)
		time.Sleep(20 * time.Millisecond)
		if fixture.probe.count() != i+1 {
			t.Fatalf("retry %d came before %s", i+1, retry)
		}
		fixture.advance(time.Second)
		waitForPin(t, "the retry", func() bool { return fixture.probe.count() == i+2 })
	}
	if calls := device.calls(); len(calls) != 0 {
		t.Fatalf("unknown rounds told the device %v", calls)
	}
}

// Three spaced in-service failures retire the pin and record no; failures
// inside the spacing count once.
func TestV6PinServiceFailuresFlipToNo(t *testing.T) {
	path := testV6Path(t)
	fixture := newPinFixture(t, path, v6Yes)
	device := &fakePinDevice{}
	defer fixture.register(t, testProxy("198.51.100.12:1080"), device)()
	waitForPin(t, "the pin to come on", device.pinOn)
	observer := device.currentObserver()

	observer.PinnedIpv6Dial(false)
	observer.PinnedIpv6Dial(false) // same event
	fixture.advance(v6ServiceFailureSpacing)
	observer.PinnedIpv6Dial(false)
	time.Sleep(20 * time.Millisecond)
	if !device.pinOn() {
		t.Fatal("two counted failures retired the pin")
	}
	// a success clears the run of failures
	observer.PinnedIpv6Dial(true)
	for i := 0; i < v6FlipAfter; i++ {
		fixture.advance(v6ServiceFailureSpacing)
		observer.PinnedIpv6Dial(false)
	}
	waitForPin(t, "the pin to go off", func() bool { return !device.pinOn() })
	if got := fixture.controller.ledger.Verdict(testProxy("198.51.100.12:1080").Key()); got != v6No {
		t.Fatalf("verdict after the flip = %s, want no", got)
	}
	fixture.stop()
	if got := readV6File(t, path).Entries[testProxy("198.51.100.12:1080").Key()].Verdict; got != v6No {
		t.Fatalf("the flip was not written: %s", got)
	}
}

// A restart reads the file and does not re-learn: a fresh yes goes straight
// to the pin, a fresh no to nothing, and neither is probed.
func TestV6PinRestartSkipsTheProbe(t *testing.T) {
	path := testV6Path(t)
	first := newPinFixture(t, path, v6Yes, v6No)
	yesProxy, noProxy := testProxy("198.51.100.13:1080"), testProxy("198.51.100.14:1080")
	yesDevice, noDevice := &fakePinDevice{}, &fakePinDevice{}
	stopYes := first.register(t, yesProxy, yesDevice)
	waitForPin(t, "the first probe", func() bool { return first.probe.count() == 1 })
	stopNo := first.register(t, noProxy, noDevice)
	waitForPin(t, "the second probe", func() bool { return first.probe.count() == 2 })
	stopYes()
	stopNo()
	first.stop()

	second := newPinFixture(t, path)
	second.clock.Advance(time.Minute)
	device := &fakePinDevice{}
	defer second.register(t, yesProxy, device)()
	defer second.register(t, noProxy, &fakePinDevice{})()
	waitForPin(t, "the persisted yes to turn the pin on", device.pinOn)
	time.Sleep(20 * time.Millisecond)
	if second.probe.count() != 0 {
		t.Fatalf("a restart probed %d fresh verdicts", second.probe.count())
	}
}

// A host with no IPv6 path never probes and never builds a pin.
func TestV6PinNoHostV6PaysNothing(t *testing.T) {
	fixture := newPinFixture(t, testV6Path(t), v6Yes)
	fixture.hostMu.Lock()
	fixture.hostV6 = false
	fixture.hostMu.Unlock()
	device := &fakePinDevice{}
	defer fixture.register(t, testProxy("198.51.100.15:1080"), device)()
	time.Sleep(20 * time.Millisecond)
	if fixture.probe.count() != 0 || len(device.calls()) != 0 {
		t.Fatalf("probes %d, device calls %v on a host without IPv6", fixture.probe.count(), device.calls())
	}
}

// The direct slot (nil proxy settings) is never registered.
func TestV6PinIgnoresTheDirectSlot(t *testing.T) {
	fixture := newPinFixture(t, testV6Path(t), v6Yes)
	device := &fakePinDevice{}
	defer fixture.register(t, nil, device)()
	time.Sleep(20 * time.Millisecond)
	if fixture.probe.count() != 0 || len(device.calls()) != 0 || device.currentObserver() != nil {
		t.Fatal("the direct slot was registered with the pin controller")
	}
}

// A hot-swap successor folds in the parent's newer verdicts at takeover.
func TestV6PinTakeOverMergesTheParent(t *testing.T) {
	path := testV6Path(t)
	fixture := newPinFixture(t, path)
	fixture.controller.persister.SetOwner(false)
	proxySettings := testProxy("198.51.100.16:1080")
	device := &fakePinDevice{}
	// the candidate's slot starts before the parent's last verdict lands; its
	// own round is unknown and leaves it unpinned
	defer fixture.register(t, proxySettings, device)()
	waitForPin(t, "the candidate's own probe", func() bool { return fixture.probe.count() == 1 })
	// the parent's newer verdict reaches the file
	fixture.clock.Advance(time.Second)
	parent := newV6Ledger(fixture.clock.Now)
	parent.Record(proxySettings.Key(), v6Yes)
	if err := newV6LedgerPersister(path, parent, true).SaveNow(); err != nil {
		t.Fatal(err)
	}
	if device.pinOn() {
		t.Fatal("the candidate pinned before it merged anything")
	}
	fixture.controller.takeOver()
	waitForPin(t, "the merged yes to turn the pin on", device.pinOn)
}

// TestRunRegistersV6PinController checks the wiring, as
// TestRunSetsDisableFamilyTransports does for Step 1: a deleted call site
// fails here, not on a box.
func TestRunRegistersV6PinController(t *testing.T) {
	source, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"defer v6Pins.start(ctx)()",
		"setProcessV6Pins(v6Pins)",
		"newV6LedgerPersister(v6LedgerPath, v6Verdicts, !self.hotSwapCandidate)",
		"defer v6Pins.register(proxyCtx, proxySettings, display, device, networkSpace.GetPlatformUrlV6(), v6ControlPlatformUrl(networkSpace))()",
		"settings.DisablePlatformFamilyTransports = providerDisablesFamilyTransports(proxySettings)",
	} {
		if !strings.Contains(string(source), want) {
			t.Fatalf("miner/run.go no longer contains %q", want)
		}
	}
	hotswap, err := os.ReadFile("hotswap.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"stopV6LedgerWritesForDrain()", "takeOverV6LedgerAfterHandoff(ctx, HotSwapDrainTimeout+auditReconcileGrace)"} {
		if !strings.Contains(string(hotswap), want) {
			t.Fatalf("miner/hotswap.go no longer contains %q", want)
		}
	}
}
