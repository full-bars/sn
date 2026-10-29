// The provide run narrates its own bring-up at default verbosity: one line per
// slot's authentication outcome (reused, new, failed with reason and retry)
// and, while any slot is still pending, a periodic
// "providers: R running, P pending, F failed" heartbeat. An operator watching
// a 1000-slot start sees who is up, who is still waiting, and who fell out,
// instead of inferring it from a silent stretch between the [reuse]/[new]
// stream.
package miner

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/clientauth"
)

// controlReachedIdentities is the set of proxies that have reached the control
// plane in THIS process. It is monotonic on purpose and it is NOT a liveness
// count: a proxy that reached once and then died stays in the set.
//
// Why it exists at all: proxy.state carries each proxy's persisted grade, so a
// process that has not completed a single dial can still render a few hundred
// "up" proxies from a previous run's measurements, which reads as a healthy
// pool at uptime=0s. Membership here is evidence this process produced.
//
// Why the health line calls it "reached" and not "up": with no signal that a
// slot stopped reaching, a monotonic count cannot answer "is it up now". Saying
// "up" would replace the persisted lie with a liveness lie. The word in the log
// line is what the number can support.
//
// Keyed by the slot's display identity, not by its slot index. run.go gives a
// proxy a new index after a reload, and an index-keyed counter would then count
// the same proxy twice. Display is the proxy address ("proxy:<addr>") or
// "direct"; two credentialed entries on one address share a display and would
// collapse into one count - the miner launches one slot per proxy address, so
// that shape does not occur here, and using connect.ProxySettings.Key() would
// mean threading a new argument through run.go while #230 owns that file.
var controlReachedIdentities = struct {
	sync.Mutex
	byKey map[string]struct{}
}{byKey: map[string]struct{}{}}

// controlReachedCount is the number of distinct proxies that reached the
// control plane since this process started.
func controlReachedCount() int {
	controlReachedIdentities.Lock()
	defer controlReachedIdentities.Unlock()
	return len(controlReachedIdentities.byKey)
}

// noteControlReachedIdentity records one proxy's first reach; callers hold no
// lock.
func noteControlReachedIdentity(identity string) {
	controlReachedIdentities.Lock()
	defer controlReachedIdentities.Unlock()
	controlReachedIdentities.byKey[identity] = struct{}{}
}

// resetControlReached exists for tests: the set is process-wide on purpose (the
// health line has no other handle on the bring-up tracker), so a test that
// asserts a fresh value has to start from empty.
func resetControlReached() {
	controlReachedIdentities.Lock()
	defer controlReachedIdentities.Unlock()
	controlReachedIdentities.byKey = map[string]struct{}{}
}

// providerBringUpHeartbeatInterval is the heartbeat cadence while slots are
// pending. A variable so tests can shorten it.
var providerBringUpHeartbeatInterval = 10 * time.Second

// providerSlotDisplay names a provider slot for operator lines: the direct
// worker or the proxy address, never credentials.
func providerSlotDisplay(settings *connect.ProxySettings) string {
	if settings == nil {
		return "direct"
	}
	return "proxy:" + redactedProxyAddress(settings.Address)
}

// providerBringUpLine is the heartbeat's line: the live tally of the run's
// provider slots, and how many of the slots running right now kept their client
// id across the restart (reused) versus were minted (new), and how many have
// completed a dial to the control plane. The client-id pair describes the same
// population as the running count, which is also the population the metrics and
// the status payload report: a slot that failed or was removed is in neither. A
// reused id alone proves only that a stored login was found; the reached count
// is the evidence that the slot is talking to the platform. The counts trail the
// stable "N running, N pending, N failed" prefix so existing readers of it
// still match.
func providerBringUpLine(running, pending, failed, reused, minted, reached int) string {
	return fmt.Sprintf("providers: %d running, %d pending, %d failed (client ids: %d reused, %d new; control plane reached: %d)", running, pending, failed, reused, minted, reached)
}

// providerBringUpTracker counts the launch list's slots by bring-up state.
// pending slots have not completed authentication; running slots authenticated
// (their device follows immediately); failed slots' workers returned an error
// before they could run. Safe for the concurrent provider workers.
type providerBringUpTracker struct {
	mu      sync.Mutex
	total   int
	running int
	failed  int
	// stopped counts the slots whose worker exited without failing: a reloader
	// removal or a shutdown cancels the slot's context, which is not a failure
	// to report but is a slot the run no longer serves. Pending is what is left
	// after running, failed and stopped, so a stopped slot does not reappear as
	// work still to come.
	stopped int
	states  map[uint64]string
	// identities is each authenticated slot's latest outcome: true when the
	// slot kept its client id, false when it was minted. It is the run's
	// history, not a liveness signal: the readers count only the slots whose
	// state is still "running".
	identities map[uint64]bool
	// identityOutcomes is each slot's latest identity decision, keyed like
	// identities. A slot can decide twice in one start (a refused credential,
	// then a replacement), and the start's count follows the latest, so the
	// summary reports what the pool holds now rather than a history.
	identityOutcomes map[uint64]minerIdentityOutcome
	// adoptedIdentities is how many slots the automatic legacy import took
	// over in this start. Adoption happens before any slot has an index.
	adoptedIdentities int
	// identityWarned makes the replacement wave one warning line per start.
	identityWarned bool
	// reached is the slots whose first dial through their proxy succeeded.
	reached map[uint64]bool
	// reachedIdentities dedupes reached by proxy identity: a reload gives the
	// same proxy a new slot index, and the count must not double.
	reachedIdentities map[string]bool
	// watches holds each authenticated slot's pending "never reached" check.
	watches map[uint64]*time.Timer
	// overdue is the slots whose grace expired without reaching the control
	// plane, in the order they expired. The heartbeat tick drains it into one
	// tally, so a fleet-wide outage prints one line per tick instead of one
	// warning per identity.
	overdue    []uint64
	overdueSet map[uint64]bool
	displays   map[uint64]string
	// authAt is when each authenticated slot was admitted, so the tally can
	// report the age of the wait rather than the age of the process.
	authAt map[uint64]time.Time
	// lastNotConnected and lastSummaryAt rate-limit the tally: a change in the
	// count prints on the next tick, an unchanged count at most once per
	// providerControlSummaryInterval. Without the second rule one stuck slot
	// would print every tick for the life of the process.
	lastNotConnected int
	lastSummaryAt    time.Time
	// closed is set at shutdown, so a late watch stays silent. stop() alone is
	// also called when the last slot resolves, and the watches must outlive
	// that: they exist to report the slots that authenticated and then never
	// reached the control plane.
	closed    bool
	started   bool
	startedAt time.Time
	finished  bool
	stopCh    chan struct{}
}

func newProviderBringUpTracker(total int) *providerBringUpTracker {
	return &providerBringUpTracker{
		total:             total,
		states:            map[uint64]string{},
		identities:        map[uint64]bool{},
		identityOutcomes:  map[uint64]minerIdentityOutcome{},
		reached:           map[uint64]bool{},
		reachedIdentities: map[string]bool{},
		watches:           map[uint64]*time.Timer{},
		overdueSet:        map[uint64]bool{},
		displays:          map[uint64]string{},
		authAt:            map[uint64]time.Time{},
		stopCh:            make(chan struct{}),
	}
}

// runningIdentityCounts returns how many of the slots running right now reused
// their client id and how many minted one. A slot that authenticated and then
// failed or was removed keeps its entry in identities (that is the run's
// history) but is in neither count, which is what makes these the same numbers
// the heartbeat, the metrics and the status payload report. The caller must
// hold mu.
func (self *providerBringUpTracker) runningIdentityCounts() (reused, minted int) {
	for index, wasReused := range self.identities {
		if self.states[index] != "running" {
			continue
		}
		if wasReused {
			reused++
		} else {
			minted++
		}
	}
	return reused, minted
}

// pendingLocked is the slots still waiting on an outcome: launched, and neither
// running, failed nor stopped. It clamps at zero because a reload can raise the
// total after slots have already failed, and an operator line reading
// "489 running, -191 pending" costs more trust than a zero ever would.
// Callers must hold mu.
func (self *providerBringUpTracker) pendingLocked() int {
	pending := self.total - self.running - self.failed - self.stopped
	if pending < 0 {
		return 0
	}
	return pending
}

// providerBringUpCurrent is the run's bring-up tracker. The /metrics scrape and
// the status payload read their client-id counts from it, so all three
// surfaces report the same numbers instead of each keeping its own tally. Nil
// before a run starts and again once it ends, which is what makes both read
// zero rather than the previous run's counts.
var providerBringUpCurrent atomic.Pointer[providerBringUpTracker]

// setProviderBringUpTracker publishes the run's tracker; nil clears it.
func setProviderBringUpTracker(tracker *providerBringUpTracker) {
	providerBringUpCurrent.Store(tracker)
}

// providerClientIDCounts is the running slots split by how this run's identity
// started: reused from a stored login, or minted. live is false when no run has
// begun, so a caller that reports provenance can say "not measured" instead of
// publishing zeros.
func providerClientIDCounts() (reused, minted int, live bool) {
	tracker := providerBringUpCurrent.Load()
	if tracker == nil {
		return 0, 0, false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	reused, minted = tracker.runningIdentityCounts()
	return reused, minted, true
}

// writeProviderClientIDMetrics renders the running slots by identity
// provenance. The counts follow the live tracker, so a slot that comes up adds
// to one of them and a slot that leaves running drops out of both; with no run
// started there is no tracker and both read 0.
func writeProviderClientIDMetrics(b *strings.Builder) {
	reused, minted, _ := providerClientIDCounts()
	fmt.Fprintf(b, "# HELP urnet_provider_client_ids_reused Running provider slots whose client id was reused from a stored login this run.\n")
	fmt.Fprintf(b, "# TYPE urnet_provider_client_ids_reused gauge\n")
	fmt.Fprintf(b, "urnet_provider_client_ids_reused %d\n", reused)
	fmt.Fprintf(b, "# HELP urnet_provider_client_ids_minted Running provider slots whose client id was minted this run.\n")
	fmt.Fprintf(b, "# TYPE urnet_provider_client_ids_minted gauge\n")
	fmt.Fprintf(b, "urnet_provider_client_ids_minted %d\n", minted)
}

// start emits the opening heartbeat and begins the periodic one. It is a no-op
// when every slot is already resolved, and idempotent.
func (self *providerBringUpTracker) start() {
	if self == nil {
		return
	}
	self.mu.Lock()
	if self.started {
		self.mu.Unlock()
		return
	}
	self.started = true
	self.startedAt = time.Now()
	pending := self.pendingLocked()
	self.mu.Unlock()
	// Read the interval here, not inside the goroutine: the variable is a test
	// seam, and a goroutine that starts late would read it while the test that
	// shortened it is restoring it.
	interval := providerBringUpHeartbeatInterval
	// The overdue reporter drains its queue a few slots per tick. It outlives
	// the heartbeat below on purpose: the heartbeat stops once every slot has
	// authenticated, but a slot that authenticated and never dialed is exactly
	// the one still worth naming.
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			self.mu.Lock()
			closed := self.closed
			self.mu.Unlock()
			if closed {
				return
			}
			self.reportOverdue()
		}
	}()
	// Emit a startup summary after one heartbeat interval so the counts have
	// settled. Use the same interval as the heartbeat so tests can shorten both
	// together.
	go func() {
		time.Sleep(interval)
		self.mu.Lock()
		closed := self.closed
		self.mu.Unlock()
		if !closed {
			self.emitStartupSummary()
		}
	}()
	self.emit()
	if pending <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-self.stopCh:
				return
			case <-ticker.C:
				self.mu.Lock()
				pending := self.pendingLocked()
				resolved := pending <= 0
				self.mu.Unlock()
				if resolved {
					// The resolving transition already emitted the final
					// tally; nothing is pending, so nothing left to report.
					return
				}
				self.emit()
			}
		}
	}()
}

// stop ends the heartbeat goroutine. Safe to call more than once.
func (self *providerBringUpTracker) stop() {
	if self == nil {
		return
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	select {
	case <-self.stopCh:
	default:
		close(self.stopCh)
	}
}

// emit writes the current tally. The caller must not hold mu.
func (self *providerBringUpTracker) emit() {
	if self == nil {
		return
	}
	self.mu.Lock()
	if self.closed {
		self.mu.Unlock()
		return
	}
	pending := self.pendingLocked()
	reused, minted := self.runningIdentityCounts()
	line := providerBringUpLine(self.running, pending, self.failed, reused, minted, len(self.reachedIdentities))
	self.mu.Unlock()
	tlog("%s\n", line)
}

// emitStartupSummary emits a one-line summary of the bring-up after the
// first heartbeat interval so the operator sees the pool split at a glance.
// The identity half is the start's mints in one place: the per-slot lines name
// each one, but a count is what tells an operator whether the pool healed or
// the state directory it read was not the one holding the identities.
func (self *providerBringUpTracker) emitStartupSummary() {
	if self == nil {
		return
	}
	self.mu.Lock()
	if self.closed || self.total == 0 {
		self.mu.Unlock()
		return
	}
	total := self.total
	running := self.running
	failed := self.failed
	pending := self.pendingLocked()
	reused, minted := self.runningIdentityCounts()
	reached := len(self.reachedIdentities)
	summary := fmt.Sprintf("[startup] %d slots: %d auth ok, %d pending, %d failed (%d reused, %d new); control-connected %d",
		total, running, pending, failed, reused, minted, reached)
	// The reused/new pair above is what the credential path did; this is what
	// the registration path decided. They agree except in the retained
	// operation's crash window, where a credential is recovered for an identity
	// that was never minted here.
	if segment := self.identitySegment(); segment != "" {
		summary += "; " + segment
	}
	self.mu.Unlock()
	tlog("%s\n", summary)
	// The replacement wave is not decided here. A slot still authenticating has
	// not presented its credential yet, so a rate taken now divides by a pool
	// that does not exist: four replacements among four decided slots reads as
	// 100% while 496 healthy slots are still coming up. apply() decides it when
	// the last slot resolves.
}

// identitySegment renders the identity outcomes this start produced, or "" when
// every slot either kept its identity or has not decided yet. Leaving the
// segment out of a healthy start keeps that line byte-identical to the one
// existing readers and release notes quote.
func (self *providerBringUpTracker) identitySegment() string {
	mintedNew, replacements, adopted, revoked := self.identityOutcomeCounts()
	if mintedNew+replacements+adopted+revoked == 0 {
		return ""
	}
	return fmt.Sprintf("identity: %d minted new, %d minted replacement, %d adopted, %d revoked",
		mintedNew, replacements, adopted, revoked)
}

// identityOutcomeCounts returns this start's identity outcomes. Each slot
// counts once, as its latest outcome, and adoption is counted where the
// automatic legacy import took a slot over. The caller must hold mu.
func (self *providerBringUpTracker) identityOutcomeCounts() (mintedNew, replacements, adopted, revoked int) {
	adopted = self.adoptedIdentities
	for _, outcome := range self.identityOutcomes {
		switch outcome {
		case identityOutcomeMintedNew:
			mintedNew++
		case identityOutcomeMintedReplacement:
			replacements++
		case identityOutcomeRevoked:
			revoked++
		}
	}
	return
}

// Replacement waves. A handful of replacements is a handful of slots healing;
// a wave is a host that read a state directory without its identities, or a
// platform refusing identities that used to work. The two look identical from
// inside the process, so the threshold is what separates the case worth a
// warning from the case worth a line.
const (
	identityWavePercent = 5
	identityWaveFloor   = 3
)

// identityWaveThreshold is how many rejected identities in one start stop
// being a few slots healing: more than max(3, 5% of the identities the pool
// presented to the platform).
func identityWaveThreshold(presented int) int {
	threshold := presented * identityWavePercent / 100
	if threshold < identityWaveFloor {
		threshold = identityWaveFloor
	}
	return threshold
}

// identityWaveCounts returns how many slots presented a saved identity to the
// platform and how many of those it did not accept. A slot that reused its
// credential and a slot that was refused both presented one; so did a slot
// that came back with a replacement. A slot that had nothing to present is not
// part of the rate: a first start mints every identity, and counting those
// would warn on a host doing exactly what it should. The caller must hold mu.
func (self *providerBringUpTracker) identityWaveCounts() (rejected, presented int) {
	reused := 0
	for _, wasReused := range self.identities {
		if wasReused {
			reused++
		}
	}
	var replacements, revoked int
	for _, outcome := range self.identityOutcomes {
		switch outcome {
		case identityOutcomeMintedReplacement:
			replacements++
		case identityOutcomeRevoked:
			revoked++
		}
	}
	return replacements + revoked, reused + replacements + revoked
}

// settledLocked reports that every slot reached a terminal state, so the
// identity counts are final. The caller must hold mu.
func (self *providerBringUpTracker) settledLocked() bool {
	return self.total <= 0 || self.running+self.failed >= self.total
}

// replacementWave decides whether this start's rejected identities are a wave
// and claims the one warning. It refuses to decide before the start has
// settled: the rate is rejected over presented, and neither half is known
// until the last slot resolves. The caller must hold mu.
func (self *providerBringUpTracker) replacementWave() (rejected, presented, threshold int, wave bool) {
	rejected, presented = self.identityWaveCounts()
	threshold = identityWaveThreshold(presented)
	if self.identityWarned || !self.settledLocked() || threshold >= rejected {
		return rejected, presented, threshold, false
	}
	self.identityWarned = true
	return rejected, presented, threshold, true
}

// emitReplacementWave prints the one warning a start gets when its rejected
// identities were a wave. Called when the last slot resolves.
func (self *providerBringUpTracker) emitReplacementWave() {
	if self == nil {
		return
	}
	self.mu.Lock()
	rejected, presented, threshold, wave := self.replacementWave()
	self.mu.Unlock()
	if wave {
		tlog("%s\n", identityWaveLine(rejected, presented, threshold))
	}
}

// identityWaveLine is the one warning a start prints when the platform did not
// accept identities in bulk. It names both causes and the order to check them:
// the state directory is cheaper to inspect and is the one that produces a full
// wave while every other measure looks healthy.
func identityWaveLine(rejected, presented, threshold int) string {
	return fmt.Sprintf("⚠️ [identity] %d of %d saved identities were replaced or refused this start (threshold %d); check first that the state directory is the one holding the identities (wrong mount, a restore older than the identities, permissions), then whether the platform is refusing identities that used to work",
		rejected, presented, threshold)
}

// identityOutcome records one slot's identity decision: it becomes the slot's
// counted outcome and, when it is one of the four that change a slot's
// identity, one durable event. The same decision reported twice for one slot is
// recorded once, so a retry that lands on the same outcome does not turn into a
// second mint.
func (self *providerBringUpTracker) identityOutcome(index uint64, display string, outcome clientauth.IdentityOutcome, reason string) {
	recorded, ok := minerIdentityOutcomeOf(outcome)
	if self == nil || !ok {
		return
	}
	self.mu.Lock()
	previous, seen := self.identityOutcomes[index]
	self.identityOutcomes[index] = recorded
	self.mu.Unlock()
	if seen && previous == recorded {
		return
	}
	// Outside the lock: the store writes to disk, and a slow disk must not
	// hold up the slots reporting behind this one.
	recordProviderIdentityEvent(display, recorded, reason)
}

// adoptIdentities records the slots the automatic legacy import took over
// before any worker ran. Adoption is its own outcome, not a mint, and a slot
// that authenticates with the imported identity records nothing more: that
// identity is the one the import proved. The records go in one transaction,
// because this runs before the first proxy does and a disk sync per adopted
// identity would be paid by every start on a migrated host.
func (self *providerBringUpTracker) adoptIdentities(displays []string) {
	if self == nil || len(displays) == 0 {
		return
	}
	self.mu.Lock()
	self.adoptedIdentities += len(displays)
	self.mu.Unlock()
	recordProviderIdentityEvents(displays, identityOutcomeAdopted, "")
}

// authReused narrates one slot's reused authentication and moves it to
// running. The leading text matches the line the v3.23 provider and
// docs/Hot-Restart.md document, so a grep for "(reused)" finds every slot that
// kept its identity; the slot follows it.
// The per-slot identity lines are the ones an operator greps on every start to
// see whether the proxies came up with the identity they should have, and
// docs/Hot-Restart.md documents them by shape. They are constants so that a
// change to one is a visible diff and the test beside them fails if the shape
// moves.
const (
	providerClientIdReusedFormat = "♻️ client_id: %s (reused) provider=%d slot=%s\n"
	providerClientIdNewFormat    = "✨ client_id: %s (new) provider=%d slot=%s\n"
)

func (self *providerBringUpTracker) authReused(index uint64, clientID connect.Id, display string) {
	self.apply(index, "running", func() {
		self.identities[index] = true
		self.watchControl(index, display)
		tlog(providerClientIdReusedFormat, clientID, index, display)
	})
}

// authNew narrates one slot's fresh authentication and moves it to running.
func (self *providerBringUpTracker) authNew(index uint64, clientID connect.Id, display string) {
	self.apply(index, "running", func() {
		self.identities[index] = false
		self.watchControl(index, display)
		tlog(providerClientIdNewFormat, clientID, index, display)
	})
}

// providerControlGrace is how long an authenticated slot may go without a
// successful dial before it is called out. A variable so tests can shorten it.
var providerControlGrace = 60 * time.Second

// shutdown ends the tracker's heartbeat and cancels every pending control
// check. Called when the run ends.
func (self *providerBringUpTracker) shutdown() {
	if self == nil {
		return
	}
	self.mu.Lock()
	self.closed = true
	for index, watch := range self.watches {
		watch.Stop()
		delete(self.watches, index)
	}
	self.mu.Unlock()
	// A finished run publishes nothing: the gauges and the status payload must
	// read zero, not the tally of the run that just ended.
	providerBringUpCurrent.CompareAndSwap(self, nil)
	self.stop()
}

// providerControlNameLimit is how many not-yet-connected slots one tick may
// name. Beyond it the summary's count and sample carry the scale: a start in
// which the control plane is unreachable for every identity would otherwise
// print one warning per slot, which is the flood this cap removes.
const providerControlNameLimit = 5

// watchControl starts the slot's "never reached the control plane" check. The
// caller holds mu. The check only queues the slot: the line is printed by
// reportOverdue, so arrivals from many slots at once share one tick and one
// summary instead of printing concurrently.
func (self *providerBringUpTracker) watchControl(index uint64, display string) {
	if self.reached[index] {
		return
	}
	self.displays[index] = display
	self.authAt[index] = time.Now()
	if previous := self.watches[index]; previous != nil {
		previous.Stop()
	}
	self.watches[index] = time.AfterFunc(providerControlGrace, func() {
		self.mu.Lock()
		reached, closed := self.reached[index], self.closed
		if !reached && !closed && !self.overdueSet[index] {
			self.overdue = append(self.overdue, index)
			self.overdueSet[index] = true
		}
		self.mu.Unlock()
		// Promptness without the flood: reportOverdue prints only when the
		// not-connected count changed or its repeat interval elapsed, so the
		// first expiry of a wave prints and the rest of the wave is folded
		// into the next due tally.
		if !reached && !closed {
			self.reportOverdue()
		}
	})
}

// providerControlSummaryInterval is how often an unchanged not-connected count
// is repeated. A count that changes prints on the next tick.
const providerControlSummaryInterval = 5 * time.Minute

// providerControlChangeGap is the shortest gap between two tallies that report
// a changed count: a pool that flaps would otherwise print on every tick.
const providerControlChangeGap = time.Minute

// reportOverdue prints one tally for the slots whose grace expired without
// reaching the control plane: how many of the authenticated identities are
// still not connected, how long the oldest has waited, and a bounded sample of
// their addresses. One line per tick replaces the one-warning-per-slot flood,
// and an unchanged count is repeated at most once per
// providerControlSummaryInterval, so a single stuck slot cannot print for the
// life of the process.
func (self *providerBringUpTracker) reportOverdue() {
	if self == nil {
		return
	}
	self.mu.Lock()
	if self.closed {
		self.mu.Unlock()
		return
	}
	now := time.Now()
	notConnected := 0
	distinct := 0
	oldestAge := time.Duration(0)
	sample := make([]string, 0, providerControlNameLimit)
	// Several slots can sit behind one proxy address, so a per-identity sample
	// repeats the same address and hides how broad the problem is. Count and
	// name each address once.
	seen := make(map[string]bool, len(self.identities))
	for index := range self.identities {
		if self.reached[index] || self.states[index] != "running" {
			continue
		}
		notConnected++
		name := strings.TrimPrefix(self.displays[index], "proxy:")
		if !seen[name] {
			seen[name] = true
			distinct++
			if len(sample) < providerControlNameLimit {
				sample = append(sample, name)
			}
		}
		if authAt, ok := self.authAt[index]; ok {
			if age := now.Sub(authAt); oldestAge < age {
				oldestAge = age
			}
		}
	}
	self.overdue = nil
	self.overdueSet = map[uint64]bool{}
	if notConnected == 0 {
		// A later wave must print even when it lands on the same count as the
		// wave before this quiet stretch.
		self.lastNotConnected = 0
		self.lastSummaryAt = time.Time{}
		self.mu.Unlock()
		return
	}
	since := now.Sub(self.lastSummaryAt)
	due := self.lastSummaryAt.IsZero() ||
		(notConnected != self.lastNotConnected && providerControlChangeGap <= since) ||
		providerControlSummaryInterval <= since
	self.lastNotConnected = notConnected
	if due {
		self.lastSummaryAt = now
	}
	identities := len(self.identities)
	self.mu.Unlock()

	if !due {
		return
	}
	tlog("⚠️ [control] %d of %d identities authenticated but not control-connected after %s; oldest %s; sample: %s\n",
		notConnected, identities, formatDuration(providerControlGrace), formatDuration(oldestAge), strings.Join(sample, ", "))
}

// controlReached records a slot's first successful dial through its proxy. The
// dial itself is already in the log as the library's [net][s]select line, which
// carries the slot label, the transport and the duration, so this adds no
// narration of its own; the tally and the health line report the aggregate.
func (self *providerBringUpTracker) controlReached(index uint64, display string, _ time.Duration, _ string) {
	if self == nil {
		return
	}
	self.mu.Lock()
	if self.reached[index] {
		self.mu.Unlock()
		return
	}
	self.reached[index] = true
	// Counted per proxy identity, not per slot index: a reload gives the same
	// proxy a new index, and the health line must not count it twice. The guard
	// above already returns early for an index that reached before, so this is
	// the belt to that braces.
	noteControlReachedIdentity(display)
	self.reachedIdentities[display] = true
	if watch := self.watches[index]; watch != nil {
		watch.Stop()
		delete(self.watches, index)
	}
	shouldEmit := self.finished
	self.mu.Unlock()
	if shouldEmit {
		self.emit()
	}
}

// authFailed narrates one slot's failed authentication, with the reason and
// what happens next (retry), and moves it to failed.
func (self *providerBringUpTracker) authFailed(index uint64, display string, reason error, retry string) {
	self.apply(index, "failed", func() {
		// Disabled creation is an operator decision, not a custody fault, and
		// it is the only refusal this path keeps: one plain line, no retry
		// hint, no recovery advice.
		if strings.Contains(reason.Error(), "client_registration_disabled") {
			tlog("[auth] provider %d skipped: provider client creation is disabled for this run (slot %s)\n", index, display)
			return
		}
		tlog("[auth] provider %d failed: %v (slot %s, retry %s)\n", index, reason, display, retry)
	})
}

// workerFailed records a worker that ended with an error after authentication
// (or before any auth line); the tally moves without a duplicate line.
func (self *providerBringUpTracker) workerFailed(index uint64) {
	self.apply(index, "failed", nil)
}

// slotStopped records a slot whose worker has exited without failing it. This
// is the reloader's removal and the run's own shutdown: the slot's context is
// cancelled, so provideWithProxy returns context.Canceled, and the tally used
// to keep the slot "running" until the run ended. A slot nobody serves must not
// be counted as running, nor reported by the client-id gauges that read the
// same states, so every exit path calls this, clean cancellation included.
func (self *providerBringUpTracker) slotStopped(index uint64) {
	if self == nil {
		return
	}
	self.mu.Lock()
	current := self.states[index]
	switch current {
	case "failed", "stopped":
		// Already out of the running tally: a failed slot is counted as a
		// failure and a stopped one as gone, and either would be counted twice
		// by moving it again.
		self.mu.Unlock()
		return
	case "running":
		self.running--
	}
	self.stopped++
	self.states[index] = "stopped"
	// A stop can be the last thing the run was waiting for, and no tally would
	// print it: apply moves the count, a stop only removes work, so the final
	// line is emitted from here too.
	final := self.pendingLocked() <= 0 && !self.finished
	if final {
		self.finished = true
	}
	reused, minted := self.runningIdentityCounts()
	line := providerBringUpLine(self.running, self.pendingLocked(), self.failed, reused, minted, len(self.reached))
	self.mu.Unlock()
	if final {
		tlog("%s\n", line)
		self.stop()
	}
}

// apply updates one slot under the lock, prints its line (when any), and emits
// the final tally exactly once when the last pending slot resolves.
func (self *providerBringUpTracker) apply(index uint64, state string, print func()) {
	if self == nil {
		return
	}
	self.mu.Lock()
	current, ok := self.states[index]
	if !ok {
		current = "pending"
	}
	if current != state {
		switch current {
		case "running":
			self.running--
		case "failed":
			self.failed--
		}
		switch state {
		case "running":
			self.running++
		case "failed":
			self.failed++
		}
		self.states[index] = state
	}
	if print != nil {
		print()
	}
	pending := self.pendingLocked()
	final := pending <= 0 && !self.finished
	if final {
		self.finished = true
	}
	reused, minted := self.runningIdentityCounts()
	line := providerBringUpLine(self.running, pending, self.failed, reused, minted, len(self.reached))
	self.mu.Unlock()
	if final {
		tlog("%s\n", line)
		// Every slot has resolved, so the identity counts are final and the one
		// replacement-wave decision this start gets can be made.
		self.emitReplacementWave()
		self.stop()
	}
}

// providerAuthGiveUpInfo is what a failed authentication recorded for the
// per-slot line: whether the identity gave up, whether it was evicted, and
// when it is eligible again.
type providerAuthGiveUpInfo struct {
	giveUp  int
	retryIn time.Duration
	evicted bool
}

// providerAuthRetryToken renders the retry half of a failed-auth line.
func providerAuthRetryToken(info providerAuthGiveUpInfo) string {
	switch {
	case info.evicted:
		return "never (evicted from the URL cache)"
	case 0 < info.retryIn:
		return "in " + formatDuration(info.retryIn)
	default:
		return "on the next start"
	}
}
