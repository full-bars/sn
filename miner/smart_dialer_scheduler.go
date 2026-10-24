package miner

// smart_dialer_scheduler.go replaces the provider's per-identity probe loops
// (provider/smart_dialer_probe.go) with ONE scheduler goroutine over EGRESS
// PATHS.
//
// The smart dialer (connect's measured-cost transport preference, toggled by
// the `smart_dialer` control key) only ranks transports it has MEASURED, and
// the first-choice transport wins wherever it works, so the alternatives are
// never dialed and never measured. Something has to dial them periodically.
//
// The provider does that with one goroutine, one context, one timer and one
// summary registration per identity, started inside the slot's goroutine after
// that identity authenticated. In the provider that is a fixed cost; in the
// miner it is not, and it has three consequences that this file is built to
// remove:
//
//  1. It cannot be turned on at runtime. The loops are created inside the slot
//     closure, so a slot that is already running has no reachable registry to
//     add itself to: `smart_dialer` had to be restart-required (it was
//     classified so in control_socket.go) because a live "on" would flip the
//     preference and then measure nothing for the life of the process.
//
//  2. It scales with identities, not with dial outcomes. A node running
//     thousands of identities behind a handful of gateways has thousands of
//     goroutines, timers and five-minute wakeups all measuring the same few
//     egress paths: the cost measured through one identity's session on a
//     gateway says the same thing as the next identity's on the same gateway.
//
//  3. It cannot be switched off cheaply. Each loop has to notice the
//     preference is gone at its next wake.
//
// The shape here is:
//
//   - ONE goroutine (`run`), started when the preference turns on and
//     cancelled and joined when it turns off. While the preference is off the
//     process has no scheduler goroutine, no timer, and no summary ticker.
//   - A due-time heap of PROBE TARGETS. A target is an egress path -- the
//     proxy address a slot dials through, or "direct" -- not an identity.
//     Identities register as members of their path (`Register`), and a path is
//     probed once no matter how many identities share it. The round's verdict
//     is recorded on the path and stamped on every member, so every identity
//     behind that egress shares it. Which member's strategy carries the round
//     rotates (the one measured longest ago), because connect keeps a
//     measurement inside the strategy that made it: one probe per path, and no
//     identity is permanently unmeasured either.
//   - A TRAFFIC-idle gate. A probe is a real connect on the path, competing
//     with the clients that path is serving, so a path is probed only after it
//     has carried no client session for smartDialerTrafficQuiet. This is
//     `proxyAdmissionGate.QuietFor`, which used to read the AUTH clock: an
//     auth-idle window is close to unreachable on a node where some identity
//     is always authenticating, so the probes it gated would never run, and
//     the resource a probe disturbs is the path's client traffic.
//   - A process-wide budget: the same in-flight cap and one-connect-a-second
//     pacing as before, plus a probes-per-hour ceiling, so a fleet that comes
//     due at once cannot spend the hour's probe budget in a minute.
//
// With one target per address the provider's two-hour GATEWAY HOLD disappears
// rather than being ported: the hold existed to keep two identities behind one
// host:port from measuring each other's egress, and here the address is the
// unit of measurement, so the path is measured once and neither identity
// measures the other.
//
// The ranking itself (which transport is fastest on a path) is connect's: the
// round reports an order and records it in the strategy that dialed.

import (
	"container/heap"
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
	"github.com/urnetwork/connect"
)

const (
	// smartDialerProbeStartupGrace is how long the process runs before any
	// probe may start. A hot restart reuses client logins, so the traffic gate
	// alone can look idle while the pool is still dialing in.
	smartDialerProbeStartupGrace = 5 * time.Minute
	// smartDialerTrafficQuiet is how long an egress path must have carried no
	// client session before a probe may use it.
	smartDialerTrafficQuiet = 2 * time.Minute
	// smartDialerProbeMaxInFlight is the process-wide cap on probe connects
	// that have been admitted but not finished.
	smartDialerProbeMaxInFlight = 2
	// smartDialerProbeInterval is the process-wide spacing of probe connect
	// starts: one a second, however many paths are due.
	smartDialerProbeInterval = time.Second
	// smartDialerProbeMaxPerHour caps probe connects in a fixed one-hour
	// window. The pacing already bounds the rate; this bounds the hour, so a
	// fleet that all comes due at once cannot spend every wakeup it has.
	smartDialerProbeMaxPerHour = 1200
	// smartDialerPathInterval is how often a path's round is re-run. connect
	// keeps its own, longer refresh age (30m, doubling to 24h while the
	// ranking is stable) and a call with nothing due does no network work, so
	// this can be short and still cheap.
	smartDialerPathInterval = 5 * time.Minute
	// smartDialerFirstRoundSpread is the window the FIRST round of every path
	// is spread over, so a fleet that starts together does not put every path
	// on the wire in the same instant.
	smartDialerFirstRoundSpread = smartDialerProbeStartupGrace
	// smartDialerLiveEnableSpread is the first-round spread for a path
	// registered by a running process (the preference was turned on at
	// runtime): there is no bring-up to protect, so the first probe is only
	// staggered enough to keep one enable from queueing the whole fleet.
	smartDialerLiveEnableSpread = 5 * time.Second
	// smartDialerRecheckMin and smartDialerRecheckJitter bound how long a path
	// that was held off (traffic busy, budget spent) waits before asking
	// again. The jitter keeps a thousand waiters from polling in step.
	smartDialerRecheckMin    = 2 * time.Second
	smartDialerRecheckJitter = 4 * time.Second
	// smartDialerStopTimeout bounds the join in Stop. A cancel reaches a probe
	// through its context, but a transport connect already in the kernel does
	// not have to return from it, and Stop is called from the control socket:
	// one stuck dial must not be able to hold a control command open forever.
	smartDialerStopTimeout = 5 * time.Second
)

// errSmartDialerPathBusy ends a round because the path started serving
// clients (or never stopped). It is not a verdict on any transport: the round
// is simply not allowed to dial now, and the path is rescheduled.
var errSmartDialerPathBusy = fmt.Errorf("smart dialer: the egress path is carrying client traffic")

// errSmartDialerBudget ends a round that has spent the hour's probe budget. It
// is not a verdict on any transport.
var errSmartDialerBudget = fmt.Errorf("smart dialer: probe budget for this hour is spent")

// smartDialerPath names an egress path.
//
// Address is the proxy's host:port, or "" for the direct path (the host's own
// egress). ASN is the second half of the identity of a path -- two addresses
// in one network usually have one dial outcome -- and is left empty: nothing
// in the miner resolves an address to an ASN today, and inventing a lookup
// would put a resolver in the probe path. It is part of the key so that the
// day an existing state does carry it, dedup gets finer without reshaping the
// registry.
type smartDialerPath struct {
	address string
	asn     string
}

func (self smartDialerPath) label() string {
	if self.address == "" {
		return "direct"
	}
	if self.asn == "" {
		return self.address
	}
	return self.address + " as" + self.asn
}

// smartDialerMember is one identity registered on a path: the strategy the
// round dials with (the slot's own, so the measurement and the dial are the
// same dialer set) and the bandwidth counter that says whether this identity
// is serving clients.
type smartDialerMember struct {
	strategy *connect.ClientStrategy
	apiUrl   string
	index    int
	bw       *bandwidth.ProxyBandwidth
	viaSocks bool
	// measuredAt is when a round last ran on THIS member's strategy, and
	// verdictAt when the path's verdict last reached it (they differ: a
	// verdict is shared by every member of the path, a measurement only
	// lands in the strategy that dialed).
	measuredAt time.Time
	verdictAt  time.Time
	rounds     int
	// verdict is the path's last ranking as this identity sees it.
	verdict smartDialerPathVerdict
}

// smartDialerPathVerdict is what a path's last round found, kept for every
// identity that shares the path.
type smartDialerPathVerdict struct {
	at             time.Time
	leader         string
	previousLeader string
	results        []connect.ProbeTransportResult
	overloaded     bool
}

func (self smartDialerPathVerdict) leaderChanged() bool {
	return self.previousLeader != "" && self.leader != "" && self.previousLeader != self.leader
}

// smartDialerTarget is one egress path and everyone probing through it.
type smartDialerTarget struct {
	path    smartDialerPath
	members []*smartDialerMember

	due       time.Time
	heapIndex int

	verdict smartDialerPathVerdict
	rounds  int
	// held counts rounds that had to wait for the path to go quiet.
	held int
}

func (self *smartDialerTarget) counters() []*bandwidth.ProxyBandwidth {
	counters := make([]*bandwidth.ProxyBandwidth, 0, len(self.members))
	for _, member := range self.members {
		counters = append(counters, member.bw)
	}
	return counters
}

// nextMember picks the member whose own strategy has gone longest without a
// measurement, so one probe per path still covers every identity on it over
// time. A path with one identity always probes that identity.
func (self *smartDialerTarget) nextMember() *smartDialerMember {
	var next *smartDialerMember
	for _, member := range self.members {
		if next == nil || member.measuredAt.Before(next.measuredAt) {
			next = member
		}
	}
	return next
}

// applyVerdict stamps a finished round on every identity that shares the path.
//
// connect keeps a measurement inside the strategy that made it and exposes no
// way to inject one into another strategy, so what every member gets here is
// the RANKING (which transport leads this path, which are dead on it) and the
// rounds counter; the sample itself lives in the strategy that dialed, and a
// member's own dialer set is measured when the rotation reaches it. What this
// buys is the thing the redesign is for: one probe per egress path, and every
// identity behind that egress knowing the path's answer.
func (self *smartDialerTarget) applyVerdict(member *smartDialerMember, verdict smartDialerPathVerdict) {
	self.verdict = verdict
	self.rounds += 1
	member.measuredAt = verdict.at
	member.rounds += 1
	for _, other := range self.members {
		other.verdict = verdict
		other.verdictAt = verdict.at
	}
}

// smartDialerDueHeap orders targets by due time, then by path label so two
// targets that come due in the same instant are always visited in one order
// (a map's iteration order would make the probe sequence different on every
// run, and the logs unreadable).
type smartDialerDueHeap []*smartDialerTarget

func (self smartDialerDueHeap) Len() int { return len(self) }

func (self smartDialerDueHeap) Less(i int, j int) bool {
	if !self[i].due.Equal(self[j].due) {
		return self[i].due.Before(self[j].due)
	}
	return self[i].path.label() < self[j].path.label()
}

func (self smartDialerDueHeap) Swap(i int, j int) {
	self[i], self[j] = self[j], self[i]
	self[i].heapIndex = i
	self[j].heapIndex = j
}

func (self *smartDialerDueHeap) Push(x any) {
	target := x.(*smartDialerTarget)
	target.heapIndex = len(*self)
	*self = append(*self, target)
}

func (self *smartDialerDueHeap) Pop() any {
	old := *self
	n := len(old)
	target := old[n-1]
	old[n-1] = nil
	target.heapIndex = -1
	*self = old[:n-1]
	return target
}

// smartDialerConfig is the schedule's knobs. The process uses the consts
// above; tests build a scheduler with a fast config instead of waiting out
// five-minute windows.
type smartDialerConfig struct {
	startupGrace     time.Duration
	trafficQuiet     time.Duration
	interval         time.Duration
	maxInFlight      int
	pathInterval     time.Duration
	maxProbesPerHour int
	firstRoundSpread time.Duration
	recheckMin       time.Duration
	recheckJitter    time.Duration
	summaryInterval  time.Duration
}

func defaultSmartDialerConfig() smartDialerConfig {
	return smartDialerConfig{
		startupGrace:     smartDialerProbeStartupGrace,
		trafficQuiet:     smartDialerTrafficQuiet,
		interval:         smartDialerProbeInterval,
		maxInFlight:      smartDialerProbeMaxInFlight,
		pathInterval:     smartDialerPathInterval,
		maxProbesPerHour: smartDialerProbeMaxPerHour,
		firstRoundSpread: smartDialerFirstRoundSpread,
		recheckMin:       smartDialerRecheckMin,
		recheckJitter:    smartDialerRecheckJitter,
		summaryInterval:  smartDialerSummaryInterval,
	}
}

// smartDialerScheduler owns the paths, the due heap, the probe budget and the
// goroutine that walks them.
type smartDialerScheduler struct {
	config    smartDialerConfig
	startTime time.Time
	now       func() time.Time
	// sleep waits d or until ctx ends; it is a field so the budget tests can
	// advance a fake clock instead of waiting out a second per connect.
	sleep func(ctx context.Context, d time.Duration) error
	// probe runs one round on one member's strategy; a field so the dedup and
	// budget tests can count rounds without a network dial.
	probe func(ctx context.Context, target *smartDialerTarget, member *smartDialerMember) int

	// slots caps probe connects in flight. With one dispatcher the cap is an
	// upper bound rather than the usual concurrency, but it is kept: it is the
	// thing that keeps a second dispatcher (or a future worker pool) from
	// exceeding the process budget.
	slots chan struct{}

	mutex sync.Mutex
	// targets is the registry by path. A path exists while some identity is
	// registered on it.
	targets map[smartDialerPath]*smartDialerTarget
	due     smartDialerDueHeap
	// nextConnect is when the next probe connect may start (the pacing).
	nextConnect time.Time
	// hourStart and hourProbes are the fixed one-hour probe window.
	hourStart  time.Time
	hourProbes int
	// roundMu is held for the length of a round, so Unregister can wait for a
	// round that is dialing with the strategy the slot is about to close.
	roundMu sync.Mutex

	// wake nudges the loop when a target is registered or rescheduled.
	wake chan struct{}

	lifeMutex sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	// stopping is true between the cancel and the end of the join in Stop, so
	// a Start that finds a live cancel can say why it refused instead of
	// reporting a scheduler that is not actually probing.
	stopping bool
	// stopTimeout bounds the join in Stop. A field so a test can watch the
	// bound without waiting out the process window.
	stopTimeout time.Duration

	// deferred counts rounds that had to wait for their path to go quiet.
	deferred atomic.Int64
	// probes counts probe connects admitted in this process's life.
	probes atomic.Int64
}

func newSmartDialerScheduler(config smartDialerConfig, startTime time.Time) *smartDialerScheduler {
	self := &smartDialerScheduler{
		config:      config,
		startTime:   startTime,
		now:         time.Now,
		sleep:       sleepContext,
		slots:       make(chan struct{}, config.maxInFlight),
		targets:     map[smartDialerPath]*smartDialerTarget{},
		wake:        make(chan struct{}, 1),
		stopTimeout: smartDialerStopTimeout,
	}
	self.probe = self.probeWithConnect
	return self
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var globalSmartDialerScheduler = newSmartDialerScheduler(defaultSmartDialerConfig(), processStart)

// startSmartDialerScheduler starts the one scheduler goroutine if it is not
// already running, and reports whether this call started it. It is safe to
// call from the control socket, from the startup replay and from a test: the
// preference can be turned on in all three places and must never start two
// dispatchers.
func startSmartDialerScheduler() bool {
	return globalSmartDialerScheduler.Start()
}

// stopSmartDialerScheduler cancels the scheduler and joins it. A cancelling
// probe connect unwinds through the gate's context, so the join returns once
// the in-flight round has given up, not after its timeout.
func stopSmartDialerScheduler() {
	globalSmartDialerScheduler.Stop()
}

// smartDialerSchedulerRunning reports whether the dispatcher goroutine is up.
func smartDialerSchedulerRunning() bool {
	return globalSmartDialerScheduler.Running()
}

// startSmartDialerIdentity registers one authenticated identity as a member of
// its egress path and returns the func that withdraws it.
//
// addr is the proxy's host:port, or "" for the direct path. The unregister
// func waits for any round that is dialing with this identity's strategy, so
// the caller can close the strategy as soon as it returns.
func startSmartDialerIdentity(strategy *connect.ClientStrategy, bw *bandwidth.ProxyBandwidth, apiUrl string, index int, addr string, viaSocks bool) (stop func()) {
	member := &smartDialerMember{
		strategy: strategy,
		apiUrl:   apiUrl,
		index:    index,
		bw:       bw,
		viaSocks: viaSocks,
	}
	scheduler := globalSmartDialerScheduler
	target := scheduler.Register(smartDialerPath{address: addr}, member)
	var once sync.Once
	return func() {
		once.Do(func() {
			scheduler.Unregister(target, member)
		})
	}
}

// Register adds a member to the path's target, creating the target (and its
// first due time) if this is the path's first identity.
func (self *smartDialerScheduler) Register(path smartDialerPath, member *smartDialerMember) *smartDialerTarget {
	self.mutex.Lock()
	target := self.targets[path]
	created := target == nil
	if created {
		target = &smartDialerTarget{
			path:      path,
			heapIndex: -1,
			due:       self.firstDue(self.now()),
		}
		self.targets[path] = target
		heap.Push(&self.due, target)
	}
	target.members = append(target.members, member)
	self.mutex.Unlock()
	self.poke()
	return target
}

// Unregister removes a member from its target, dropping the target when the
// last identity on the path withdraws, and waits out a round that is dialing
// with it.
//
// The round lock comes FIRST, and the removal happens under it. A round selects
// its member and dials under roundMu, so taking it here both waits out a round
// that already captured this member and stops a new round from selecting it
// while it is being withdrawn. Removing first and waiting afterwards left the
// window the other way round: a round could read the member, the removal could
// complete (and the caller close the strategy), and only then the round took
// roundMu and dialed the closed strategy.
func (self *smartDialerScheduler) Unregister(target *smartDialerTarget, member *smartDialerMember) {
	if target == nil || member == nil {
		return
	}
	self.roundMu.Lock()
	defer self.roundMu.Unlock()
	self.mutex.Lock()
	// the trailing slots are cleared as well: the reslice below leaves the
	// withdrawn pointers in the backing array, and they would keep the strategy
	// and its counter reachable for the life of the target
	members := target.members[:0]
	for _, other := range target.members {
		if other != member {
			members = append(members, other)
		}
	}
	clear(target.members[len(members):])
	target.members = members
	if len(target.members) == 0 {
		if self.targets[target.path] == target {
			delete(self.targets, target.path)
		}
		if 0 <= target.heapIndex && target.heapIndex < len(self.due) && self.due[target.heapIndex] == target {
			heap.Remove(&self.due, target.heapIndex)
		}
	}
	self.mutex.Unlock()
}

// paths reports how many egress paths are registered.
func (self *smartDialerScheduler) paths() int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return len(self.targets)
}

// identities reports how many identities are registered across all paths.
func (self *smartDialerScheduler) identities() int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	count := 0
	for _, target := range self.targets {
		count += len(target.members)
	}
	return count
}

// firstDue is when a newly registered path probes for the first time.
//
// The process-wide startup grace gates the actual connect, so this only has to
// decide the ORDER the paths come due in. A fleet that starts together
// registers its paths in a burst; spreading the first round over the grace (or
// over the short live-enable window when the process is long past its grace)
// keeps the heap from opening with every path in the same instant, and the
// one-a-second pacing finishes the job from there.
func (self *smartDialerScheduler) firstDue(now time.Time) time.Time {
	at := self.startTime.Add(self.config.startupGrace)
	if now.After(at) {
		at = now
	}
	spread := self.config.firstRoundSpread
	if self.config.startupGrace < now.Sub(self.startTime) {
		spread = min(spread, smartDialerLiveEnableSpread)
	}
	if spread <= 0 {
		return at
	}
	return at.Add(time.Duration(rand.Int63n(int64(spread))))
}

// poke wakes the loop so a new or rescheduled target is picked up now rather
// than at the next timer expiry.
func (self *smartDialerScheduler) poke() {
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

// replaySmartDialerAtStartup puts the persisted measured-cost preference in
// force before any identity dials, and starts the scheduler when it is on.
//
// The key is live-applied, so this is not the only way it takes effect: it is
// what makes a restart keep the value an operator set. Without it a restart
// would silently return the process to the off default, and the paths would
// stay parked with a preference that consulted measurements nothing produced.
// The provider replays the same key at startup (applyPersistedRuntimeTuning).
func replaySmartDialerAtStartup(state *controlState) {
	if state == nil {
		return
	}
	value, ok := state.get("smart_dialer")
	if !ok {
		value = controlLiveDefaults["smart_dialer"]
	}
	enabled := strings.EqualFold(value, "on")
	previous := connect.SetSmartDialer(enabled)
	if previous != enabled {
		was := "off"
		if previous {
			was = "on"
		}
		tlog("⚙️ [control] smart_dialer=%s at startup (was %s)\n", value, was)
	}
	if enabled {
		// parked paths start probing as soon as identities register; the
		// startup grace in the scheduler keeps the first connect off the wire
		// until the pool has dialed in
		startSmartDialerScheduler()
	}
}

// Start launches the dispatcher goroutine unless it is already running.
//
// A start is also a fresh process budget: the pacing, the in-flight cap and the
// hour's probe count belong to one run of the scheduler, so turning the
// preference off and on again does not inherit an hour that a previous run
// already spent.
func (self *smartDialerScheduler) Start() bool {
	self.lifeMutex.Lock()
	defer self.lifeMutex.Unlock()
	if self.cancel != nil {
		if self.stopping {
			tlog("[smart-dialer] the previous scheduler run has not joined yet (a probe connect is still returning from its cancel): not starting a second dispatcher beside it\n")
		}
		return false
	}
	self.mutex.Lock()
	self.nextConnect = time.Time{}
	self.hourStart = time.Time{}
	self.hourProbes = 0
	self.mutex.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	self.cancel, self.done = cancel, done
	ref := newLoopRef("smart_dialer")
	go func() {
		defer close(done)
		superviseLoop(ctx, ref, func() { self.run(ctx) }, nil)
	}()
	return true
}

// Stop cancels the dispatcher and joins it, including any in-flight round.
//
// lifeMutex is held across the cancel AND the join. Clearing the cancel before
// the join let a Start that arrived while the first dispatcher was still
// exiting see no run at all and spawn a second one; and a second Stop that
// arrived in the same window returned without joining. The join is part of the
// lifecycle, so it belongs inside the same critical section.
//
// The join is bounded by stopTimeout. Stop is called from the control socket's
// live side effect (smart_dialer off), and a probe connect that does not
// return from its cancel would otherwise hold that command - and any write
// behind it - open indefinitely. When the bound is hit the cancel/done pair is
// left in place: the next Stop retries the join, and Start refuses to run a
// second dispatcher beside the one that is still going away.
func (self *smartDialerScheduler) Stop() {
	self.lifeMutex.Lock()
	defer self.lifeMutex.Unlock()
	if self.cancel == nil {
		return
	}
	cancel, done := self.cancel, self.done
	self.stopping = true
	defer func() { self.stopping = false }()
	cancel()
	timeout := self.stopTimeout
	if timeout <= 0 {
		timeout = smartDialerStopTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		// cleared only now, with the goroutine gone and the mutex still held:
		// between the cancel and this point a run is in flight and no Start
		// may see it as absent
		self.cancel, self.done = nil, nil
	case <-timer.C:
		tlog("⚠️ [smart-dialer] the probe scheduler did not join within %s: a probe connect is not returning from its cancel; the next stop retries the join\n", timeout)
	}
}

// Running reports whether the dispatcher goroutine is up.
func (self *smartDialerScheduler) Running() bool {
	self.lifeMutex.Lock()
	defer self.lifeMutex.Unlock()
	return self.cancel != nil
}

// run is the one scheduler goroutine: it takes due targets off the heap, runs
// their round, and sleeps until the next due time or summary window. It owns
// the summary window too, so stopping the scheduler stops that wakeup with it.
func (self *smartDialerScheduler) run(ctx context.Context) {
	summaryAt := self.now().Add(self.config.summaryInterval)
	for {
		now := self.now()
		if !now.Before(summaryAt) {
			if line := self.summaryWindow(); line != "" {
				tlog("%s", line)
			}
			summaryAt = now.Add(self.config.summaryInterval)
		}
		if target := self.takeDue(now); target != nil {
			self.probeTarget(ctx, target)
			if ctx.Err() != nil {
				return
			}
			continue
		}
		wait := self.wait(now, summaryAt)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-self.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// wait is how long the loop may sleep: until the next due target, or the next
// summary window, whichever is first.
func (self *smartDialerScheduler) wait(now time.Time, summaryAt time.Time) time.Duration {
	self.mutex.Lock()
	next := summaryAt
	if 0 < self.due.Len() {
		if due := self.due[0].due; due.Before(next) {
			next = due
		}
	}
	self.mutex.Unlock()
	if wait := next.Sub(now); 0 < wait {
		return wait
	}
	return 0
}

// takeDue pops the earliest target if it is due, and returns nil otherwise. A
// popped target is out of the heap until it is rescheduled, so a round can
// never be started twice for one due time.
func (self *smartDialerScheduler) takeDue(now time.Time) *smartDialerTarget {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.due.Len() == 0 {
		return nil
	}
	target := self.due[0]
	if now.Before(target.due) {
		return nil
	}
	heap.Pop(&self.due)
	return target
}

// reschedule puts a target back with its next due time.
func (self *smartDialerScheduler) reschedule(target *smartDialerTarget, at time.Time) {
	self.mutex.Lock()
	target.due = at
	if 0 <= target.heapIndex && target.heapIndex < len(self.due) && self.due[target.heapIndex] == target {
		heap.Fix(&self.due, target.heapIndex)
		self.mutex.Unlock()
		return
	}
	if self.targets[target.path] == target {
		heap.Push(&self.due, target)
	}
	self.mutex.Unlock()
	self.poke()
}

// probeTarget runs (or defers) one path's round.
func (self *smartDialerScheduler) probeTarget(ctx context.Context, target *smartDialerTarget) {
	now := self.now()
	// Nothing probes inside the startup grace. A live enable is long past it,
	// which is what makes turning the key on probe immediately.
	if now.Before(self.startTime.Add(self.config.startupGrace)) {
		self.reschedule(target, self.startTime.Add(self.config.startupGrace))
		return
	}
	if self.budgetSpent(now) {
		self.reschedule(target, self.hourEnd(now))
		return
	}
	// The round lock is taken BEFORE the member is read, and held for the whole
	// round. Unregister withdraws a member under this same lock, so a strategy
	// that is picked here cannot be closed between the pick and the dial; it
	// used to be read under the registry mutex alone, and a withdrawal could
	// complete (and the caller close the strategy) before the round took
	// roundMu.
	self.roundMu.Lock()
	self.mutex.Lock()
	member := target.nextMember()
	heldBefore := target.held
	self.mutex.Unlock()
	if member == nil {
		self.roundMu.Unlock()
		self.reschedule(target, now.Add(self.config.pathInterval))
		return
	}
	self.probe(ctx, target, member)
	self.roundMu.Unlock()
	if ctx.Err() != nil {
		// The scheduler is being stopped: the round ended because of the
		// cancel. The target was popped to be probed, so it must go back into
		// the heap even though this run is over: the next Start probes from
		// the heap, and a popped-then-forgotten path would never probe again.
		self.reschedule(target, self.now())
		return
	}
	if self.budgetSpent(self.now()) {
		// the round spent the hour's last probe: wait for the next window
		self.reschedule(target, self.hourEnd(self.now()))
		return
	}
	if self.held(target) != heldBefore {
		// the path is serving clients: ask again when the quiet window it is
		// waiting on could actually have elapsed, not every recheck tick
		self.deferred.Add(1)
		self.reschedule(target, self.now().Add(self.busyBackoff(target)))
		return
	}
	self.reschedule(target, now.Add(self.config.pathInterval))
}

// summaryWindow closes one summary window for THIS scheduler, keeping a quiet
// window's deferrals for the next window that renders.
func (self *smartDialerScheduler) summaryWindow() string {
	deferred := self.deferred.Swap(0)
	line := globalSmartDialerSummary.flush(deferred)
	if line == "" && 0 < deferred {
		self.deferred.Add(deferred)
	}
	return line
}

func (self *smartDialerScheduler) held(target *smartDialerTarget) int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return target.held
}

// probeWithConnect is one round on one member's strategy, gated per connect.
func (self *smartDialerScheduler) probeWithConnect(ctx context.Context, target *smartDialerTarget, member *smartDialerMember) int {
	if member.strategy == nil {
		return 0
	}
	options := connect.SmartDialerProbeOptions{
		// behind SOCKS several transports behave identically: the connection
		// is not a bare TCP connection there, so measuring the twins ranks two
		// copies of one behavior against noise
		CollapseSocksTwins: member.viaSocks,
		Gate: func(ctx context.Context) (func(), error) {
			return self.gate(ctx, target)
		},
		OnRound: func(round connect.ProbeRound) {
			self.record(target, member, round)
		},
	}
	return member.strategy.ProbeDialersWith(ctx, member.apiUrl, options)
}

// gate is the per-connect admission: the path has to be quiet, a process-wide
// in-flight slot has to be free, and this connect's turn in the one-a-second
// pacing has to come.
//
// A busy path ends the round instead of waiting inside it. The provider's per
// identity loops could afford to sleep here (each one blocked only itself),
// but this gate runs inside the one dispatcher, so sleeping would hold every
// other path's round behind a path that is busy serving clients. Ending the
// round and rescheduling the path costs one connect attempt and keeps the
// dispatcher free.
func (self *smartDialerScheduler) gate(ctx context.Context, target *smartDialerTarget) (func(), error) {
	if !self.pathQuiet(target) {
		self.noteHeld(target)
		return nil, errSmartDialerPathBusy
	}
	select {
	case self.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release := func() { <-self.slots }
	// traffic can start while this waited for a slot: step aside
	if !self.pathQuiet(target) {
		release()
		self.noteHeld(target)
		return nil, errSmartDialerPathBusy
	}
	if err := self.takeToken(ctx, target); err != nil {
		release()
		return nil, err
	}
	// and it can start while this waited for its turn in the pacing
	if !self.pathQuiet(target) {
		release()
		self.noteHeld(target)
		return nil, errSmartDialerPathBusy
	}
	return release, nil
}

// pathQuiet reports whether every identity on the path has been serving no
// clients for the traffic window.
func (self *smartDialerScheduler) pathQuiet(target *smartDialerTarget) bool {
	self.mutex.Lock()
	counters := target.counters()
	self.mutex.Unlock()
	return globalProxyAdmissionGate.QuietFor(counters, self.config.trafficQuiet)
}

// takeToken waits for this process's turn to start one probe connect, and
// spends one of the hour's probes.
func (self *smartDialerScheduler) takeToken(ctx context.Context, target *smartDialerTarget) error {
	self.mutex.Lock()
	now := self.now()
	if self.hourStart.IsZero() || !now.Before(self.hourStart.Add(time.Hour)) {
		self.hourStart = now
		self.hourProbes = 0
	}
	if self.config.maxProbesPerHour <= self.hourProbes {
		self.mutex.Unlock()
		return errSmartDialerBudget
	}
	at := self.nextConnect
	if at.Before(now) {
		at = now
	}
	self.nextConnect = at.Add(self.config.interval)
	self.hourProbes += 1
	budgetLeft := self.config.maxProbesPerHour - self.hourProbes
	self.mutex.Unlock()
	if budgetLeft == 0 {
		// the round can finish its current connect, but say so: the next
		// Gate call fails with errSmartDialerBudget and the path is rescheduled
		// for the next window
		tlog("[smart-dialer] %d probes spent this hour: pausing probes until %s\n",
			self.config.maxProbesPerHour, self.hourEnd(now).Format(time.RFC3339))
	}
	if wait := at.Sub(now); 0 < wait {
		if err := self.sleep(ctx, wait); err != nil {
			return err
		}
	}
	// counted once the connect is really starting: a turn that a cancel took
	// away never put anything on the wire, so it must not read as a probe
	self.probes.Add(1)
	return ctx.Err()
}

// budgetSpent reports whether the hour's probes are spent.
func (self *smartDialerScheduler) budgetSpent(now time.Time) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.hourStart.IsZero() || !now.Before(self.hourStart.Add(time.Hour)) {
		return false
	}
	return self.config.maxProbesPerHour <= self.hourProbes
}

// hourEnd is when the fixed one-hour probe window that contains now closes. It
// takes the mutex, so it must not be called from a locked section (takeToken and
// budgetSpent read hourStart under the lock they already hold).
func (self *smartDialerScheduler) hourEnd(now time.Time) time.Time {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.hourStart.IsZero() {
		return now
	}
	return self.hourStart.Add(time.Hour)
}

func (self *smartDialerScheduler) recheck() time.Duration {
	return self.config.recheckMin + time.Duration(rand.Int63n(int64(self.config.recheckJitter)))
}

// busyBackoff is how long a path that is carrying clients waits before its next
// round. The gate holds a round off for the TRAFFIC-QUIET window, so a recheck
// of a few seconds cannot find the path quiet and only spins: the wait is the
// remainder of that window (TrafficIdleFor is negative while a session is open,
// which reads as the whole window from now), with the usual jitter so a
// thousand waiters do not poll in step.
func (self *smartDialerScheduler) busyBackoff(target *smartDialerTarget) time.Duration {
	self.mutex.Lock()
	counters := target.counters()
	self.mutex.Unlock()
	wait := self.config.trafficQuiet
	for _, bw := range counters {
		if bw == nil {
			continue
		}
		if remaining := self.config.trafficQuiet - bw.TrafficIdleFor(); wait < remaining {
			wait = remaining
		}
	}
	if wait < self.config.recheckMin {
		wait = self.config.recheckMin
	}
	return wait + time.Duration(rand.Int63n(int64(self.config.recheckJitter)))
}

func (self *smartDialerScheduler) noteHeld(target *smartDialerTarget) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	target.held += 1
}

// record folds a finished round into the path, stamps every identity on the
// path with the verdict, and logs it once for the path.
func (self *smartDialerScheduler) record(target *smartDialerTarget, member *smartDialerMember, round connect.ProbeRound) {
	verdict := smartDialerPathVerdict{
		at:             self.now(),
		leader:         round.Leader,
		previousLeader: round.PreviousLeader,
		results:        round.Results,
		overloaded:     round.Overloaded,
	}
	self.mutex.Lock()
	target.applyVerdict(member, verdict)
	identities := len(target.members)
	self.mutex.Unlock()
	logProbeRound(target.path.label(), identities, member.index, round)
}

// smartDialerNote renders the smart dialer's state for the snapshot's
// miner_notes: whether the scheduler is up and what it is watching. It is the
// outside check on the live toggle -- an operator who just set the key can see
// the dispatcher and the path count without a restart and without log access.
func smartDialerNote() string {
	if !connect.SmartDialerEnabled() && !smartDialerSchedulerRunning() {
		return "smart dialer: off (no probe scheduler running)"
	}
	scheduler := globalSmartDialerScheduler
	state := "running"
	if !smartDialerSchedulerRunning() {
		state = "stopped (preference on, nothing probing)"
	}
	return fmt.Sprintf("smart dialer: %s, %d egress paths over %d identities, %d probes admitted this process",
		state, scheduler.paths(), scheduler.identities(), scheduler.probes.Load())
}
