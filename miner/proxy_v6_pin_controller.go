package miner

// proxy_v6_pin_controller.go - decide, per proxied slot, whether its provider
// runs the IPv6 pin (sdk SetPinnedIpv6Enabled), from probe rounds
// (proxy_v6_probe_staged.go), the persisted ledger (proxy_v6_probe.go,
// proxy_v6_ledger_file.go) and the pin's own dials.
//
// Cost is the design constraint: most proxies are IPv4-only and must pay
// nothing but an occasional probe. So:
//   - one controller per run, with ONE scheduler goroutine for every slot;
//     a probe runs on a transient worker, at most v6ProbeConcurrency at once;
//   - a slot's start never waits on it: the agnostic transport starts with the
//     device, and the pin is added later, if ever;
//   - a fresh persisted verdict skips the probe (yes for 6 h, no for 15 min),
//     so a restart does not re-learn the pool;
//   - the pin exists only while the ledger holds a live yes and this host has
//     an IPv6 path (a v6-pinned transport on a host without one never dials:
//     connect holds it asleep, transport_family.go refreshFamilyHold);
//   - an unknown round changes nothing and is retried from 5 minutes, growing.
//
// Lock order: a slot's applyMu, then the controller's mu. Nothing calls into
// a device under mu.

import (
	"context"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

const (
	// probes in flight at once across the run. Each holds one connection to
	// its proxy for a few hundred milliseconds when the proxy answers.
	v6ProbeConcurrency = 4
	// the first retry after an unknown round, doubling up to the cap
	v6UnknownRetryMin = 5 * time.Minute
	v6UnknownRetryMax = time.Hour
	// a yes is re-probed this long before it expires, so an unknown re-probe
	// has time to be retried before a working pin is retired for lack of
	// fresh evidence
	v6YesRefreshLead = 30 * time.Minute
	// a host without an IPv6 path is re-checked this often
	v6HostRecheck = 15 * time.Minute
	// in-service failures closer together than this count once: one transport
	// dial can make several dialer attempts, and three of them in one second
	// are one event, not three
	v6ServiceFailureSpacing = 10 * time.Second
)

// v6PinDevice is the part of the sdk device the controller drives.
type v6PinDevice interface {
	SetPinnedIpv6Enabled(enabled bool)
	SetPinnedIpv6DialObserver(observer sdk.PinnedIpv6DialObserver)
}

// v6PinSlot is one registered proxied slot.
type v6PinSlot struct {
	controller *v6PinController
	key        string
	label      string
	device     v6PinDevice
	probe      v6ProbeSettings
	ctx        context.Context
	cancel     context.CancelFunc

	// applyMu serializes the calls into the device, so the last decision
	// wins. Taken before the controller's mu.
	applyMu sync.Mutex
	// guarded by applyMu: what the device was last told
	applied bool

	// guarded by the controller's mu
	removed            bool
	probing            bool
	nextProbe          time.Time
	unknownRetry       time.Duration
	lastCountedFailure time.Time
}

// PinnedIpv6Dial is the sdk's report of one of the pin's dials. It runs on the
// dial goroutine, so it only records; a flip is applied on its own goroutine,
// because closing the pin from inside the pin's own dial would wait on itself.
func (self *v6PinSlot) PinnedIpv6Dial(success bool) {
	self.controller.serviceDial(self, success)
}

// v6PinController is the run's one controller.
type v6PinController struct {
	ledger    *v6Ledger
	persister *v6LedgerPersister
	now       func() time.Time
	// seams: production probes through the proxy and asks connect whether
	// this host has an IPv6 path
	runProbe func(ctx context.Context, settings v6ProbeSettings) v6ProbeRound
	hostV6   func() bool
	log      func(format string, args ...any)

	mu      sync.Mutex
	slots   map[*v6PinSlot]struct{}
	running int
	wake    chan struct{}
	// joins the transient workers on stop
	workers sync.WaitGroup
}

func newV6PinController(ledger *v6Ledger, persister *v6LedgerPersister) *v6PinController {
	return &v6PinController{
		ledger:    ledger,
		persister: persister,
		now:       time.Now,
		runProbe:  runV6ProbeRound,
		hostV6:    func() bool { return connect.FamilySupported(6) },
		log:       importantLogf,
		slots:     map[*v6PinSlot]struct{}{},
		wake:      make(chan struct{}, 1),
	}
}

func (self *v6PinController) signal() {
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

// v6ProbeTargetFromUrl is the host and port a platform url names, as the
// transport dials it: the scheme's default port when the url has none.
func v6ProbeTargetFromUrl(platformUrl string) (v6ProbeTarget, bool) {
	parsed, err := url.Parse(platformUrl)
	if err != nil || parsed.Hostname() == "" {
		return v6ProbeTarget{}, false
	}
	port := 443
	if parsed.Port() != "" {
		port, err = strconv.Atoi(parsed.Port())
		if err != nil {
			return v6ProbeTarget{}, false
		}
	} else if parsed.Scheme == "ws" || parsed.Scheme == "http" {
		port = 80
	}
	return v6ProbeTarget{host: parsed.Hostname(), port: port}, true
}

// v6ControlPlatformUrl is the platform url the v4 control connects to: the
// space's v4 url, else its family-agnostic url.
func v6ControlPlatformUrl(networkSpace *sdk.NetworkSpace) string {
	if platformUrlV4 := networkSpace.GetPlatformUrlV4(); platformUrlV4 != "" {
		return platformUrlV4
	}
	return networkSpace.GetPlatformUrl()
}

// register adds a proxied slot and returns its stop, which the slot defers
// before its device closes. It never waits on a probe: a slot whose ledger
// entry is a live yes gets its pin on a transient goroutine at once, any
// other slot is probed when the scheduler gets to it. nil settings (the
// direct slot) or a space with no v6 url registers nothing.
func (self *v6PinController) register(
	ctx context.Context,
	proxySettings *connect.ProxySettings,
	label string,
	device v6PinDevice,
	platformUrlV6 string,
	platformUrlV4 string,
) func() {
	if self == nil || proxySettings == nil || device == nil {
		return func() {}
	}
	v6Target, ok := v6ProbeTargetFromUrl(platformUrlV6)
	if !ok {
		return func() {}
	}
	v4Target, ok := v6ProbeTargetFromUrl(platformUrlV4)
	if !ok {
		return func() {}
	}
	slotCtx, cancel := context.WithCancel(ctx)
	slot := &v6PinSlot{
		controller: self,
		key:        proxySettings.Key(),
		label:      label,
		device:     device,
		ctx:        slotCtx,
		cancel:     cancel,
		probe: v6ProbeSettings{
			dialProxy: newV6ProbeProxyDialer(proxySettings.Network, proxySettings.Address),
			auth:      proxySettings.Auth,
			v4Target:  v4Target,
			v6Target:  v6Target,
		},
	}
	// the observer goes on before any pin can exist, so the first dial is
	// heard
	device.SetPinnedIpv6DialObserver(slot)

	self.mu.Lock()
	self.slots[slot] = struct{}{}
	self.scheduleFromLedgerWithLock(slot)
	self.mu.Unlock()
	self.applyAsync(slot)
	self.signal()

	return func() {
		self.mu.Lock()
		slot.removed = true
		delete(self.slots, slot)
		self.mu.Unlock()
		cancel()
		// an apply in flight finishes before the device is closed
		slot.applyMu.Lock()
		slot.applyMu.Unlock()
		self.signal()
	}
}

// scheduleFromLedgerWithLock sets the slot's next probe from its ledger
// entry: a live yes is re-probed shortly before it expires, a live no when it
// expires, anything else now. Must be called with mu.
func (self *v6PinController) scheduleFromLedgerWithLock(slot *v6PinSlot) {
	now := self.now()
	slot.nextProbe = now
	entry, ok := self.ledger.entry(slot.key)
	if !ok || !entry.live(now) {
		return
	}
	switch entry.Verdict {
	case v6Yes:
		slot.nextProbe = entry.CheckedAt.Add(v6YesTTL - v6YesRefreshLead)
	case v6No:
		slot.nextProbe = entry.CheckedAt.Add(v6NoTTL)
	}
	slot.unknownRetry = 0
}

// wantedWithLock is whether the slot should run the pin now.
func (self *v6PinController) wantedWithLock(slot *v6PinSlot) bool {
	return !slot.removed && self.ledger.Verdict(slot.key) == v6Yes && self.hostV6()
}

// apply brings the device in line with the wanted state. Serialized per slot,
// and the wanted state is read under the serialization, so the last call
// leaves the device as wanted then.
func (self *v6PinController) apply(slot *v6PinSlot) {
	slot.applyMu.Lock()
	defer slot.applyMu.Unlock()
	self.mu.Lock()
	removed := slot.removed
	wanted := self.wantedWithLock(slot)
	self.mu.Unlock()
	if removed || wanted == slot.applied {
		return
	}
	slot.device.SetPinnedIpv6Enabled(wanted)
	slot.applied = wanted
	if wanted {
		self.log("🌐 [ipv6] %s: IPv6 pin on (the proxy reaches the platform over IPv6)\n", slot.label)
	} else {
		self.log("🌐 [ipv6] %s: IPv6 pin off\n", slot.label)
	}
}

// applyAsync applies on a transient goroutine, joined by stop.
func (self *v6PinController) applyAsync(slot *v6PinSlot) {
	self.workers.Add(1)
	go func() {
		defer self.workers.Done()
		self.apply(slot)
	}()
}

// serviceDial records one of the pin's dials (see PinnedIpv6Dial).
func (self *v6PinController) serviceDial(slot *v6PinSlot, success bool) {
	flipped := func() bool {
		self.mu.Lock()
		defer self.mu.Unlock()
		if slot.removed {
			return false
		}
		if success {
			self.ledger.ServiceSuccess(slot.key)
			return false
		}
		now := self.now()
		if !slot.lastCountedFailure.IsZero() && now.Sub(slot.lastCountedFailure) < v6ServiceFailureSpacing {
			return false
		}
		slot.lastCountedFailure = now
		if !self.ledger.ServiceFailure(slot.key) {
			return false
		}
		slot.nextProbe = now.Add(v6NoTTL)
		slot.unknownRetry = 0
		return true
	}()
	if !flipped {
		return
	}
	self.persister.MarkDirty()
	self.log("🌐 [ipv6] %s: %d IPv6 dials failed through the proxy while it carried IPv4; recording no\n", slot.label, v6FlipAfter)
	self.applyAsync(slot)
	self.signal()
}

// record applies one round's verdict. Must be called with mu.
func (self *v6PinController) recordWithLock(slot *v6PinSlot, round v6ProbeRound) {
	now := self.now()
	switch round.verdict {
	case v6Yes, v6No:
		self.ledger.Record(slot.key, round.verdict)
		self.persister.MarkDirty()
		self.scheduleFromLedgerWithLock(slot)
	default:
		self.ledger.Record(slot.key, v6Unknown)
		if slot.unknownRetry == 0 {
			slot.unknownRetry = v6UnknownRetryMin
		} else {
			slot.unknownRetry = min(slot.unknownRetry*2, v6UnknownRetryMax)
		}
		slot.nextProbe = now.Add(slot.unknownRetry)
	}
}

// probeOne runs one round for a slot on a transient worker.
func (self *v6PinController) probeOne(slot *v6PinSlot) {
	defer self.workers.Done()
	round := self.runProbe(slot.ctx, slot.probe)
	self.mu.Lock()
	self.running--
	slot.probing = false
	changed := false
	if !slot.removed && slot.ctx.Err() == nil {
		previous := self.ledger.Last(slot.key)
		self.recordWithLock(slot, round)
		changed = round.verdict != v6Unknown && round.verdict != previous
	}
	self.mu.Unlock()
	if changed {
		// one line per change of answer, never per round: most proxies
		// answer no every 15 minutes for the life of the run
		tlog("[ipv6] %s: probe %s (v4 control %s, v6 %s)\n", slot.label, round.verdict, round.v4, round.v6)
	}
	self.apply(slot)
	self.signal()
}

// run is the scheduler: it starts the due probes, at most v6ProbeConcurrency
// at once, flushes the ledger in batches, and sleeps until the next due time.
func (self *v6PinController) run(ctx context.Context) {
	flush := time.NewTicker(v6LedgerSaveInterval)
	defer flush.Stop()
	for {
		var next time.Time
		func() {
			self.mu.Lock()
			defer self.mu.Unlock()
			now := self.now()
			hostV6 := self.hostV6()
			for slot := range self.slots {
				if slot.probing {
					continue
				}
				if !slot.nextProbe.After(now) {
					if !hostV6 {
						// a pin could not dial from here anyway; spend nothing
						slot.nextProbe = now.Add(v6HostRecheck)
					} else if self.running < v6ProbeConcurrency {
						slot.probing = true
						self.running++
						self.workers.Add(1)
						go self.probeOne(slot)
						continue
					} else {
						// a worker's finish signals the next pass
						continue
					}
				}
				if next.IsZero() || slot.nextProbe.Before(next) {
					next = slot.nextProbe
				}
			}
		}()
		var timer *time.Timer
		var timerC <-chan time.Time
		if !next.IsZero() {
			timer = time.NewTimer(max(next.Sub(self.now()), time.Millisecond))
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
		case <-self.wake:
		case <-timerC:
		case <-flush.C:
			self.persister.FlushIfDue()
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// start runs the scheduler until ctx ends, then joins every worker and
// writes the ledger a last time (if this process owns it).
func (self *v6PinController) start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		self.run(ctx)
	}()
	return func() {
		cancel()
		<-done
		self.workers.Wait()
		self.persister.Stop()
	}
}

// takeOver is the hot-swap successor's side: fold in the parent's final
// verdicts, become the ledger's writer, and bring every slot in line with
// what it now knows.
func (self *v6PinController) takeOver() {
	self.mergeFromDisk()
	self.persister.SetOwner(true)
}

// mergeFromDisk folds the file's newer entries into the ledger and re-plans
// the slots they belong to.
func (self *v6PinController) mergeFromDisk() {
	changed := self.ledger.mergeNewer(readV6LedgerEntries(self.persister.path))
	if len(changed) == 0 {
		return
	}
	keys := map[string]bool{}
	for _, key := range changed {
		keys[key] = true
	}
	toApply := []*v6PinSlot{}
	self.mu.Lock()
	for slot := range self.slots {
		if keys[slot.key] && !slot.probing {
			self.scheduleFromLedgerWithLock(slot)
			toApply = append(toApply, slot)
		}
	}
	self.mu.Unlock()
	for _, slot := range toApply {
		self.applyAsync(slot)
	}
	self.signal()
}

// The run's controller, for the hot-swap paths in hotswap.go, which have no
// handle on the run. Nil outside a provider run.
var (
	processV6PinsMu sync.Mutex
	processV6Pins   *v6PinController
)

func setProcessV6Pins(controller *v6PinController) {
	processV6PinsMu.Lock()
	defer processV6PinsMu.Unlock()
	processV6Pins = controller
}

func currentV6Pins() *v6PinController {
	processV6PinsMu.Lock()
	defer processV6PinsMu.Unlock()
	return processV6Pins
}

// stopV6LedgerWritesForDrain is the hot-swap parent's side: from the start
// of its drain its closing transports only produce stale evidence, so it
// writes once and stops.
func stopV6LedgerWritesForDrain() {
	if controller := currentV6Pins(); controller != nil {
		controller.persister.Stop()
	}
}

// takeOverV6LedgerAfterHandoff is the successor's side, run at takeover and
// again once the parent's drain is over, as the audit ring is.
func takeOverV6LedgerAfterHandoff(ctx context.Context, wait time.Duration) {
	controller := currentV6Pins()
	if controller == nil {
		return
	}
	controller.takeOver()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
	}
	controller.mergeFromDisk()
	_ = controller.persister.SaveNow()
}
