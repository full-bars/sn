package miner

// Tests for the swap-thrash watchdog port (provider/thrash_watchdog.go in the
// relay). Every reading comes from a scripted sensor and every wait from a
// fake clock: no test here touches /proc, /sys or the real state directory.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var swapThrashT0 = time.Unix(1_800_000_000, 0)

// formatLogLine renders one log call the way the production writers do.
func formatLogLine(format string, args ...any) string {
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

// scriptedSwapThrashSensor returns readings in order, repeating the last one
// once the script is exhausted.
type scriptedSwapThrashSensor struct {
	readings []swapThrashRead
	calls    atomic.Int64
}

func (s *scriptedSwapThrashSensor) read() swapThrashRead {
	if len(s.readings) == 0 {
		return swapThrashRead{}
	}
	i := int(s.calls.Add(1) - 1)
	if i >= len(s.readings) {
		i = len(s.readings) - 1
	}
	return s.readings[i]
}

// swapThrashTestWatch builds a watch over the scripted sensor, a fake clock
// and a status file in a temp dir.
func swapThrashTestWatch(t *testing.T, sensor *scriptedSwapThrashSensor, clock *time.Time, logs *[]string) (*swapThrashWatch, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "thrash_status")
	watch := newSwapThrashWatch(swapThrashOptions{
		readFn:   sensor.read,
		nowFn:    func() time.Time { return *clock },
		interval: swapThrashSampleInterval,
		statusPath: func() (string, error) {
			return path, nil
		},
		logf: func(format string, args ...any) {
			*logs = append(*logs, "detail: "+formatLogLine(format, args...))
		},
		notef: func(format string, args ...any) {
			*logs = append(*logs, "note: "+formatLogLine(format, args...))
		},
		critf: func(format string, args ...any) {
			*logs = append(*logs, "crit: "+formatLogLine(format, args...))
		},
		// The pre-exit line is stderr-only in production; the rig observes it
		// here so a test can still assert the restart is announced.
		exitNotef: func(format string, args ...any) {
			*logs = append(*logs, "exit: "+formatLogLine(format, args...))
		},
		// Evidence is off for the scripted-storm rig: these tests drive the real
		// action ladder into restarts and refusals, and a bundle per rung would
		// land in the real state directory (the rig injects no state dir). The
		// capture path has its own tests in incident_capture_test.go.
		evidenceFn: func(swapThrashEscalation, swapThrashRates, swapThrashRead, string, int) {},
	})
	return watch, path
}

// swapThrashCalmReading is an idle box: every signal readable and quiet.
func swapThrashCalmReading() swapThrashRead {
	return swapThrashRead{
		psiSomeTotal: 1_000_000, psiFullTotal: 500_000,
		psiSomeOK: true, psiFullOK: true, psiSomeUnit: true, psiUnit: true,
		swapIn: 10_000, swapOut: 20_000, swapOK: true,
		refault: 1_000, refaultOK: true,
		unitSwapMiB:      64,
		unitSwapOK:       true,
		hostSwapUsedMiB:  100,
		hostSwapTotalMiB: 4000,
		hostSwapOK:       true,
		ramAvailMiB:      3000,
		ramTotalMiB:      8000,
		ramAvailOK:       true,
		ramTotalOK:       true,
		heapFrac:         0.4,
		heapUsedMiB:      400,
		heapLimitMiB:     1000,
		heapOK:           true,
	}
}

// swapThrashStormReading is the same box 30s into a swap storm: PSI full 45%,
// 800 pages/s of swap, and this cgroup holding 94% of the swap in use.
func swapThrashStormReading(at time.Duration) swapThrashRead {
	rd := swapThrashCalmReading()
	seconds := float64(at / time.Second)
	rd.psiFullTotal = 500_000 + uint64(0.45*seconds*1_000_000)
	rd.psiSomeTotal = 1_000_000 + uint64(0.8*seconds*1_000_000)
	rd.swapIn = 10_000 + 400*uint64(at/time.Second)
	rd.swapOut = 20_000 + 400*uint64(at/time.Second)
	rd.refault = 1_000 + 100*uint64(at/time.Second)
	rd.unitSwapMiB = 3100
	rd.hostSwapUsedMiB = 3300
	rd.hostSwapTotalMiB = 3300
	rd.ramAvailMiB = 80
	rd.heapFrac = 1.6
	rd.heapUsedMiB = 1600
	rd.heapLimitMiB = 1000
	return rd
}

// The miner has no swap sensor, no PSI reading and no watchdog state, so this
// test does not even build before the port: a scripted swap-thrash pattern
// must drive the ladder to thrashing and publish the human-readable status.
func TestSwapThrashWatchDetectsScriptedSwapStorm(t *testing.T) {
	clock := swapThrashT0
	sensor := &scriptedSwapThrashSensor{readings: []swapThrashRead{
		swapThrashCalmReading(),
		swapThrashStormReading(30 * time.Second),
		swapThrashStormReading(60 * time.Second),
		swapThrashStormReading(120 * time.Second),
	}}
	var logs []string
	watch, path := swapThrashTestWatch(t, sensor, &clock, &logs)

	// Startup baseline.
	watch.sampleIfDue(clock)
	if watch.state() != swapThrashCalm {
		t.Fatalf("baseline: want calm, got %s", watch.state())
	}

	// Severe (PSI full >= 25%) sustained for 90s takes the ladder to
	// thrashing; before that the box is only under pressure.
	clock = swapThrashT0.Add(30 * time.Second)
	watch.sampleIfDue(clock)
	if watch.state() != swapThrashUnderPressure {
		t.Fatalf("t+30s: want under-pressure, got %s", watch.state())
	}
	clock = swapThrashT0.Add(60 * time.Second)
	watch.sampleIfDue(clock)
	if watch.state() != swapThrashUnderPressure {
		t.Fatalf("t+60s: want under-pressure, got %s", watch.state())
	}
	clock = swapThrashT0.Add(120 * time.Second)
	watch.sampleIfDue(clock)
	if watch.state() != swapThrashThrashing {
		t.Fatalf("t+120s: want thrashing, got %s", watch.state())
	}

	snap := watch.snapshot()
	if snap == nil {
		t.Fatal("no snapshot published")
	}
	if snap.State != "thrashing" {
		t.Fatalf("snapshot state: got %q", snap.State)
	}
	if snap.Attribution != "unit" {
		t.Fatalf("attribution: got %q, want unit", snap.Attribution)
	}
	if snap.UnitShare == nil || *snap.UnitShare < 0.9 {
		t.Fatalf("unit share: got %v", snap.UnitShare)
	}
	for _, want := range []string{
		"the box was stalled on memory ~45% of the time",
		"swapping ~3 MB/s",
		"this provider holds 94% of all swap in use",
		"heap 1.6x its limit",
	} {
		if !strings.Contains(snap.Summary, want) {
			t.Errorf("summary missing %q: %s", want, snap.Summary)
		}
	}

	// The status file is the relay's operator surface, same path name and
	// field names.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read status file: %v", err)
	}
	var onDisk swapThrashSnapshot
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("status file is not the snapshot JSON: %v", err)
	}
	if onDisk.State != "thrashing" || onDisk.Summary == "" {
		t.Fatalf("status file: state=%q summary=%q", onDisk.State, onDisk.Summary)
	}

	// The onset line must be the relay's plain sentence: who holds the
	// memory and what happens next, no decoding.
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "The box is out of RAM and thrashing swap") {
		t.Fatalf("no plain-language onset line in logs:\n%s", joined)
	}
	if !strings.Contains(joined, "belongs to this provider") {
		t.Fatalf("onset line does not attribute the memory:\n%s", joined)
	}
}

// A thrash that belongs to another process is detected and reported as such.
func TestSwapThrashWatchAttributesAnotherProcess(t *testing.T) {
	clock := swapThrashT0
	readings := []swapThrashRead{swapThrashCalmReading()}
	for _, at := range []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second} {
		storm := swapThrashStormReading(at)
		storm.unitSwapMiB = 100
		storm.hostSwapUsedMiB = 3000
		storm.hostSwapTotalMiB = 3300
		readings = append(readings, storm)
	}
	sensor := &scriptedSwapThrashSensor{readings: readings}

	var logs []string
	watch, _ := swapThrashTestWatch(t, sensor, &clock, &logs)
	watch.sampleIfDue(clock)
	for _, at := range []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second} {
		clock = swapThrashT0.Add(at)
		watch.sampleIfDue(clock)
	}
	snap := watch.snapshot()
	if snap == nil || snap.Attribution != "other" {
		t.Fatalf("attribution: %+v", snap)
	}
	if !strings.Contains(snap.Summary, "the memory belongs to another process") {
		t.Fatalf("summary: %s", snap.Summary)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "the memory belongs to another process") ||
		!strings.Contains(joined, "Not restarting") {
		t.Fatalf("other-process onset line missing:\n%s", joined)
	}
}

// No readable source is calm, and the status says so instead of inventing
// zeros.
func TestSwapThrashWatchUnavailableSensorsStayCalm(t *testing.T) {
	clock := swapThrashT0
	sensor := &scriptedSwapThrashSensor{readings: []swapThrashRead{{}}}
	var logs []string
	watch, path := swapThrashTestWatch(t, sensor, &clock, &logs)
	watch.sampleIfDue(clock)
	clock = swapThrashT0.Add(time.Minute)
	watch.sampleIfDue(clock)

	if watch.state() != swapThrashCalm {
		t.Fatalf("want calm with no signals, got %s", watch.state())
	}
	snap := watch.snapshot()
	if snap == nil {
		t.Fatal("no snapshot")
	}
	if snap.PSIFull != nil || snap.SwapIOPS != nil || snap.UnitSwapMiB != nil {
		t.Fatalf("unavailable signals must serialize as null: %+v", snap)
	}
	if !strings.Contains(snap.Summary, "no signals readable") {
		t.Fatalf("summary: %s", snap.Summary)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("status file must exist: %v", err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "INERT") {
		t.Fatalf("an all-unavailable box must say the watch is inert:\n%s", strings.Join(logs, "\n"))
	}
}

// The ladder relaxes one state per sustained-calm window, never straight to
// calm.
func TestSwapThrashWatchRelaxesOneStepPerCalmWindow(t *testing.T) {
	clock := swapThrashT0
	sensor := &scriptedSwapThrashSensor{readings: []swapThrashRead{
		swapThrashCalmReading(),
		swapThrashStormReading(30 * time.Second),
		swapThrashStormReading(60 * time.Second),
		swapThrashStormReading(120 * time.Second),
	}}
	var logs []string
	watch, _ := swapThrashTestWatch(t, sensor, &clock, &logs)
	watch.sampleIfDue(clock)
	for _, at := range []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second} {
		clock = swapThrashT0.Add(at)
		watch.sampleIfDue(clock)
	}
	if watch.state() != swapThrashThrashing {
		t.Fatalf("want thrashing, got %s", watch.state())
	}

	// Calm starts at t+150s: the storm's cumulative counters stop advancing.
	calm := swapThrashCalmReading()
	calm.psiFullTotal = sensor.readings[3].psiFullTotal
	calm.psiSomeTotal = sensor.readings[3].psiSomeTotal
	calm.swapIn = sensor.readings[3].swapIn
	calm.swapOut = sensor.readings[3].swapOut
	calm.refault = sensor.readings[3].refault
	calm.unitSwapMiB = 64
	calm.hostSwapUsedMiB = 100
	sensor.readings = append(sensor.readings, calm)

	clock = swapThrashT0.Add(150 * time.Second)
	watch.sampleIfDue(clock)
	if watch.state() != swapThrashThrashing {
		t.Fatalf("calm just started: want still thrashing, got %s", watch.state())
	}
	clock = swapThrashT0.Add(150*time.Second + swapThrashCalmRelax + time.Second)
	watch.sampleIfDue(clock)
	if watch.state() != swapThrashUnderPressure {
		t.Fatalf("first calm window: want under-pressure, got %s", watch.state())
	}
	// The next window counts from the tick after the step down.
	clock = swapThrashT0.Add(150*time.Second + swapThrashCalmRelax + time.Minute)
	watch.sampleIfDue(clock)
	if watch.state() != swapThrashUnderPressure {
		t.Fatalf("between calm windows: want under-pressure, got %s", watch.state())
	}
	clock = swapThrashT0.Add(150*time.Second + swapThrashCalmRelax + time.Minute + swapThrashCalmRelax + time.Second)
	watch.sampleIfDue(clock)
	if watch.state() != swapThrashCalm {
		t.Fatalf("second calm window: want calm, got %s", watch.state())
	}
}

// One sample per interval: the miner's 1Hz loop may call every second, but
// the watchdog reads the host at its own 30s pace.
func TestSwapThrashWatchSamplesOnItsInterval(t *testing.T) {
	clock := swapThrashT0
	sensor := &scriptedSwapThrashSensor{readings: []swapThrashRead{swapThrashCalmReading()}}
	var logs []string
	watch, _ := swapThrashTestWatch(t, sensor, &clock, &logs)

	watch.sampleIfDue(clock)
	if sensor.calls.Load() != 1 {
		t.Fatalf("first call must sample immediately, calls=%d", sensor.calls.Load())
	}
	clock = swapThrashT0.Add(time.Second)
	watch.sampleIfDue(clock)
	if sensor.calls.Load() != 1 {
		t.Fatalf("a tick inside the interval must not sample, calls=%d", sensor.calls.Load())
	}
	clock = swapThrashT0.Add(swapThrashSampleInterval + time.Second)
	watch.sampleIfDue(clock)
	if sensor.calls.Load() != 2 {
		t.Fatalf("a tick past the interval must sample, calls=%d", sensor.calls.Load())
	}
}

// Parsers: a missing key is unavailable, never a readable zero.
func TestSwapThrashParsers(t *testing.T) {
	some, full, someOK, fullOK := parseSwapThrashPSI("some avg10=0.00 avg60=0.00 avg300=0.00 total=111\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=42\n")
	if !someOK || !fullOK || some != 111 || full != 42 {
		t.Fatalf("psi: got (%d,%v,%d,%v)", some, someOK, full, fullOK)
	}
	if _, _, _, fullOK := parseSwapThrashPSI("some avg10=0.00 total=5\n"); fullOK {
		t.Fatal("missing full line must be unavailable")
	}

	in, out, ok := parseSwapThrashVMStatSwap("pswpin 100\npswpout 7\n")
	if !ok || in != 100 || out != 7 {
		t.Fatalf("vmstat: got (%d,%d,%v)", in, out, ok)
	}
	if _, _, ok := parseSwapThrashVMStatSwap("pswpin 100\n"); ok {
		t.Fatal("partial vmstat counters must be unavailable")
	}
	if v, ok := parseSwapThrashCounter("anon 100\npswpin 2\npgscan_direct 9\n", "pgscan_direct"); !ok || v != 9 {
		t.Fatalf("counter: got (%d,%v)", v, ok)
	}
	if _, ok := parseSwapThrashCounter("anon 100\n", "pgscan_direct"); ok {
		t.Fatal("missing counter must be unavailable")
	}

	total, free, ok := parseSwapThrashMeminfoSwap("MemTotal: 8000000 kB\nSwapTotal: 4194304 kB\nSwapFree: 1048576 kB\nMemAvailable: 524288 kB\n")
	if !ok || total != 4096 || free != 1024 {
		t.Fatalf("meminfo swap: got (%d,%d,%v)", total, free, ok)
	}
	if v, ok := parseSwapThrashMeminfoValue("MemTotal: 8000000 kB\nMemAvailable: 524288 kB\n", "MemAvailable"); !ok || v != 512 {
		t.Fatalf("meminfo value: got (%d,%v)", v, ok)
	}
}

// The miner's existing always-on sampler loop is what drives the watchdog:
// replacing the package watch with a scripted one and stepping the loop must
// sample it, with no second process and no real host read.
func TestControlSamplerDrivesSwapThrashWatch(t *testing.T) {
	clock := swapThrashT0
	sensor := &scriptedSwapThrashSensor{readings: []swapThrashRead{swapThrashCalmReading()}}
	var logs []string
	watch, _ := swapThrashTestWatch(t, sensor, &clock, &logs)
	watch.interval = 0

	previous := swapThrashWatchGlobal
	swapThrashWatchGlobal = watch
	t.Cleanup(func() { swapThrashWatchGlobal = previous })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		controlRuntime.runSamplerWith(ctx, time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for sensor.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sampler loop did not stop on cancel")
	}
	if sensor.calls.Load() == 0 {
		t.Fatal("the miner's sampler loop never sampled the swap-thrash watch")
	}
}
