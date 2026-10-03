package provider

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
)

// The smart dialer's probes are background work. Everything here exists so
// they can never cost auth or hot-restart bring-up anything:
//
//   - nothing probes until the process has been up smartDialerProbeStartupGrace
//     (a hot restart reuses client logins, so the auth gate alone can sit idle
//     while the pool is still dialing in);
//   - nothing probes unless the auth admission gate has been idle for
//     smartDialerProbeAuthQuiet, and the check is re-made after a slot is won,
//     so a probe that was waiting steps aside the moment auth work arrives;
//   - at most smartDialerProbeMaxInFlight probe connects run in the whole
//     process, at one per smartDialerProbeInterval on average;
//   - auth never takes a lock a probe holds, so a probe cannot make auth wait.
const (
	smartDialerProbeMaxInFlight  = 2
	smartDialerProbeInterval     = time.Second
	smartDialerProbeAuthQuiet    = 2 * time.Minute
	smartDialerProbeStartupGrace = 5 * time.Minute
	// smartDialerGatewayHold is how long one proxy owns the probing of an
	// address another proxy also uses. Gateways that front many proxies (the
	// session is in the username, so the egress rotates) share one host:port,
	// and a cost measured through one says nothing stable about the others.
	smartDialerGatewayHold = 2 * time.Hour
	// smartDialerRecheck is how often a waiting probe re-reads whether auth is
	// idle, jittered so a thousand waiters do not poll in step.
	smartDialerRecheckMin    = 2 * time.Second
	smartDialerRecheckJitter = 4 * time.Second

	smartDialerSummaryInterval = 5 * time.Minute
	// smartDialerSampleCap bounds the per-transport samples held for one
	// summary window; past it a new sample replaces a random old one.
	smartDialerSampleCap = 1024
	// smartDialerLeaderLogBurst caps how many leader-change lines one minute may
	// print; the summary counts all of them.
	smartDialerLeaderLogBurst = 20
)

// errSmartDialerGatewayShared ends a round for a proxy whose address another
// proxy already owns. It is not held against any transport.
var errSmartDialerGatewayShared = errors.New("smart dialer: address is probed through another proxy")

type gatewayOwner struct {
	owner string
	until time.Time
}

type smartDialerScheduler struct {
	start time.Time
	quiet func(time.Duration) bool
	now   func() time.Time
	// sleep waits d or until ctx ends
	sleep func(ctx context.Context, d time.Duration) error

	slots chan struct{}

	mutex    sync.Mutex
	nextTime time.Time
	gateways map[string]gatewayOwner

	// deferred counts gate calls that had to wait because auth was not idle
	deferred atomic.Int64
}

func newSmartDialerScheduler(start time.Time, quiet func(time.Duration) bool) *smartDialerScheduler {
	return &smartDialerScheduler{
		start:    start,
		quiet:    quiet,
		now:      time.Now,
		sleep:    sleepContext,
		slots:    make(chan struct{}, smartDialerProbeMaxInFlight),
		gateways: map[string]gatewayOwner{},
	}
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

var globalSmartDialerScheduler = newSmartDialerScheduler(providerStartTime, globalProxyAdmissionGate.QuietFor)

// claimGateway reports whether owner may probe through addr: the first proxy
// to ask owns it for smartDialerGatewayHold, renewed while it keeps probing.
func (self *smartDialerScheduler) claimGateway(addr string, owner string) bool {
	if addr == "" {
		return true
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	now := self.now()
	held, ok := self.gateways[addr]
	if ok && held.owner != owner && now.Before(held.until) {
		return false
	}
	self.gateways[addr] = gatewayOwner{owner: owner, until: now.Add(smartDialerGatewayHold)}
	return true
}

// ready reports whether probing is allowed right now.
func (self *smartDialerScheduler) ready() bool {
	return smartDialerProbeStartupGrace <= self.now().Sub(self.start) && self.quiet(smartDialerProbeAuthQuiet)
}

// takeToken waits for this process's turn to start one probe connect.
func (self *smartDialerScheduler) takeToken(ctx context.Context) error {
	self.mutex.Lock()
	now := self.now()
	at := self.nextTime
	if at.Before(now) {
		at = now
	}
	self.nextTime = at.Add(smartDialerProbeInterval)
	self.mutex.Unlock()
	if wait := at.Sub(now); 0 < wait {
		return self.sleep(ctx, wait)
	}
	return ctx.Err()
}

// Gate is the connect library's per connect hook: it blocks until probing is
// allowed and returns the release for the in-flight slot, or an error that
// ends the proxy's round.
func (self *smartDialerScheduler) Gate(ctx context.Context, addr string, owner string) (func(), error) {
	if !self.claimGateway(addr, owner) {
		return nil, errSmartDialerGatewayShared
	}
	waited := false
	for {
		if !self.ready() {
			if !waited {
				waited = true
				self.deferred.Add(1)
			}
			d := smartDialerRecheckMin + time.Duration(rand.Int63n(int64(smartDialerRecheckJitter)))
			if err := self.sleep(ctx, d); err != nil {
				return nil, err
			}
			continue
		}
		select {
		case self.slots <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		release := func() { <-self.slots }
		// auth may have started while this waited for a slot: step aside
		if !self.ready() {
			release()
			continue
		}
		if err := self.takeToken(ctx); err != nil {
			release()
			return nil, err
		}
		if !self.ready() {
			release()
			continue
		}
		return release, nil
	}
}

// transportAggregate is one transport's results in a summary window.
type transportAggregate struct {
	samples []time.Duration
	seen    int
	ok      int
	fail    int
	blocked int
}

func (self *transportAggregate) add(latency time.Duration) {
	self.seen += 1
	if len(self.samples) < smartDialerSampleCap {
		self.samples = append(self.samples, latency)
	} else {
		self.samples[rand.Intn(len(self.samples))] = latency
	}
}

func (self *transportAggregate) median() time.Duration {
	if len(self.samples) == 0 {
		return 0
	}
	sorted := slices.Clone(self.samples)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}

type slowestProbe struct {
	index     int
	addr      string
	transport string
	latency   time.Duration
}

// smartDialerSummary collects probe rounds across every proxy and renders one
// line per window. Rounds of different proxies are not in step, so the window
// is wall-clock, not a round.
type smartDialerSummary struct {
	mutex         sync.Mutex
	rounds        int
	proxies       map[int]struct{}
	transports    map[string]*transportAggregate
	leaders       map[string]int
	leaderChanges int
	slowest       slowestProbe
	overloaded    int

	leaderLogWindow time.Time
	leaderLogCount  int
	leaderSuppress  int
}

func newSmartDialerSummary() *smartDialerSummary {
	return &smartDialerSummary{
		proxies:    map[int]struct{}{},
		transports: map[string]*transportAggregate{},
		leaders:    map[string]int{},
	}
}

var globalSmartDialerSummary = newSmartDialerSummary()

func formatProbeLatency(result connect.ProbeTransportResult) string {
	switch result.Status {
	case "ok":
		return fmt.Sprintf("%dms", result.Latency.Milliseconds())
	case "skipped":
		return "skip"
	default:
		return result.Status
	}
}

// formatProbeRound is the per proxy detail line (verbose only): every
// transport's cost or outcome and the leader, with the proxy's index and
// address. The address is host:port; never the proxy key, which carries the
// username.
func formatProbeRound(index int, addr string, round connect.ProbeRound) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[smart-dialer] proxy[%d] (%s)", index, addr)
	for _, result := range round.Results {
		fmt.Fprintf(&b, " %s=%s", result.Transport, formatProbeLatency(result))
	}
	switch {
	case round.Overloaded:
		b.WriteString(" api-overloaded")
	case round.Leader == "":
		b.WriteString(" leader=none")
	case round.PreviousLeader != "" && round.PreviousLeader != round.Leader:
		fmt.Fprintf(&b, " leader=%s->%s", round.PreviousLeader, round.Leader)
	default:
		fmt.Fprintf(&b, " leader=%s", round.Leader)
	}
	b.WriteString("\n")
	return b.String()
}

// record folds one finished round in and returns the leader-change line to
// print now, if any (rate limited; the summary counts every change).
func (self *smartDialerSummary) record(now time.Time, index int, addr string, round connect.ProbeRound) string {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.rounds += 1
	self.proxies[index] = struct{}{}
	if round.Overloaded {
		self.overloaded += 1
	}
	for _, result := range round.Results {
		aggregate := self.transports[result.Transport]
		if aggregate == nil {
			aggregate = &transportAggregate{}
			self.transports[result.Transport] = aggregate
		}
		switch result.Status {
		case "ok":
			aggregate.ok += 1
			aggregate.add(result.Latency)
			if self.slowest.latency < result.Latency {
				self.slowest = slowestProbe{index: index, addr: addr, transport: result.Transport, latency: result.Latency}
			}
		case "fail":
			aggregate.fail += 1
		case "blocked":
			aggregate.blocked += 1
		}
	}
	if round.Leader != "" {
		self.leaders[round.Leader] += 1
	}
	if round.PreviousLeader == "" || round.Leader == "" || round.PreviousLeader == round.Leader {
		return ""
	}
	self.leaderChanges += 1
	if 1*time.Minute <= now.Sub(self.leaderLogWindow) {
		self.leaderLogWindow = now
		self.leaderLogCount = 0
	}
	if smartDialerLeaderLogBurst <= self.leaderLogCount {
		self.leaderSuppress += 1
		return ""
	}
	self.leaderLogCount += 1
	return fmt.Sprintf("[smart-dialer] proxy[%d] (%s) leader changed %s -> %s\n", index, addr, round.PreviousLeader, round.Leader)
}

// flush renders and resets the window. It returns "" when nothing was probed.
func (self *smartDialerSummary) flush(deferred int64) string {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.rounds == 0 {
		self.leaderSuppress = 0
		return ""
	}

	names := make([]string, 0, len(self.transports))
	for name := range self.transports {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	fmt.Fprintf(&b, "[smart-dialer] last %s: %d rounds over %d proxies;", smartDialerSummaryInterval, self.rounds, len(self.proxies))
	for _, name := range names {
		aggregate := self.transports[name]
		fmt.Fprintf(&b, " %s median=%dms ok=%d fail=%d blocked=%d;", name, aggregate.median().Milliseconds(), aggregate.ok, aggregate.fail, aggregate.blocked)
	}
	leaders := make([]string, 0, len(self.leaders))
	for name := range self.leaders {
		leaders = append(leaders, name)
	}
	sort.Strings(leaders)
	b.WriteString(" leader")
	for _, name := range leaders {
		fmt.Fprintf(&b, " %s=%d", name, self.leaders[name])
	}
	fmt.Fprintf(&b, "; leader_changes=%d", self.leaderChanges)
	if 0 < self.leaderSuppress {
		fmt.Fprintf(&b, " (%d change lines suppressed)", self.leaderSuppress)
	}
	if 0 < self.slowest.latency {
		fmt.Fprintf(&b, "; slowest proxy[%d] (%s) %s=%dms", self.slowest.index, self.slowest.addr, self.slowest.transport, self.slowest.latency.Milliseconds())
	}
	if 0 < self.overloaded {
		fmt.Fprintf(&b, "; api_overloaded=%d", self.overloaded)
	}
	fmt.Fprintf(&b, "; deferred_for_auth=%d\n", deferred)

	self.rounds = 0
	self.proxies = map[int]struct{}{}
	self.transports = map[string]*transportAggregate{}
	self.leaders = map[string]int{}
	self.leaderChanges = 0
	self.leaderSuppress = 0
	self.slowest = slowestProbe{}
	self.overloaded = 0
	return b.String()
}

var smartDialerSummaryOnce sync.Once

// startSmartDialerSummary prints the aggregate line once per window, only when
// something was probed in it. Idempotent; the first probing proxy starts it.
func startSmartDialerSummary() {
	smartDialerSummaryOnce.Do(func() {
		go connect.HandleError(func() {
			ticker := time.NewTicker(smartDialerSummaryInterval)
			defer ticker.Stop()
			for range ticker.C {
				deferred := globalSmartDialerScheduler.deferred.Swap(0)
				if line := globalSmartDialerSummary.flush(deferred); line != "" {
					tlog("%s", line)
				}
			}
		})
	})
}

// logProbeRound prints the per proxy detail at verbosity 2 and the leader
// change line (rate limited) always.
func logProbeRound(index int, addr string, round connect.ProbeRound) {
	if line := globalSmartDialerSummary.record(time.Now(), index, addr, round); line != "" {
		tlog("%s", line)
	}
	if glog.V(2) {
		tlog("%s", formatProbeRound(index, addr, round))
	}
}
