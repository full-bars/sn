package miner

// smart_dialer_summary.go is the reporting half of the smart dialer's
// schedule: what the probes found, per window, in one line.
//
// It is deliberately separate from smart_dialer_scheduler.go: the scheduler
// decides WHEN a path is probed, this decides what the operator is told about
// it. Rounds of different paths are not in step, so the window is wall-clock,
// not a round.
//
// The window's ticker belongs to the scheduler goroutine (run) rather than to
// a ticker of its own. The provider starts one summary ticker for the process
// lifetime, which is the same thing there because the provider's probing is
// never withdrawn; here probing is opt-in and can be turned off at runtime, so
// a ticker nobody stops would be a five-minute wakeup for a process that is
// not probing at all.

import (
	"fmt"
	"math/rand"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

const (
	smartDialerSummaryInterval = 5 * time.Minute
	// smartDialerSampleCap bounds the per-transport samples held for one
	// summary window; past it a new sample replaces a random old one.
	smartDialerSampleCap = 1024
	// smartDialerLeaderLogBurst caps how many leader-change lines one minute
	// may print; the summary counts all of them.
	smartDialerLeaderLogBurst = 20
)

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
	path       string
	identities int
	transport  string
	latency    time.Duration
}

// smartDialerSummary collects probe rounds across every path and renders one
// line per window.
type smartDialerSummary struct {
	mutex         sync.Mutex
	rounds        int
	paths         map[string]struct{}
	identities    map[string]int
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
		paths:      map[string]struct{}{},
		identities: map[string]int{},
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

// smartDialerProbeDetail is the provider's glog.V(2) gate for the per path
// detail line. The miner has no glog verbosity level of its own (the -v flag is
// the operator supervisor's), so it reads one environment variable instead of
// adding a flag to a CLI that already has more than it can explain.
var smartDialerProbeDetail = sync.OnceValue(func() bool {
	return os.Getenv("URNETWORK_SMART_DIALER_PROBE_DETAIL") != ""
})

// formatProbeRound is the per path detail line (verbose only): every
// transport's cost or outcome and the leader, with the path and how many
// identities share it. The label is host:port (or "direct"); never the proxy
// key, which carries the username.
func formatProbeRound(path string, identities int, measuredBy int, round connect.ProbeRound) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[smart-dialer] %s (%d identities, measured via proxy[%d])", path, identities, measuredBy)
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
func (self *smartDialerSummary) record(now time.Time, path string, identities int, round connect.ProbeRound) string {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	self.rounds += 1
	self.paths[path] = struct{}{}
	self.identities[path] = identities
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
				self.slowest = slowestProbe{path: path, identities: identities, transport: result.Transport, latency: result.Latency}
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
	return fmt.Sprintf("[smart-dialer] %s leader changed %s -> %s\n", path, round.PreviousLeader, round.Leader)
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

	identities := 0
	for _, count := range self.identities {
		identities += count
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[smart-dialer] last %s: %d rounds over %d paths (%d identities);", smartDialerSummaryInterval, self.rounds, len(self.paths), identities)
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
		fmt.Fprintf(&b, "; slowest %s (%d identities) %s=%dms", self.slowest.path, self.slowest.identities, self.slowest.transport, self.slowest.latency.Milliseconds())
	}
	if 0 < self.overloaded {
		fmt.Fprintf(&b, "; api_overloaded=%d", self.overloaded)
	}
	fmt.Fprintf(&b, "; deferred_for_traffic=%d\n", deferred)

	self.rounds = 0
	self.paths = map[string]struct{}{}
	self.identities = map[string]int{}
	self.transports = map[string]*transportAggregate{}
	self.leaders = map[string]int{}
	self.leaderChanges = 0
	self.leaderSuppress = 0
	self.slowest = slowestProbe{}
	self.overloaded = 0
	return b.String()
}

// smartDialerSummaryWindow closes one window: it renders and resets the
// aggregate line, and returns "" when nothing was probed in it.
//
// The deferral count is consumed only by a window that renders. A quiet window
// (every path busy serving clients, so no round finished) keeps its count:
// those gate calls really did wait for traffic, and dropping the number
// because nothing completed would lose the one figure that explains the quiet.
func smartDialerSummaryWindow() string {
	return globalSmartDialerScheduler.summaryWindow()
}

// logProbeRound prints the per path detail when the detail line is on, and the
// leader change line (rate limited) always.
func logProbeRound(path string, identities int, measuredBy int, round connect.ProbeRound) {
	if line := globalSmartDialerSummary.record(time.Now(), path, identities, round); line != "" {
		tlog("%s", line)
	}
	if smartDialerProbeDetail() {
		tlog("%s", formatProbeRound(path, identities, measuredBy, round))
	}
}
