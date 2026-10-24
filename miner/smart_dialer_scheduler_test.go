package miner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urfoundation/sn/provider/bandwidth"
	"github.com/urnetwork/connect"
)

// The scheduler redesign's tests, in the order the redesign matters:
//
//	live on  -> probing starts with no restart, for slots that are already up
//	live off -> in-flight probes are cancelled and the goroutine joined
//	off      -> no goroutine, no timer at all
//	dedup    -> N identities on one egress path are ONE probe, and the verdict
//	            reaches every one of them
//	traffic  -> a path serving clients is skipped, not waited on
//	budget   -> the hour's ceiling holds
//
// Every test runs a scheduler with millisecond knobs against the real loop, so
// what is proven is the loop's behaviour and not a mock of it.

func testSmartDialerConfig() smartDialerConfig {
	return smartDialerConfig{
		startupGrace:     0,
		trafficQuiet:     0,
		interval:         time.Millisecond,
		maxInFlight:      smartDialerProbeMaxInFlight,
		pathInterval:     5 * time.Millisecond,
		maxProbesPerHour: 1000,
		firstRoundSpread: 0,
		recheckMin:       time.Millisecond,
		recheckJitter:    time.Millisecond,
		summaryInterval:  time.Hour,
	}
}

// newTestSmartDialerScheduler installs a fast scheduler as the process's one
// scheduler (the control socket, the startup replay and the slots all reach for
// the global) and restores the previous one afterwards.
func newTestSmartDialerScheduler(t *testing.T, config smartDialerConfig) *smartDialerScheduler {
	t.Helper()
	previous := globalSmartDialerScheduler
	previousSummary := globalSmartDialerSummary
	scheduler := newSmartDialerScheduler(config, time.Now().Add(-time.Hour))
	globalSmartDialerScheduler = scheduler
	globalSmartDialerSummary = newSmartDialerSummary()
	t.Cleanup(func() {
		scheduler.Stop()
		globalSmartDialerScheduler = previous
		globalSmartDialerSummary = previousSummary
	})
	return scheduler
}

// testSmartDialerMember is one identity on a path: a counter (so the traffic
// gate has something to read) and, when a test wants the real round, a
// strategy.
func testSmartDialerMember(t *testing.T, bw *bandwidth.ProxyBandwidth, apiUrl string) *smartDialerMember {
	t.Helper()
	if bw == nil {
		bw = &bandwidth.ProxyBandwidth{}
	}
	return &smartDialerMember{
		bw:     bw,
		apiUrl: apiUrl,
		index:  0,
	}
}

// countingProbe replaces the connect round with a counter, so a test can watch
// the schedule without a network dial. It goes through the real gate first
// (pacing, in-flight cap, hour ceiling and the traffic check are all there), and
// records the verdict like the real round does.
func countingProbe(scheduler *smartDialerScheduler, probes *atomic.Int64, members *atomic.Int64) func(context.Context, *smartDialerTarget, *smartDialerMember) int {
	return func(ctx context.Context, target *smartDialerTarget, member *smartDialerMember) int {
		release, err := scheduler.gate(ctx, target)
		if err != nil {
			return 0
		}
		release()
		probes.Add(1)
		members.Add(int64(member.index + 1))
		scheduler.record(target, member, connect.ProbeRound{
			Leader: "normal",
			Results: []connect.ProbeTransportResult{
				{Transport: "normal", Latency: 40 * time.Millisecond, Status: "ok"},
			},
		})
		return 1
	}
}

// waitForNoSchedulerGoroutine waits until the dispatcher goroutine's frame is
// gone. Stop joins the goroutine, but the frame survives the join until the
// goroutine has actually returned to the scheduler, so a single check races.
func waitForNoSchedulerGoroutine(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for strings.Contains(goroutineStacks(), "miner.(*smartDialerScheduler).run") {
		if deadline.Before(time.Now()) {
			t.Fatal("the scheduler goroutine is still running after Stop joined it")
		}
		time.Sleep(time.Millisecond)
	}
}

// goroutineStacks returns every goroutine's stack. A frame names the miner
// function the goroutine is in, which is how these tests tell a started
// scheduler from an allocated-nothing one.
func goroutineStacks() string {
	buf := make([]byte, 4<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// THE HEADLINE: enabling at runtime starts probing, for a slot that is already
// running, with no restart. The identity registered while the preference was
// off, the socket set turns it on, and the next thing that happens is a probe.
func TestSmartDialerLiveEnableStartsProbingWithoutARestart(t *testing.T) {
	state := newAuditWiringState(t)
	previous := connect.SetSmartDialer(false)
	t.Cleanup(func() { connect.SetSmartDialer(previous) })

	scheduler := newTestSmartDialerScheduler(t, testSmartDialerConfig())
	var probes atomic.Int64
	var members atomic.Int64
	scheduler.probe = countingProbe(scheduler, &probes, &members)

	// a running slot: its identity is registered on its egress path
	scheduler.Register(smartDialerPath{address: "10.0.0.1:1080"}, testSmartDialerMember(t, nil, ""))
	time.Sleep(20 * time.Millisecond)
	if got := probes.Load(); got != 0 {
		t.Fatalf("%d probes ran while the preference was off", got)
	}

	resp := handleControlRequest(state, controlRequest{Cmd: "set", Key: "smart_dialer", Value: "on"})
	if !resp.OK {
		t.Fatalf("set smart_dialer on: %s", resp.Error)
	}
	if resp.Effect != controlEffectLive || resp.NeedsRestart {
		t.Fatalf("set smart_dialer on reported effect=%q needs_restart=%v, want live/false", resp.Effect, resp.NeedsRestart)
	}
	if !connect.SmartDialerEnabled() {
		t.Fatal("the set did not apply the preference to the running process")
	}
	if !scheduler.Running() {
		t.Fatal("the set did not start the probe scheduler")
	}
	waitFor(t, 5*time.Second, "a probe for the already-running identity", func() bool { return 0 < probes.Load() })

	// and the verdict reached the identity
	waitFor(t, 5*time.Second, "the path verdict on the identity", func() bool {
		scheduler.mutex.Lock()
		defer scheduler.mutex.Unlock()
		target := scheduler.targets[smartDialerPath{address: "10.0.0.1:1080"}]
		return target != nil && target.members[0].verdict.leader == "normal"
	})
}

// THE OTHER HALF: disabling at runtime cancels an in-flight probe and joins the
// scheduler, and enabling again starts it.
func TestSmartDialerLiveDisableCancelsAndJoinsThenEnableRestarts(t *testing.T) {
	state := newAuditWiringState(t)
	previous := connect.SetSmartDialer(false)
	t.Cleanup(func() { connect.SetSmartDialer(previous) })

	scheduler := newTestSmartDialerScheduler(t, testSmartDialerConfig())
	// a round that blocks until the scheduler is cancelled: this is an
	// in-flight probe, and only the cancel can end it
	started := make(chan struct{}, 4)
	scheduler.probe = func(ctx context.Context, target *smartDialerTarget, member *smartDialerMember) int {
		started <- struct{}{}
		<-ctx.Done()
		return 0
	}
	scheduler.Register(smartDialerPath{address: "10.0.0.2:1080"}, testSmartDialerMember(t, nil, ""))

	if resp := handleControlRequest(state, controlRequest{Cmd: "set", Key: "smart_dialer", Value: "on"}); !resp.OK || resp.Effect != controlEffectLive {
		t.Fatalf("set on = %+v, want ok and live", resp)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the scheduler never started a probe")
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		resp := handleControlRequest(state, controlRequest{Cmd: "set", Key: "smart_dialer", Value: "off"})
		if !resp.OK || resp.Effect != controlEffectLive {
			t.Errorf("set off = %+v, want ok and live", resp)
		}
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("disabling smart_dialer did not join the scheduler: the in-flight probe was not cancelled")
	}
	if connect.SmartDialerEnabled() {
		t.Fatal("the set did not withdraw the preference")
	}
	if scheduler.Running() {
		t.Fatal("the scheduler still reports running after the set off")
	}
	waitForNoSchedulerGoroutine(t)

	// enable again: the parked path probes again with no restart
	var probes atomic.Int64
	var members atomic.Int64
	scheduler.probe = countingProbe(scheduler, &probes, &members)
	if resp := handleControlRequest(state, controlRequest{Cmd: "set", Key: "smart_dialer", Value: "on"}); !resp.OK {
		t.Fatalf("second set on: %s", resp.Error)
	}
	waitFor(t, 5*time.Second, "a probe after re-enabling", func() bool { return 0 < probes.Load() })

	// clearing the key is the same stop, and restores the off default
	if resp := handleControlRequest(state, controlRequest{Cmd: "clear", Key: "smart_dialer"}); !resp.OK || resp.NeedsRestart || resp.Effect != controlEffectLive {
		t.Fatalf("clear = %+v, want ok, live, no restart", resp)
	}
	if connect.SmartDialerEnabled() {
		t.Fatal("a clear left the preference on")
	}
	if scheduler.Running() {
		t.Fatal("a clear left the scheduler running")
	}
}

// Off must cost nothing: no scheduler goroutine and no timer. The identity is
// registered (that is what makes a live enable possible) but nothing runs.
func TestSmartDialerSchedulerAllocatesNothingWhileOff(t *testing.T) {
	scheduler := newTestSmartDialerScheduler(t, testSmartDialerConfig())
	var probes atomic.Int64
	var members atomic.Int64
	scheduler.probe = countingProbe(scheduler, &probes, &members)
	for i := 0; i < 3; i++ {
		scheduler.Register(smartDialerPath{address: "10.0.0.9:1080"}, testSmartDialerMember(t, nil, ""))
	}
	if strings.Contains(goroutineStacks(), "miner.(*smartDialerScheduler).run") {
		t.Fatal("a scheduler goroutine runs while the preference is off")
	}
	time.Sleep(50 * time.Millisecond)
	if got := probes.Load(); got != 0 {
		t.Fatalf("%d probes ran while the preference was off", got)
	}
	if scheduler.Running() {
		t.Fatal("the scheduler reports running while the preference is off")
	}
}

// N identities behind one gateway are ONE probe. The scheduler's unit is the
// egress path, and the round's verdict is stamped on every identity that
// shares it.
func TestSmartDialerProbesOneRoundForManyIdentitiesOnOnePath(t *testing.T) {
	config := testSmartDialerConfig()
	// one due time per path in the window, so the count is rounds, not chances
	config.pathInterval = time.Hour
	scheduler := newTestSmartDialerScheduler(t, config)
	var probes atomic.Int64
	var memberSum atomic.Int64
	scheduler.probe = countingProbe(scheduler, &probes, &memberSum)

	const identities = 25
	const paths = 2
	for i := 0; i < identities; i++ {
		member := testSmartDialerMember(t, nil, "")
		member.index = i
		address := "10.1.0.1:1080"
		if i%paths != 0 {
			address = "10.1.0.2:1080"
		}
		scheduler.Register(smartDialerPath{address: address}, member)
	}
	if got := scheduler.identities(); got != identities {
		t.Fatalf("registered %d identities, want %d", got, identities)
	}
	if got := scheduler.paths(); got != paths {
		t.Fatalf("registered %d paths, want %d", got, paths)
	}

	scheduler.Start()
	waitFor(t, 5*time.Second, "a round on each path", func() bool { return int64(paths) <= probes.Load() })
	// nothing more is due for the rest of the window
	time.Sleep(50 * time.Millisecond)

	// each round is one probe, and a round is per path: with 25 identities and
	// 2 paths the probe count must be bounded by rounds, not by identities
	if got := probes.Load(); got == 0 {
		t.Fatal("no probe ran for either path")
	}
	// every path verdict carries the identity count, so the summary and the
	// control surface name the fan-out
	scheduler.mutex.Lock()
	for path, target := range scheduler.targets {
		if target.rounds == 0 {
			t.Errorf("path %s was never probed", path.label())
		}
		for _, member := range target.members {
			if member.verdict.leader != "normal" {
				t.Errorf("path %s member %d did not receive the path verdict", path.label(), member.index)
			}
		}
	}
	scheduler.mutex.Unlock()

	// one round per path, not one per identity
	if got := probes.Load(); got != int64(paths) {
		t.Fatalf("%d probes for %d identities on %d paths, want one per path (%d)", got, identities, paths, paths)
	}
}

// Rotation: one round per path, but the member that dials it changes, so every
// identity's own dialer set is measured over time instead of the first one
// forever.
func TestSmartDialerPathRoundRotatesTheMeasuringIdentity(t *testing.T) {
	scheduler := newTestSmartDialerScheduler(t, testSmartDialerConfig())
	var probes atomic.Int64
	var memberSum atomic.Int64
	scheduler.probe = countingProbe(scheduler, &probes, &memberSum)

	for i := 0; i < 3; i++ {
		member := testSmartDialerMember(t, nil, "")
		member.index = i
		scheduler.Register(smartDialerPath{address: "10.2.0.1:1080"}, member)
	}
	scheduler.Start()
	waitFor(t, 5*time.Second, "three rounds on the path", func() bool { return 3 <= probes.Load() })

	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	measured := 0
	for _, member := range scheduler.targets[smartDialerPath{address: "10.2.0.1:1080"}].members {
		if member.rounds != 0 {
			measured += 1
		}
	}
	if measured < 2 {
		t.Fatalf("only %d of 3 identities ever measured: the round does not rotate", measured)
	}
}

// A path that is serving clients is skipped. The gate reads the counters'
// traffic clock, not the auth clock: an idleness window that a busy node never
// reaches would mean the probes never run at all.
func TestSmartDialerSkipsAPathServingClients(t *testing.T) {
	config := testSmartDialerConfig()
	config.trafficQuiet = 40 * time.Millisecond
	scheduler := newTestSmartDialerScheduler(t, config)
	var probes atomic.Int64
	var memberSum atomic.Int64
	scheduler.probe = countingProbe(scheduler, &probes, &memberSum)

	bw := &bandwidth.ProxyBandwidth{}
	member := testSmartDialerMember(t, bw, "")
	bw.AddSession("client", time.Now())
	scheduler.Register(smartDialerPath{address: "10.3.0.1:1080"}, member)

	scheduler.Start()
	time.Sleep(120 * time.Millisecond)
	if got := probes.Load(); got != 0 {
		t.Fatalf("%d probes ran on a path that is serving a client", got)
	}
	scheduler.mutex.Lock()
	held := scheduler.targets[smartDialerPath{address: "10.3.0.1:1080"}].held
	scheduler.mutex.Unlock()
	if held == 0 {
		t.Fatal("the round was not recorded as held for traffic")
	}

	// the client leaves: after the quiet window the path is probed
	bw.RemoveSession("client")
	waitFor(t, 5*time.Second, "a probe after the path went quiet", func() bool { return 0 < probes.Load() })
}

// A quiet path is probed even while another path is busy: idleness is a
// property of the path, so one busy egress cannot starve the rest.
func TestSmartDialerBusyPathDoesNotStarveAQuietOne(t *testing.T) {
	config := testSmartDialerConfig()
	config.trafficQuiet = 30 * time.Millisecond
	scheduler := newTestSmartDialerScheduler(t, config)
	var probes atomic.Int64
	var memberSum atomic.Int64
	scheduler.probe = countingProbe(scheduler, &probes, &memberSum)

	busyBw := &bandwidth.ProxyBandwidth{}
	busyBw.AddSession("client", time.Now())
	scheduler.Register(smartDialerPath{address: "10.4.0.1:1080"}, testSmartDialerMember(t, busyBw, ""))
	scheduler.Register(smartDialerPath{address: "10.4.0.2:1080"}, testSmartDialerMember(t, nil, ""))

	scheduler.Start()
	waitFor(t, 5*time.Second, "a probe on the quiet path", func() bool { return 0 < probes.Load() })
	scheduler.mutex.Lock()
	busy := scheduler.targets[smartDialerPath{address: "10.4.0.1:1080"}].rounds
	quiet := scheduler.targets[smartDialerPath{address: "10.4.0.2:1080"}].rounds
	scheduler.mutex.Unlock()
	if busy != 0 {
		t.Fatalf("the busy path ran %d rounds", busy)
	}
	if quiet == 0 {
		t.Fatal("the quiet path ran no rounds while the busy one was skipped")
	}
}

// The process-wide budget is unchanged where it matters: one connect a second,
// at most smartDialerProbeMaxInFlight at once, plus the hour's ceiling.
func TestSmartDialerGatePacesConnectsAndCapsTheHour(t *testing.T) {
	config := testSmartDialerConfig()
	config.interval = 10 * time.Millisecond
	config.maxProbesPerHour = 3
	scheduler := newTestSmartDialerScheduler(t, config)
	target := scheduler.Register(smartDialerPath{address: "10.5.0.1:1080"}, testSmartDialerMember(t, nil, ""))

	first := time.Now()
	for i := 0; i < config.maxProbesPerHour; i++ {
		release, err := scheduler.gate(context.Background(), target)
		if err != nil {
			t.Fatalf("gate %d: %v", i, err)
		}
		release()
	}
	// three connects are spaced at one interval each
	if elapsed := time.Since(first); elapsed < 2*config.interval {
		t.Fatalf("3 connects started in %v, want at least %v", elapsed, 2*config.interval)
	}
	// the fourth is refused: the hour's probes are spent
	if _, err := scheduler.gate(context.Background(), target); !errors.Is(err, errSmartDialerBudget) {
		t.Fatalf("gate after the hour's budget: err = %v, want errSmartDialerBudget", err)
	}
	if got := scheduler.probes.Load(); got != int64(config.maxProbesPerHour) {
		t.Fatalf("%d probe connects admitted, want the ceiling %d", got, config.maxProbesPerHour)
	}
	// and the scheduler does not run a round with no budget left
	if !scheduler.budgetSpent(time.Now()) {
		t.Fatal("the budget does not read as spent")
	}
}

// The in-flight cap still bounds concurrent connects.
func TestSmartDialerGateCapsInFlight(t *testing.T) {
	scheduler := newTestSmartDialerScheduler(t, testSmartDialerConfig())
	target := scheduler.Register(smartDialerPath{address: "10.5.0.2:1080"}, testSmartDialerMember(t, nil, ""))

	var releases []func()
	for i := 0; i < smartDialerProbeMaxInFlight; i++ {
		release, err := scheduler.gate(context.Background(), target)
		if err != nil {
			t.Fatalf("gate %d: %v", i, err)
		}
		releases = append(releases, release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := scheduler.gate(ctx, target); err == nil {
		t.Fatal("a third connect was admitted while the cap was full")
	}
	releases[0]()
	release, err := scheduler.gate(context.Background(), target)
	if err != nil {
		t.Fatalf("no slot after a release: %v", err)
	}
	release()
	for _, release := range releases[1:] {
		release()
	}
}

// Nothing probes inside the startup grace, and everything becomes eligible the
// moment it ends.
func TestSmartDialerSchedulerHoldsProbesForTheStartupGrace(t *testing.T) {
	config := testSmartDialerConfig()
	config.startupGrace = 150 * time.Millisecond
	config.firstRoundSpread = 0
	start := time.Now()
	scheduler := newSmartDialerScheduler(config, start)
	previousGlobal := globalSmartDialerScheduler
	globalSmartDialerScheduler = scheduler
	t.Cleanup(func() {
		scheduler.Stop()
		globalSmartDialerScheduler = previousGlobal
	})
	var probes atomic.Int64
	var members atomic.Int64
	scheduler.probe = countingProbe(scheduler, &probes, &members)
	scheduler.Register(smartDialerPath{address: "10.6.0.1:1080"}, testSmartDialerMember(t, nil, ""))
	scheduler.Start()

	time.Sleep(config.startupGrace / 2)
	if got := probes.Load(); got != 0 {
		t.Fatalf("%d probes ran inside the startup grace", got)
	}
	waitFor(t, 5*time.Second, "a probe after the startup grace", func() bool { return 0 < probes.Load() })
}

// The first round of every path is staggered instead of firing at boot: a fleet
// that starts together registers its paths in a burst, and the due times must
// spread over the window rather than pile up on one instant.
func TestSmartDialerFirstRoundIsStaggered(t *testing.T) {
	config := testSmartDialerConfig()
	config.startupGrace = 100 * time.Millisecond
	config.firstRoundSpread = 200 * time.Millisecond
	start := time.Now()
	scheduler := newSmartDialerScheduler(config, start)

	earliest, latest := time.Time{}, time.Time{}
	distinct := map[time.Time]struct{}{}
	for i := 0; i < 40; i++ {
		address := fmt.Sprintf("10.7.0.%d:1080", i)
		target := scheduler.Register(smartDialerPath{address: address}, testSmartDialerMember(t, nil, ""))
		due := target.due
		if earliest.IsZero() || due.Before(earliest) {
			earliest = due
		}
		if latest.IsZero() || latest.Before(due) {
			latest = due
		}
		distinct[due] = struct{}{}
	}
	graceEnd := start.Add(config.startupGrace)
	if earliest.Before(graceEnd) {
		t.Fatalf("a first round is due at %v, before the grace ends at %v", earliest.Sub(start), graceEnd.Sub(start))
	}
	if spread := latest.Sub(earliest); spread == 0 {
		t.Fatal("every path is due at the same instant: the first round is not staggered")
	}
	if 2 > len(distinct) {
		t.Fatalf("the first round has %d distinct due times", len(distinct))
	}
	if max := config.startupGrace + config.firstRoundSpread; latest.Sub(graceEnd) > max {
		t.Fatalf("a first round is due %v after the grace, past the %v window", latest.Sub(graceEnd), max)
	}
}

// The real round: connect measures every transport against the api host and
// the scheduler records the verdict on the path.
func TestSmartDialerRoundMeasuresAgainstTheApiAndRecordsTheVerdict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	previous := connect.SetSmartDialer(true)
	t.Cleanup(func() { connect.SetSmartDialer(previous) })

	config := testSmartDialerConfig()
	config.pathInterval = time.Second
	scheduler := newTestSmartDialerScheduler(t, config)
	strategy := connect.NewClientStrategy(context.Background(), connect.DefaultClientStrategySettings())
	t.Cleanup(strategy.Close)

	bw := &bandwidth.ProxyBandwidth{}
	scheduler.Register(smartDialerPath{address: "10.8.0.1:1080"}, &smartDialerMember{
		strategy: strategy,
		apiUrl:   server.URL,
		bw:       bw,
	})
	scheduler.Start()

	waitFor(t, 5*time.Second, "a recorded verdict", func() bool {
		scheduler.mutex.Lock()
		defer scheduler.mutex.Unlock()
		target := scheduler.targets[smartDialerPath{address: "10.8.0.1:1080"}]
		return target != nil && 0 < target.rounds
	})
	scheduler.mutex.Lock()
	target := scheduler.targets[smartDialerPath{address: "10.8.0.1:1080"}]
	verdict := target.verdict
	scheduler.mutex.Unlock()
	if len(verdict.results) == 0 {
		t.Fatal("the round reported no transports")
	}
	ok := 0
	for _, result := range verdict.results {
		if result.Status == "ok" {
			ok += 1
		}
	}
	if ok == 0 {
		t.Fatalf("no transport measured ok against a live server: %+v", verdict.results)
	}
	if verdict.leader == "" {
		t.Fatalf("the round produced no leader: %+v", verdict.results)
	}
}

// A slot that ends must not leave a round dialing with a closed strategy: the
// unregister waits the round out, and the path disappears with its last
// identity.
func TestSmartDialerUnregisterWaitsForTheRoundAndDropsThePath(t *testing.T) {
	scheduler := newTestSmartDialerScheduler(t, testSmartDialerConfig())
	inRound := make(chan struct{}, 1)
	finish := make(chan struct{})
	scheduler.probe = func(ctx context.Context, target *smartDialerTarget, member *smartDialerMember) int {
		inRound <- struct{}{}
		<-finish
		return 0
	}
	member := testSmartDialerMember(t, nil, "")
	scheduler.Register(smartDialerPath{address: "10.9.0.1:1080"}, member)
	scheduler.Start()
	select {
	case <-inRound:
	case <-time.After(3 * time.Second):
		t.Fatal("the round never started")
	}

	unregistered := make(chan struct{})
	go func() {
		defer close(unregistered)
		scheduler.Unregister(scheduler.targets[smartDialerPath{address: "10.9.0.1:1080"}], member)
	}()
	select {
	case <-unregistered:
		t.Fatal("unregister returned while a round was still dialing with the member")
	case <-time.After(30 * time.Millisecond):
	}
	close(finish)
	select {
	case <-unregistered:
	case <-time.After(3 * time.Second):
		t.Fatal("unregister did not return after the round finished")
	}
	if got := scheduler.paths(); got != 0 {
		t.Fatalf("%d paths left after the last identity withdrew, want 0", got)
	}
}

// A connect waiting for its turn in the pacing ends with its context, so the
// scheduler's Stop (which cancels the loop's context) never waits out a path
// interval or a pacing interval.
func TestSmartDialerGatePacingWaitIsCancellable(t *testing.T) {
	config := testSmartDialerConfig()
	config.interval = time.Hour
	scheduler := newTestSmartDialerScheduler(t, config)
	target := scheduler.Register(smartDialerPath{address: "10.10.0.1:1080"}, testSmartDialerMember(t, nil, ""))

	// the first connect takes the token at now; the next one is due in an hour
	release, err := scheduler.gate(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	if _, err := scheduler.gate(ctx, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled: the pacing wait must end with its context", err)
	}
	if elapsed := time.Since(started); 5*time.Second < elapsed {
		t.Fatalf("the pacing wait held for %v instead of ending with the cancel", elapsed)
	}
	if got := scheduler.probes.Load(); got != 1 {
		t.Fatalf("%d connects admitted, want only the one that got its turn", got)
	}
}

// The scale claim, measured: a 2000-identity node behind five gateways runs ONE
// dispatcher goroutine and probes FIVE paths. Before the redesign it ran 2000
// probe goroutines, 2000 contexts and 2000 five-minute timers -- and every one
// of them measured the same handful of egresses.
func TestSmartDialerGoroutinesDoNotScaleWithIdentities(t *testing.T) {
	config := testSmartDialerConfig()
	config.pathInterval = time.Hour
	scheduler := newTestSmartDialerScheduler(t, config)
	var probes atomic.Int64
	var memberSum atomic.Int64
	scheduler.probe = countingProbe(scheduler, &probes, &memberSum)

	const identities = 2000
	const paths = 5
	before := runtime.NumGoroutine()
	for i := 0; i < identities; i++ {
		member := testSmartDialerMember(t, nil, "")
		member.index = i
		scheduler.Register(smartDialerPath{address: fmt.Sprintf("10.20.%d.1:1080", i%paths)}, member)
	}
	if got := scheduler.identities(); got != identities {
		t.Fatalf("registered %d identities, want %d", got, identities)
	}
	if got := scheduler.paths(); got != paths {
		t.Fatalf("registered %d paths, want %d", got, paths)
	}

	scheduler.Start()
	waitFor(t, 10*time.Second, "one round on each path", func() bool { return int64(paths) <= probes.Load() })
	time.Sleep(20 * time.Millisecond)

	if got := probes.Load(); got != int64(paths) {
		t.Fatalf("%d probes for %d identities on %d paths, want %d", got, identities, paths, paths)
	}
	// the dispatcher (and connect's error handler around it), not one per identity
	if grew := runtime.NumGoroutine() - before; 10 < grew {
		t.Fatalf("goroutines grew by %d for %d identities: the schedule still scales with identities", grew, identities)
	}
	for _, frame := range []string{"miner.(*smartDialerScheduler).run"} {
		if got := strings.Count(goroutineStacks(), frame); got != 1 {
			t.Fatalf("%d %s goroutines, want 1", got, frame)
		}
	}
}

// Every identity that shares a path reads the same verdict, so the ranking a
// path produces is the ranking all of its identities act on.
func TestSmartDialerPathVerdictReachesEveryIdentity(t *testing.T) {
	config := testSmartDialerConfig()
	// exactly one round in the window, so "measured" counts one round's work
	config.pathInterval = time.Hour
	scheduler := newTestSmartDialerScheduler(t, config)
	var probes atomic.Int64
	var memberSum atomic.Int64
	scheduler.probe = countingProbe(scheduler, &probes, &memberSum)

	path := smartDialerPath{address: "10.21.0.1:1080"}
	var members []*smartDialerMember
	for i := 0; i < 4; i++ {
		member := testSmartDialerMember(t, nil, "")
		member.index = i
		members = append(members, member)
		scheduler.Register(path, member)
	}
	scheduler.Start()
	waitFor(t, 5*time.Second, "a round", func() bool { return 0 < probes.Load() })

	// the round's verdict, and only one of the four carries the measurement
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()
	for _, member := range members {
		if member.verdict.leader != "normal" {
			t.Fatalf("identity %d has no path verdict", member.index)
		}
		if member.verdictAt.IsZero() {
			t.Fatalf("identity %d was never stamped with the path verdict", member.index)
		}
	}
	measured := 0
	for _, member := range members {
		if !member.measuredAt.IsZero() {
			measured += 1
		}
	}
	if measured != 1 {
		t.Fatalf("%d identities measured in one round, want 1 (the path's round)", measured)
	}
}

// The stop func a slot gets is safe to call twice and only withdraws its own
// identity: a slot that ends must not take the path (and everyone still on it)
// down with it.
func TestSmartDialerIdentityStopIsIdempotentAndScoped(t *testing.T) {
	scheduler := newTestSmartDialerScheduler(t, testSmartDialerConfig())
	path := smartDialerPath{address: "10.30.0.1:1080"}
	stopFirst := startSmartDialerIdentity(nil, nil, "", 1, path.address, false)
	stopSecond := startSmartDialerIdentity(nil, nil, "", 2, path.address, false)
	if got := scheduler.identities(); got != 2 {
		t.Fatalf("registered %d identities, want 2", got)
	}

	stopFirst()
	stopFirst() // idempotent
	if got := scheduler.identities(); got != 1 {
		t.Fatalf("%d identities left after one stop, want 1", got)
	}
	if got := scheduler.paths(); got != 1 {
		t.Fatalf("the path was dropped while an identity still used it (%d paths)", got)
	}
	stopSecond()
	if got := scheduler.paths(); got != 0 {
		t.Fatalf("%d paths left after the last identity stopped, want 0", got)
	}

	// a stale target from before a re-registration must not remove the new one
	stale := scheduler.Register(path, testSmartDialerMember(t, nil, ""))
	first := stale.members[0]
	scheduler.Unregister(stale, first)
	replacement := scheduler.Register(path, testSmartDialerMember(t, nil, ""))
	if got := scheduler.paths(); got != 1 {
		t.Fatalf("%d paths after re-registering, want 1", got)
	}
	scheduler.Unregister(stale, first)
	if got := scheduler.paths(); got != 1 {
		t.Fatalf("a stale unregister removed the re-registered path (%d paths)", got)
	}
	scheduler.Unregister(replacement, replacement.members[0])
	if got := scheduler.paths(); got != 0 {
		t.Fatalf("%d paths after the replacement withdrew, want 0", got)
	}
}

// Two ordering invariants of the slot wiring, which a refactor could silently
// break and no unit test of the scheduler would notice:
//
//   - an identity becomes a probe target only AFTER it authenticated (an
//     identity stuck in slow retry must not be probed), and
//   - the probe registration is undone before the strategy it dials with is
//     closed, so a round is joined before its strategy goes away.
func TestSmartDialerIdentityIsRegisteredAfterAuthAndStoppedBeforeTheClose(t *testing.T) {
	source, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	created := strings.Index(text, "clientStrategy := connect.NewClientStrategy(proxyCtx, clientStrategySettings)")
	authSucceeded := strings.Index(text, "noteProviderAuthSuccess(proxySettings)")
	registered := strings.Index(text, "startSmartDialerIdentity(clientStrategy, bw, self.apiUrl")
	if created < 0 || authSucceeded < 0 {
		t.Fatal("the slot no longer creates its strategy or records auth success this way; update this test")
	}
	if registered < 0 {
		t.Fatal("the slot does not register the identity with the smart dialer scheduler")
	}
	if registered < authSucceeded {
		t.Fatal("the identity is registered before its auth outcome is known; it must register only after auth succeeds")
	}
	if created > registered {
		t.Fatal("the registration does not name the slot's own strategy")
	}
	if strings.Count(text, "startSmartDialerIdentity(") != 1 {
		t.Fatal("expected exactly one smart dialer registration per identity")
	}
	closeDefer := strings.Index(text, "defer clientStrategy.Close()")
	stopDefer := strings.Index(text, "defer stopSmartDialerIdentity()")
	if closeDefer < 0 || stopDefer < 0 {
		t.Fatal("the slot no longer defers the strategy close and the smart dialer stop")
	}
	if closeDefer > stopDefer {
		t.Fatal("the registration is not joined before the strategy is closed")
	}
}
