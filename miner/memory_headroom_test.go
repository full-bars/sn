package miner

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The memory-headroom guard is the miner's half of the RAM story: H3 roughly
// doubles a proxy's footprint and is admission-controlled per identity, so the
// box can go from comfortable to swapping without the proxy count moving at
// all. These tests pin the provider's thresholds and hysteresis, and then prove
// the guard is actually wired into the monitor loop and reports from a known
// memory state.

// A 2 GiB box's free memory dipped to 132 MB at 590 connected clients and
// nothing said so: the pressure score read a fixed 1.00 all day, so it carried
// no signal. A headroom tracker on real free memory logs when the box gets
// short and again when it recovers, with hysteresis so a reading that hovers at
// the line does not spam the log.
func TestHeadroomLowThreshold(t *testing.T) {
	cases := map[int64]int64{
		512:   150, // tiny box: the absolute floor
		963:   150, // 1 GB box
		1499:  150, // 10% is still under the floor
		1500:  150, // exactly the floor
		1501:  150, // the floor, until 10% clears it
		1930:  193, // 2 GiB box: 10% of RAM
		3999:  399, // just under the cap's 10% line
		4000:  400, // capped
		4001:  400, // 10% is past the cap
		24000: 400,
	}
	for ram, want := range cases {
		if got := headroomLowThresholdMiB(ram); got != want {
			t.Errorf("RAM %d MiB: threshold %d, want %d", ram, got, want)
		}
	}
}

func TestHeadroomTrackerEntersAfterTwoSamplesAndLeavesAfterFourWithHysteresis(t *testing.T) {
	var h headroomTracker
	const low = int64(193)
	observe := func(avail int64) string { return h.Observe(avail, low) }

	if got := observe(600); got != "" {
		t.Fatalf("healthy: %q", got)
	}
	if got := observe(142); got != "" {
		t.Fatalf("one low sample must not trigger: %q", got)
	}
	if got := observe(150); got != "low" {
		t.Fatalf("second consecutive low sample must trigger, got %q", got)
	}
	if got := observe(120); got != "" {
		t.Fatalf("already low: must not re-announce, got %q", got)
	}
	// Recovery needs to clear the threshold by 25% (241 MiB) four times running.
	for i, avail := range []int64{230, 300, 300, 300} {
		if got := observe(avail); got != "" {
			t.Fatalf("recovery sample %d (%d MiB) announced early: %q", i, avail, got)
		}
	}
	// 230 MiB is below the 241 MiB recovery line and resets the streak; the four
	// consecutive readings that follow (300, 300, 300, then 320) are the clear run.
	if got := observe(320); got != "recovered" {
		t.Fatalf("fourth consecutive clear sample must announce recovery, got %q", got)
	}
	if h.low {
		t.Fatalf("tracker must be back to normal")
	}
}

func TestHeadroomTrackerDoesNotFlapAtTheLine(t *testing.T) {
	var h headroomTracker
	const low = int64(200)
	transitions := 0
	// Hovering across the threshold but never clearing it by the recovery margin.
	for i := 0; i < 60; i++ {
		avail := int64(190)
		if i%2 == 1 {
			avail = 210
		}
		if h.Observe(avail, low) != "" {
			transitions++
		}
	}
	if transitions > 1 {
		t.Fatalf("hovering at the line produced %d transitions, want at most the single entry", transitions)
	}
}

func TestHeadroomTrackerIgnoresUnknownReadings(t *testing.T) {
	var h headroomTracker
	h.Observe(100, 193)
	for i := 0; i < 10; i++ {
		if got := h.Observe(-1, 193); got != "" {
			t.Fatalf("unknown reading produced %q", got)
		}
	}
	if got := h.Observe(100, 193); got != "low" {
		t.Fatalf("an unknown reading must not reset the streak, got %q", got)
	}
}

func TestHeadroomLogLine(t *testing.T) {
	low := headroomLogLine("low", 132, 193, 2001, 77780)
	for _, want := range []string{"[proxy][resources]", "low memory headroom", "132 MiB available", "below 193 MiB", "2001 proxies", "77780 goroutines"} {
		if !strings.Contains(low, want) {
			t.Errorf("low line %q missing %q", low, want)
		}
	}
	rec := headroomLogLine("recovered", 420, 193, 2001, 41000)
	if !strings.Contains(rec, "memory headroom recovered") || !strings.Contains(rec, "420 MiB available") {
		t.Errorf("recovered line: %q", rec)
	}
	if headroomLogLine("", 1, 1, 1, 1) != "" {
		t.Errorf("no transition must produce no line")
	}
	// The line must be one the important-log filter keeps, or the announcement
	// dies with the ramlog on a reboot and the operator never sees it.
	if !isImportantLogLine(low) || !isImportantLogLine(rec) {
		t.Errorf("headroom lines must match an important-log marker")
	}
}

// stubHostAvail swaps the shared free-memory reading for a fabricated one and
// restores it.
func stubHostAvail(t *testing.T, avail func() int64) {
	t.Helper()
	prev := hostAvailMiB
	hostAvailMiB = avail
	t.Cleanup(func() { hostAvailMiB = prev })
}

// TestHostAvailMiBTakesTheTighterOfHostAndCgroup pins the one number the guard
// and the pressure fraction share. A container's /proc/meminfo reports the
// host's free memory, so trusting it alone would have the guard report plenty
// of headroom while the cgroup limit is nearly full.
func TestHostAvailMiBTakesTheTighterOfHostAndCgroup(t *testing.T) {
	prevHost, prevCgroup := hostMemAvailableMiB, cgroupAvailableMiB
	t.Cleanup(func() {
		hostMemAvailableMiB = prevHost
		cgroupAvailableMiB = prevCgroup
	})

	cases := []struct {
		name     string
		host, cg int64
		want     int64
	}{
		{"cgroup tighter", 2000, 300, 300},
		{"host tighter", 200, 1000, 200},
		{"bare metal, no cgroup limit", 2000, -1, 2000},
		{"container cannot read meminfo", -1, 300, 300},
		{"nothing readable", -1, -1, -1},
		{"real zero is data, not no-data", -1, 0, 0},
	}
	for _, c := range cases {
		host, cg := c.host, c.cg
		hostMemAvailableMiB = func() int64 { return host }
		cgroupAvailableMiB = func() int64 { return cg }
		if got := readHostAvailMiB(); got != c.want {
			t.Errorf("%s: host=%d cgroup=%d gave %d, want %d", c.name, host, cg, got, c.want)
		}
	}
}

// TestReadMemAvailFracReadsThroughHostAvailMiB proves the pressure fraction
// takes its numerator from the shared hostAvailMiB reading -- the tighter of
// host and cgroup, the same definition the headroom guard uses -- instead of
// reading host memory itself. It does NOT prove the guard and the fraction see
// the same number in one tick: each takes its own read, milliseconds apart, so
// on a box in flux they can differ.
func TestReadMemAvailFracReadsThroughHostAvailMiB(t *testing.T) {
	ram := detectEffectiveRAMLimitBytes()
	if ram <= 0 {
		t.Skip("no RAM baseline on this host")
	}
	prevHost, prevCgroup := hostMemAvailableMiB, cgroupAvailableMiB
	t.Cleanup(func() {
		hostMemAvailableMiB = prevHost
		cgroupAvailableMiB = prevCgroup
	})
	hostReads, cgroupReads := 0, 0
	hostMemAvailableMiB = func() int64 { hostReads++; return 800 }
	cgroupAvailableMiB = func() int64 { cgroupReads++; return 200 }

	frac, err := readMemAvailFrac()
	if err != nil {
		t.Fatalf("readMemAvailFrac: %v", err)
	}
	// 200 MiB (the cgroup limit) is the binding number: a private host read
	// would have reported 800 MiB and hidden the container's shortage.
	want := float64(200) * 1024 * 1024 / float64(ram)
	if frac != want {
		t.Fatalf("fraction %v, want the cgroup-tighter %v", frac, want)
	}
	if hostReads != 1 || cgroupReads != 1 {
		t.Fatalf("fraction read host=%d cgroup=%d times, want exactly one of each", hostReads, cgroupReads)
	}

	// Neither source readable is a sensor error, not a zero fraction: an
	// unreadable host must not read as maximum pressure.
	hostMemAvailableMiB = func() int64 { return -1 }
	cgroupAvailableMiB = func() int64 { return -1 }
	if _, err := readMemAvailFrac(); err == nil {
		t.Fatal("unreadable memory produced a fraction instead of an error")
	}
}

// TestHeadroomLowForRAMLimit pins the threshold when the RAM limit is
// unreadable: it must be the 400 MiB cap, never the 150 MiB floor. Shifting an
// unreadable limit (0 >> 20) into the threshold used to clamp to the floor,
// which left a large host swapping with nothing logged: an 8 GiB box at 350 or
// 200 MiB free is exactly the state the guard exists to catch.
func TestHeadroomLowForRAMLimit(t *testing.T) {
	cases := []struct {
		name     string
		ramBytes int64
		want     int64
	}{
		{"unreadable, zero", 0, headroomLowCapMiB},
		{"unreadable, negative", -1, headroomLowCapMiB},
		{"8 GiB box", 8 << 30, 400}, // 8192 MiB: 10% is capped
		{"2 GiB box", 2 << 30, 204}, // 2048 MiB: 10%
		{"1 GiB box", 1 << 30, 150}, // 1024 MiB: 10% is under the floor
	}
	for _, c := range cases {
		if got := headroomLowForRAMLimit(c.ramBytes); got != c.want {
			t.Errorf("%s: %d bytes gave %d MiB, want %d", c.name, c.ramBytes, got, c.want)
		}
	}
}

// stubEffectiveRAM swaps the effective RAM limit the monitor sizes its headroom
// threshold from.
func stubEffectiveRAM(t *testing.T, ram func() int64) {
	t.Helper()
	prev := effectiveRAMLimitBytes
	effectiveRAMLimitBytes = ram
	t.Cleanup(func() { effectiveRAMLimitBytes = prev })
}

// stubHeadroomProxyAddresses swaps the running-pool enumeration the headroom
// line counts, so a test can count how often it runs.
func stubHeadroomProxyAddresses(t *testing.T, addrs func() []string) {
	t.Helper()
	prev := headroomProxyAddresses
	headroomProxyAddresses = addrs
	t.Cleanup(func() { headroomProxyAddresses = prev })
}

// startHeadroomMonitor runs the real monitor loop on a 2ms tick with its
// importantLogf output captured. Stub every reading first: the loop starts
// immediately. Every seam it touches is restored only after the loop has
// returned, so none is swapped under a running goroutine.
func startHeadroomMonitor(t *testing.T) <-chan string {
	t.Helper()
	resetPressureForTest(t)

	prevInterval := pressureSampleInterval
	pressureSampleInterval = 2 * time.Millisecond

	lines := make(chan string, 256)
	prevHook := importantLogHook
	importantLogHook = func(line string) {
		select {
		case lines <- line:
		default:
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPressureMonitor(ctx, false)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		importantLogHook = prevHook
		pressureSampleInterval = prevInterval
	})
	return lines
}

// waitHeadroomLine waits for a captured line containing want.
func waitHeadroomLine(t *testing.T, lines <-chan string, want string) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case line := <-lines:
			if strings.Contains(line, want) {
				return line
			}
		case <-deadline:
			t.Fatalf("no %q line within 10s", want)
			return ""
		}
	}
}

// TestMemoryHeadroomGuardReportsFromKnownMemoryState drives the real monitor
// loop from a fabricated memory reading: the guard must announce "low" once the
// box is short and "recovered" once it clears, through the same importantLogf
// path the operator's events.log reads. Self-heal is OFF throughout, because
// the provider observes headroom before its self-heal gate -- a box running out
// of memory is worth knowing about whether or not the actuators are armed.
func TestMemoryHeadroomGuardReportsFromKnownMemoryState(t *testing.T) {
	// 100 MiB is below every possible threshold (150-400 MiB); 600 MiB clears
	// even the 400 MiB cap's 25% recovery margin (500 MiB).
	var avail atomic.Int64
	avail.Store(600)
	stubHostAvail(t, func() int64 { return avail.Load() })

	lines := startHeadroomMonitor(t)

	// A healthy box announces nothing.
	select {
	case line := <-lines:
		if strings.Contains(line, "headroom") {
			t.Fatalf("healthy box (%d MiB) announced %q", avail.Load(), line)
		}
	case <-time.After(100 * time.Millisecond):
	}

	avail.Store(100)
	low := waitHeadroomLine(t, lines, "low memory headroom")
	for _, want := range []string{"[proxy][resources]", "100 MiB available", "below", "goroutines"} {
		if !strings.Contains(low, want) {
			t.Errorf("low line %q missing %q", low, want)
		}
	}

	// Still low: the guard must not re-announce on every tick.
	select {
	case line := <-lines:
		if strings.Contains(line, "low memory headroom") {
			t.Fatalf("re-announced while still low: %q", line)
		}
	case <-time.After(100 * time.Millisecond):
	}

	avail.Store(600)
	rec := waitHeadroomLine(t, lines, "memory headroom recovered")
	if !strings.Contains(rec, "600 MiB available") {
		t.Errorf("recovered line %q does not report the reading", rec)
	}
}

// TestMemoryHeadroomGuardUsesTheCapWhenRAMIsUnreadable drives the real loop
// with a failed RAM detection. The threshold must be the 400 MiB cap: at 350 MiB
// free an 8 GiB host is already giving memory back to the kernel, yet the old
// 0>>20 path clamped to the 150 MiB floor and logged nothing -- nor would it at
// 200 MiB, with swap already in play.
func TestMemoryHeadroomGuardUsesTheCapWhenRAMIsUnreadable(t *testing.T) {
	stubEffectiveRAM(t, func() int64 { return 0 })
	stubHeadroomProxyAddresses(t, func() []string { return nil })

	var avail atomic.Int64
	avail.Store(350)
	stubHostAvail(t, func() int64 { return avail.Load() })

	lines := startHeadroomMonitor(t)

	low := waitHeadroomLine(t, lines, "low memory headroom")
	if !strings.Contains(low, "below 400 MiB") {
		t.Fatalf("unreadable-RAM line %q must carry the 400 MiB cap, not the floor", low)
	}
	if !strings.Contains(low, "350 MiB available") {
		t.Errorf("line %q does not report the reading", low)
	}
}

// TestMemoryHeadroomGuardEnumeratesProxiesOnlyOnTransition proves the pool count
// is taken on a transition and never on a steady-state tick. Counting the pool
// copies the running set, dedups it into a map and sorts it under the bandwidth
// registry lock; on a multi-thousand-proxy node that is real work, and a tick
// with no transition discards the count anyway.
func TestMemoryHeadroomGuardEnumeratesProxiesOnlyOnTransition(t *testing.T) {
	var calls atomic.Int64
	stubHeadroomProxyAddresses(t, func() []string {
		calls.Add(1)
		// The direct transport is a goroutine, not a pool member: it must not
		// be counted, so the line below says 2, not 3.
		return []string{directProxyKey, "1.1.1.1:1", "2.2.2.2:2"}
	})

	var avail atomic.Int64
	avail.Store(600)
	stubHostAvail(t, func() int64 { return avail.Load() })
	stubEffectiveRAM(t, func() int64 { return 8 << 30 }) // threshold 400 MiB

	lines := startHeadroomMonitor(t)

	// Healthy ticks: several samples, no transition, so no enumeration at all.
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("healthy ticks enumerated the pool %d times, want 0", got)
	}

	// 300 MiB is below the cap and below its 500 MiB recovery margin.
	avail.Store(300)
	low := waitHeadroomLine(t, lines, "low memory headroom")
	if !strings.Contains(low, "2 proxies") {
		t.Errorf("low line %q must report the pool size less the direct transport", low)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("entry enriched the pool %d times, want exactly 1", got)
	}

	// Still low: further ticks must not enumerate again.
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("steady low ticks enumerated the pool %d times, want 1", got)
	}

	// The recovery transition is the second and last reason to count it.
	avail.Store(600)
	waitHeadroomLine(t, lines, "memory headroom recovered")
	if got := calls.Load(); got != 2 {
		t.Fatalf("after recovery the pool was enumerated %d times, want 2 (one per transition)", got)
	}
}
