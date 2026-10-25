package miner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureHeartbeatLog swaps the proxy pipeline log sink for a buffer, so a
// pulse's lines can be asserted on without writing to the test's stderr.
func captureHeartbeatLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buffer := &bytes.Buffer{}
	prev := proxyManagerLogWriter
	proxyManagerLogWriter = buffer
	t.Cleanup(func() { proxyManagerLogWriter = prev })
	return buffer
}

// readHeartbeatLine extracts and parses the status file's heartbeat line. It
// fails the test when the line is missing, because a missing line is the signal
// "this process has never pulsed" and every caller here expects one.
func readHeartbeatLine(t *testing.T, text string) healthHeartbeatRecord {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		rest, ok := strings.CutPrefix(line, " heartbeat: ")
		if !ok {
			continue
		}
		var record healthHeartbeatRecord
		if err := json.Unmarshal([]byte(rest), &record); err != nil {
			t.Fatalf("heartbeat line is not valid JSON: %v\nline: %s", err, line)
		}
		return record
	}
	t.Fatalf("no heartbeat line in:\n%s", text)
	return healthHeartbeatRecord{}
}

// heartbeatStaleAt is the instant the record becomes stale, the way an external
// monitor computes it.
func heartbeatStaleAt(t *testing.T, record healthHeartbeatRecord) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, record.At)
	if err != nil {
		t.Fatalf("heartbeat at %q is not RFC3339: %v", record.At, err)
	}
	return at.Add(time.Duration(record.StaleAfterSec * float64(time.Second)))
}

func TestHealthHeartbeatInterval(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", healthHeartbeatDefaultInterval},
		{"2m", 2 * time.Minute},
		{"90s", 90 * time.Second},
		{"1m", healthHeartbeatMinInterval},                  // exactly the floor
		{"45s", healthHeartbeatMinInterval},                 // below the floor clamps, not defaults
		{"5s", healthHeartbeatMinInterval},                  // same for anything faster
		{"0", healthHeartbeatDefaultInterval},               // parses, but not a cadence
		{"-3m", healthHeartbeatDefaultInterval},             // negative
		{"every-5-minutes", healthHeartbeatDefaultInterval}, // unparseable
		{" 3m ", 3 * time.Minute},                           // operator whitespace
	}
	for _, c := range cases {
		if got := healthHeartbeatInterval(c.raw); got != c.want {
			t.Errorf("healthHeartbeatInterval(%q) = %s, want %s", c.raw, got, c.want)
		}
	}
}

// seedLiveHeartbeatHealth writes the registry's health buckets directly. The
// noteLiveProxyHealth* helpers only produce "up" and "recently_offline"
// (degraded); a dead identity is what the state file seeds, and the count
// arithmetic must be pinned against all three buckets.
func seedLiveHeartbeatHealth(t *testing.T, healths map[string]string) {
	t.Helper()
	resetProxyHealthReportStateForTest()
	t.Cleanup(resetProxyHealthReportStateForTest)
	proxyLiveHealthRegistry.Lock()
	defer proxyLiveHealthRegistry.Unlock()
	for key, health := range healths {
		proxyLiveHealthRegistry.byKey[key] = proxyLiveHealthEntry{Health: health}
	}
}

// TestPublishHealthHeartbeatCountsEveryBucket is the regression test for the
// arity bug this pulse shipped with: proxyHealthRegistryCounts returns
// (up, degraded, dead, authFailures), a positional binding read dead as "down"
// and threw the real dead count away, and a fully dead pool rendered as
// proxies=0 up=0 down=0 — the exact healthy-looking report the heartbeat exists
// to prevent.
func TestPublishHealthHeartbeatCountsEveryBucket(t *testing.T) {
	cases := []struct {
		name     string
		healths  map[string]string
		wantUp   int
		wantDown int
	}{
		{
			name:     "healthy pool reports no down",
			healths:  map[string]string{"198.51.100.1:1080": "up", "198.51.100.2:1080": "up"},
			wantUp:   2,
			wantDown: 0,
		},
		{
			name:     "degraded only pool reports down",
			healths:  map[string]string{"198.51.100.1:1080": "recently_offline"},
			wantUp:   0,
			wantDown: 1,
		},
		{
			name:     "fully dead pool reports down, not an empty pool",
			healths:  map[string]string{"198.51.100.1:1080": "dead", "198.51.100.2:1080": "inactive"},
			wantUp:   0,
			wantDown: 2,
		},
		{
			// A leg-failure entry has no health value at all; the bucket rule
			// treats "" as dead, so it must count as down like any other dead
			// identity rather than vanishing.
			name:     "mixed pool folds degraded and dead into down",
			healths:  map[string]string{"198.51.100.1:1080": "up", "198.51.100.2:1080": "recently_offline", "198.51.100.3:1080": ""},
			wantUp:   1,
			wantDown: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withTempHome(t)
			resetHealthHeartbeatForTest()
			t.Cleanup(resetHealthHeartbeatForTest)
			// Nothing has reached the control plane in this process, so the
			// persisted grade must not surface as "up" no matter how many
			// entries say up.
			resetControlReached()
			t.Cleanup(resetControlReached)
			line := captureHeartbeatLog(t)
			seedLiveHeartbeatHealth(t, c.healths)

			now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
			hb := publishHealthHeartbeat(now, now.Add(-time.Hour), "default", healthHeartbeatDefaultInterval)

			wantTotal := c.wantUp + c.wantDown
			// c.wantUp is the PERSISTED grade now: it may only appear as
			// last_known_up, while up reports this process's own evidence.
			if hb.ProxiesUp != 0 || hb.LastKnownUp != c.wantUp || hb.ProxiesDown != c.wantDown || hb.Proxies != wantTotal {
				t.Fatalf("counts = up %d last_known_up %d down %d total %d, want 0/%d/%d/%d",
					hb.ProxiesUp, hb.LastKnownUp, hb.ProxiesDown, hb.Proxies, c.wantUp, c.wantDown, wantTotal)
			}
			// The on-disk record is what an external monitor reads: pin the same
			// numbers there, not only on the in-memory snapshot.
			record := hb.record()
			if record.ProxiesUp != 0 || record.ProxiesDown != c.wantDown || record.Proxies != wantTotal {
				t.Fatalf("record counts = up %d down %d total %d, want 0/%d/%d",
					record.ProxiesUp, record.ProxiesDown, record.Proxies, c.wantDown, wantTotal)
			}
			// And the operator-facing log line, which is where a wrong count was
			// easiest to miss.
			want := fmt.Sprintf("proxies=%d up=0 reached_ever=0 last_known_up=%d down=%d", wantTotal, c.wantUp, c.wantDown)
			if !strings.Contains(line.String(), want) {
				t.Fatalf("log line missing %q:\n%s", want, line.String())
			}
		})
	}
}

// TestHealthUpIsThisProcessControlEvidence is the other half: once slots have
// actually reached the control plane here, up reports them, and it reports them
// independently of what the persisted grade says. Without this the previous
// test would pass just as well if up were hardcoded to zero.
func TestHealthUpIsThisProcessControlEvidence(t *testing.T) {
	withTempHome(t)
	resetHealthHeartbeatForTest()
	t.Cleanup(resetHealthHeartbeatForTest)
	resetControlReached()
	t.Cleanup(resetControlReached)
	line := captureHeartbeatLog(t)
	// The whole pool is graded up in the state file, AND two slots have dialed
	// here: up must read 2, not the 3 the grade claims.
	seedLiveHeartbeatHealth(t, map[string]string{
		"198.51.100.1:1080": "up",
		"198.51.100.2:1080": "up",
		"198.51.100.3:1080": "up",
	})
	one := newControlEvidence("198.51.100.1:1080")
	two := newControlEvidence("198.51.100.2:1080")
	t.Cleanup(one.lost)
	t.Cleanup(two.lost)
	one.reached()
	two.reached()

	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	hb := publishHealthHeartbeat(now, now.Add(-time.Hour), "default", healthHeartbeatDefaultInterval)

	if hb.ProxiesUp != 2 || hb.LastKnownUp != 3 || hb.Proxies != 3 {
		t.Fatalf("counts = up %d last_known_up %d total %d, want 2/3/3",
			hb.ProxiesUp, hb.LastKnownUp, hb.Proxies)
	}
	if want := "up=2 reached_ever=2 last_known_up=3"; !strings.Contains(line.String(), want) {
		t.Fatalf("log line missing %q:\n%s", want, line.String())
	}

	// One slot exits. up is "currently reached", so it falls; reached_ever
	// remembers; the persisted grade does not move. The on-disk record carries
	// the same split.
	one.lost()
	line.Reset()
	hb = publishHealthHeartbeat(now.Add(time.Minute), now.Add(-time.Hour), "default", healthHeartbeatDefaultInterval)
	if hb.ProxiesUp != 1 || hb.ReachedEver != 2 || hb.LastKnownUp != 3 {
		t.Fatalf("after a slot exit: up %d reached_ever %d last_known_up %d, want 1/2/3",
			hb.ProxiesUp, hb.ReachedEver, hb.LastKnownUp)
	}
	if record := hb.record(); record.ProxiesUp != 1 || record.LastKnownUp != 3 {
		t.Fatalf("after a slot exit the record says proxies_up %d last_known_up %d, want 1/3",
			record.ProxiesUp, record.LastKnownUp)
	}
	if want := "up=1 reached_ever=2 last_known_up=3"; !strings.Contains(line.String(), want) {
		t.Fatalf("log line missing %q:\n%s", want, line.String())
	}
}

// TestHealthHeartbeatStaleAfterMatchesFloorInterval pins the rendered staleness
// bound at the documented floor: an operator who sets the minimum must get a
// bound built from the interval actually in force, not from the 5m default.
func TestHealthHeartbeatStaleAfterMatchesFloorInterval(t *testing.T) {
	withTempHome(t)
	resetHealthHeartbeatForTest()
	t.Cleanup(resetHealthHeartbeatForTest)
	captureHeartbeatLog(t)

	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	hb := publishHealthHeartbeat(now, now.Add(-time.Hour), "default", healthHeartbeatMinInterval)

	want := healthHeartbeatMinInterval + proxyHealthWriterInterval() + healthHeartbeatStatusGrace
	if got := time.Duration(hb.record().StaleAfterSec * float64(time.Second)); got != want {
		t.Fatalf("stale_after at the 1m floor = %s, want %s", got, want)
	}
	if hb.record().IntervalSec != healthHeartbeatMinInterval.Seconds() {
		t.Fatalf("interval_seconds = %v, want %v", hb.record().IntervalSec, healthHeartbeatMinInterval.Seconds())
	}
}

// TestStaleAfterTracksStatusWriterCadence pins the status-write term of the
// staleness bound to the cadence the writer is really running at. It used to be
// the proxyHealthLogInterval constant, which made stale_after contradict the
// file's actual refresh rate whenever the writer was started at another
// interval.
func TestStaleAfterTracksStatusWriterCadence(t *testing.T) {
	withTempHome(t)
	resetHealthHeartbeatForTest()
	captureHeartbeatLog(t)
	t.Cleanup(func() {
		resetHealthHeartbeatForTest()
		proxyHealthWriterCadence.Store(int64(proxyHealthLogInterval))
	})

	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	hb := publishHealthHeartbeat(now, now.Add(-time.Hour), "default", healthHeartbeatMinInterval)

	// The writer is started at 90s, not the 60s default.
	ctx, cancel := context.WithCancel(context.Background())
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		runProxyHealthLog(ctx, 90*time.Second)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for proxyHealthWriterInterval() != 90*time.Second {
		if time.Now().After(deadline) {
			cancel()
			<-writerDone
			t.Fatalf("runProxyHealthLog did not publish its cadence: %s", proxyHealthWriterInterval())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-writerDone

	want := healthHeartbeatMinInterval + 90*time.Second + healthHeartbeatStatusGrace
	if got := time.Duration(hb.record().StaleAfterSec * float64(time.Second)); got != want {
		t.Fatalf("stale_after with a 90s writer = %s, want %s", got, want)
	}
}

// TestJoinHealthLoopIsBounded pins that shutdown cannot hang on a health
// goroutine that is stuck in a syscall: the join gives up after the grace and
// the run logs it rather than blocking exit.
func TestJoinHealthLoopIsBounded(t *testing.T) {
	captureHeartbeatLog(t)

	closed := make(chan struct{})
	close(closed)
	if !joinHealthLoop(closed, "already stopped") {
		t.Fatal("joinHealthLoop reported a timeout for a goroutine that had already stopped")
	}

	started := time.Now()
	if joinHealthLoop(make(chan struct{}), "stuck") {
		t.Fatal("joinHealthLoop waited forever on a goroutine that never stopped")
	}
	if elapsed := time.Since(started); elapsed > healthLoopShutdownGrace+2*time.Second {
		t.Fatalf("joinHealthLoop took %s, want about %s", elapsed, healthLoopShutdownGrace)
	}
}

func TestHeartbeatHostLabelFrom(t *testing.T) {
	cases := []struct{ nodeEnv, hostname, want string }{
		{"box-a", "kernel-b", "box-a"},
		{"  ", "kernel-b", "kernel-b"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := heartbeatHostLabelFrom(c.nodeEnv, c.hostname); got != c.want {
			t.Errorf("heartbeatHostLabelFrom(%q, %q) = %q, want %q", c.nodeEnv, c.hostname, got, c.want)
		}
	}
}

// TestPublishHealthHeartbeatRecordFields pins the readings and the on-disk field
// set: an external monitor codes against these names.
func TestPublishHealthHeartbeatRecordFields(t *testing.T) {
	withTempHome(t)
	resetHealthHeartbeatForTest()
	t.Cleanup(func() {
		resetHealthHeartbeatForTest()
		resetProxyHealthReportStateForTest()
	})
	captureHeartbeatLog(t)

	noteLiveProxyHealthUp("203.0.113.9:1080")
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	hb := publishHealthHeartbeat(now, now.Add(-(90*time.Minute + 30*time.Second)), "qa", healthHeartbeatDefaultInterval)

	if hb.At != now {
		t.Fatalf("heartbeat At = %s, want %s", hb.At, now)
	}
	if hb.Interval != healthHeartbeatDefaultInterval {
		t.Fatalf("interval = %s, want the provider's %s", hb.Interval, healthHeartbeatDefaultInterval)
	}
	if hb.Uptime != 90*time.Minute+30*time.Second {
		t.Fatalf("uptime = %s, want 1h30m30s", hb.Uptime)
	}
	if hb.Profile != "qa" || hb.Version == "" {
		t.Fatalf("identity = profile %q version %q, want qa and a version", hb.Profile, hb.Version)
	}
	if hb.PID != os.Getpid() {
		t.Fatalf("pid = %d, want %d", hb.PID, os.Getpid())
	}
	// A live-health observation is not control-plane evidence: this process has
	// dialed nothing, so the entry surfaces as last_known_up while up stays 0.
	if hb.ProxiesUp != 0 || hb.LastKnownUp != 1 || hb.ProxiesDown != 0 || hb.Proxies != 1 {
		t.Fatalf("proxy counts = up %d last_known_up %d down %d total %d, want 0/1/0/1",
			hb.ProxiesUp, hb.LastKnownUp, hb.ProxiesDown, hb.Proxies)
	}
	if hb.Goroutines <= 0 {
		t.Fatalf("goroutines = %d, want a live reading", hb.Goroutines)
	}
	if got := healthHeartbeatSnapshot(); got != hb {
		t.Fatal("the pulse was not published for the status writer to read")
	}

	data, err := json.Marshal(hb.record())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"at", "interval_seconds", "stale_after_seconds", "uptime_seconds",
		"version", "profile", "host", "pid",
		"heap_mib", "sys_mib", "goroutines",
		"proxies", "proxies_up", "last_known_up", "proxies_down", "clients", "counts_known",
	} {
		if _, ok := fields[name]; !ok {
			t.Errorf("heartbeat record is missing field %q: %s", name, data)
		}
	}
	// last_known_up joins the set so a monitor reading the file can still see
	// the persisted grade after proxies_up changed meaning to "reached here".
	if len(fields) != 17 {
		t.Errorf("heartbeat record has %d fields, want the 17 named ones: %s", len(fields), data)
	}
}

// TestPublishHealthHeartbeatClampsFutureStart pins that a start time in the
// future cannot render a negative uptime, which would read as a broken process.
func TestPublishHealthHeartbeatClampsFutureStart(t *testing.T) {
	withTempHome(t)
	resetHealthHeartbeatForTest()
	t.Cleanup(resetHealthHeartbeatForTest)
	captureHeartbeatLog(t)

	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	hb := publishHealthHeartbeat(now, now.Add(time.Hour), "", healthHeartbeatDefaultInterval)
	if hb.Uptime != 0 {
		t.Fatalf("uptime = %s, want 0 for a future start", hb.Uptime)
	}
	if hb.Profile != "default" {
		t.Fatalf("profile = %q, want the provider's default", hb.Profile)
	}
}

// TestHealthHeartbeatStaleWhenPulseLoopStops is the monitor's contract: the
// stamp does not move unless a pulse happens, so a status writer that keeps
// running while the pulse loop is wedged leaves a heartbeat a monitor can call
// stale.
func TestHealthHeartbeatStaleWhenPulseLoopStops(t *testing.T) {
	withTempHome(t)
	resetHealthHeartbeatForTest()
	t.Cleanup(resetHealthHeartbeatForTest)
	captureHeartbeatLog(t)

	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	hb := publishHealthHeartbeat(now, now.Add(-time.Hour), "default", healthHeartbeatDefaultInterval)

	// A status write shortly after the pulse: fresh.
	fresh := now.Add(10 * time.Second)
	freshRecord := readHeartbeatLine(t, formatStateFileWithHeartbeat(proxyHealthReport{}, fresh, hb))
	if !fresh.Before(heartbeatStaleAt(t, freshRecord)) {
		t.Fatalf("a heartbeat %s old read as stale (stale_at %s)", fresh.Sub(now), heartbeatStaleAt(t, freshRecord))
	}

	// The writer keeps running, the pulse loop does not: the stamp must not
	// advance, and the file must become stale exactly on its own threshold.
	later := now.Add(healthHeartbeatDefaultInterval + proxyHealthWriterInterval() + healthHeartbeatStatusGrace + time.Second)
	laterRecord := readHeartbeatLine(t, formatStateFileWithHeartbeat(proxyHealthReport{}, later, hb))
	if laterRecord.At != freshRecord.At {
		t.Fatalf("heartbeat stamp advanced without a pulse: %s -> %s", freshRecord.At, laterRecord.At)
	}
	if !later.After(heartbeatStaleAt(t, laterRecord)) {
		t.Fatalf("a stopped pulse loop still reads as fresh at +%s (stale_at %s)", later.Sub(now), heartbeatStaleAt(t, laterRecord))
	}
}

// TestRunHealthHeartbeatPulsesOnCadence runs the loop with an injected cadence:
// one pulse at start, then one per interval until the context ends.
func TestRunHealthHeartbeatPulsesOnCadence(t *testing.T) {
	withTempHome(t)
	resetHealthHeartbeatForTest()
	t.Cleanup(resetHealthHeartbeatForTest)
	captureHeartbeatLog(t)

	start := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHealthHeartbeatWith(ctx, start, "default", 10*time.Millisecond)
	}()
	defer func() {
		cancel()
		<-done
	}()

	first := healthHeartbeatSnapshot()
	if first == nil {
		// The goroutine may not have been scheduled yet; the first pulse must
		// land promptly, not after a full interval.
		deadline := time.Now().Add(2 * time.Second)
		for first == nil && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
			first = healthHeartbeatSnapshot()
		}
	}
	if first == nil {
		t.Fatal("no pulse at loop start: a fresh process must not read as never pulsed")
	}

	pulses := 0
	last := first.At
	deadline := time.Now().Add(5 * time.Second)
	for pulses < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("heartbeat repeated %d times on a 10ms cadence within 5s", pulses)
		}
		if current := healthHeartbeatSnapshot(); current != nil && current.At.After(last) {
			last = current.At
			pulses++
			continue
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForHeartbeatPulses blocks until the pulse has published n distinct
// snapshots, or fails the test. Distinct pointers, not distinct timestamps: a
// fast test cadence must still count real pulses.
func waitForHeartbeatPulses(t *testing.T, n int) {
	t.Helper()
	seen := map[*healthHeartbeat]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for len(seen) < n {
		if time.Now().After(deadline) {
			t.Fatalf("saw %d distinct pulses in 5s, want %d", len(seen), n)
		}
		if hb := healthHeartbeatSnapshot(); hb != nil {
			seen[hb] = true
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRunHealthHeartbeatDoesNotPruneOnThePulsePath pins the separation that keeps
// the wedge signal honest: the prune reads three files, so it must not run
// between a pulse and the next tick. If it ever moves back in, a slow disk can
// delay the stamp past stale_after and make a healthy process read as wedged.
func TestRunHealthHeartbeatDoesNotPruneOnThePulsePath(t *testing.T) {
	withTempHome(t)
	resetHealthHeartbeatForTest()
	captureHeartbeatLog(t)
	t.Cleanup(func() {
		resetHealthHeartbeatForTest()
		globalProxyFailureHistory.Reset("6.6.6.6:1080")
	})

	// Empty desired set: a prune here would drop every identity's history.
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{}}); err != nil {
		t.Fatal(err)
	}
	globalProxyFailureHistory.RecordFailure("6.6.6.6:1080")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHealthHeartbeatWith(ctx, time.Now(), "default", 5*time.Millisecond)
	}()
	waitForHeartbeatPulses(t, 4)
	cancel()
	<-done

	if globalProxyFailureHistory.FailureCount("6.6.6.6:1080") == 0 {
		t.Fatal("the pulse pruned the history stores: the disk-reading prune is back on the liveness path")
	}
}

// TestRunProxyHistoryPruneLoopPrunesOnItsCadence pins the other half: moving the
// prune off the pulse must not have dropped it. The loop still bounds the stores,
// on its own goroutine.
func TestRunProxyHistoryPruneLoopPrunesOnItsCadence(t *testing.T) {
	withTempHome(t)
	captureHeartbeatLog(t)
	t.Cleanup(func() { globalProxyFailureHistory.Reset("6.6.6.6:1080") })

	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{}}); err != nil {
		t.Fatal(err)
	}
	globalProxyFailureHistory.RecordFailure("6.6.6.6:1080")
	if globalProxyFailureHistory.FailureCount("6.6.6.6:1080") == 0 {
		t.Fatal("could not seed a failure to prune")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runProxyHistoryPruneLoop(ctx, 5*time.Millisecond)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for globalProxyFailureHistory.FailureCount("6.6.6.6:1080") != 0 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the prune loop never pruned an undesired identity")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

// TestStatusFileCarriesHeartbeat pins the integration point: the heartbeat is
// rendered into the status file the proxy health cadence already writes, not
// into a second file.
func TestStatusFileCarriesHeartbeat(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	path := filepath.Join(dir, "proxy_health.state")
	writeProxyHealthState(dir, proxyHealthReport{Up: 1}, now)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), " heartbeat: ") {
		t.Fatalf("status file must omit the heartbeat line before the first pulse:\n%s", data)
	}

	// Publish the way a pulse does, then re-render: the next status write must
	// carry it without a second file appearing.
	healthHeartbeatCurrent.Store(&healthHeartbeat{
		At:       now,
		Interval: healthHeartbeatDefaultInterval,
		Uptime:   time.Hour,
		Version:  "v9.9.9",
		Profile:  "default",
		Host:     "box",
		PID:      4242,
	})
	t.Cleanup(resetHealthHeartbeatForTest)

	writeProxyHealthState(dir, proxyHealthReport{Up: 1}, now)
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	record := readHeartbeatLine(t, string(data))
	if record.Version != "v9.9.9" || record.PID != 4242 {
		t.Fatalf("status file heartbeat = %+v, want version v9.9.9 pid 4242", record)
	}
	if !strings.Contains(string(data), "Updated: 2026-10-24T12:00:00Z") {
		t.Fatalf("status file lost its own stamp:\n%s", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "proxy_health.state" {
			t.Fatalf("the heartbeat must not add a file of its own; found %q", entry.Name())
		}
	}
}

// TestPruneProxyHistoryStores pins the provider's rule: an identity still in
// backoff keeps its history even when it is no longer in the desired set, and
// an identity with no claim on the run loses it.
func TestPruneProxyHistoryStores(t *testing.T) {
	withTempHome(t)
	t.Cleanup(func() {
		globalProxyFailureHistory.Reset("5.5.5.5:1080")
		globalProxyFailureHistory.Reset("6.6.6.6:1080")
		globalProvenProxies.Prune(map[string]bool{})
	})

	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{}}); err != nil {
		t.Fatal(err)
	}

	// Kept: mid-backoff, so its history must survive until the backoff lapses.
	globalProxyFailureHistory.RecordFailure("5.5.5.5:1080")
	globalProxyFailureHistory.SetBackoffUntil("5.5.5.5:1080", time.Now().Add(time.Hour))
	globalProvenProxies.MarkSucceeded("5.5.5.5:1080")

	// Dropped: not desired, not in backoff.
	globalProxyFailureHistory.RecordFailure("6.6.6.6:1080")
	globalProvenProxies.MarkSucceeded("6.6.6.6:1080")

	pruneProxyHistoryStores()

	if globalProxyFailureHistory.FailureCount("5.5.5.5:1080") == 0 {
		t.Fatal("a mid-backoff identity lost its failure history")
	}
	if !globalProvenProxies.HasSucceeded("5.5.5.5:1080") {
		t.Fatal("a mid-backoff identity lost its proven mark")
	}
	if globalProxyFailureHistory.FailureCount("6.6.6.6:1080") != 0 {
		t.Fatal("an undesired identity kept its failure history")
	}
	if globalProvenProxies.HasSucceeded("6.6.6.6:1080") {
		t.Fatal("an undesired identity kept its proven mark")
	}
}

// TestPruneProxyHistoryStoresKeepsLiveIdentityOnReadError pins the safer
// fallback: when the desired set cannot be read, the keep-set is the identities
// this process is running or reporting on, never an empty set that would drop
// every backoff and retry a given-up proxy forever.
func TestPruneProxyHistoryStoresKeepsLiveIdentityOnReadError(t *testing.T) {
	home := withTempHome(t)
	captureHeartbeatLog(t)
	t.Cleanup(func() {
		globalProxyFailureHistory.Reset("7.7.7.7:1080")
		resetProxyHealthReportStateForTest()
	})

	missing := filepath.Join(home, "does-not-exist.txt")
	if err := writeProxyState(&ProxyState{Source: missing, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	noteLiveProxyHealthUp("7.7.7.7:1080")
	globalProxyFailureHistory.RecordFailure("7.7.7.7:1080")

	pruneProxyHistoryStores()

	if globalProxyFailureHistory.FailureCount("7.7.7.7:1080") == 0 {
		t.Fatal("a read error wiped the history of an identity this process is reporting on")
	}
}
