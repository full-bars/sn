package miner

import (
	"context"
	"testing"
	"time"
)

// These tests cover the resource-pressure MONITOR: the sampler that turns host
// readings into the one smoothed score every pressure-driven actuator reads.
// The pacing math (fetchStretch, scaledProbeConcurrency, the stale thresholds,
// the AIMD controller) was already ported and pinned; without a writer for
// globalPressure all of it degenerated to its calm-box behavior.

// resetPressureForTest clears the published score so a test starts from the
// same calm state a fresh process has, and isolates the self-heal resolution
// from this host's real control state and marker file.
func resetPressureForTest(t *testing.T) {
	t.Helper()
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	resetGlobalControlStateForTest()
	setPressure(0)
	setPressureNoCPU(0)
	t.Cleanup(func() {
		setPressure(0)
		setPressureNoCPU(0)
	})
}

// saturatedSample is fabricated host data, not a reading: every sensor is in
// its worst band, so any monitor that publishes what it samples must report
// full pressure.
func saturatedSample() pressureSample {
	return pressureSample{
		PSIMem:         60,
		PSICPU:         60,
		PSIIO:          60,
		MemAvailFrac:   0.01,
		LoadPerCore:    4,
		Goroutines:     100,
		RunningProxies: 0,
		HeapFrac:       0.95,
		FDFrac:         0.95,
		Cores:          4,
	}
}

// TestPressureMonitorPublishesSampledPressureToActuators is the red test: feed
// one saturated fabricated sample through the monitor and prove the published
// score reaches both currentPressure() and the actuators that pace on it.
func TestPressureMonitorPublishesSampledPressureToActuators(t *testing.T) {
	resetPressureForTest(t)
	mon := newPressureMonitor()
	mon.sample = func() pressureSample { return saturatedSample() }

	if got := currentPressure(); got != 0 {
		t.Fatalf("precondition: fresh process pressure = %v, want 0", got)
	}

	mon.step(true)

	if got := currentPressure(); got < 0.99 {
		t.Fatalf("currentPressure() = %v after a fully saturated sample; the monitor did not publish", got)
	}

	// Every actuator below reads the same atomic. If any still sees a calm box,
	// the monitor is not wired to the thing that paces them.
	if got := fetchStretch(currentPressure()); got < fetchStretchMax {
		t.Fatalf("fetch stretch = %v at full pressure, want %v", got, fetchStretchMax)
	}
	if got := scaledProbeConcurrency(currentPressure()); got != 1 {
		t.Fatalf("probe concurrency = %d at full pressure, want 1", got)
	}
	if got := reaperStaleThreshold(currentPressure()); got != reaperStaleHot {
		t.Fatalf("reaper stale threshold = %v at full pressure, want %v", got, reaperStaleHot)
	}
	if got := paidStaleThreshold(currentPressure()); got != paidStaleHot {
		t.Fatalf("paid stale threshold = %v at full pressure, want %v", got, paidStaleHot)
	}
}

// TestPressureMonitorNoCPUExcludesCPUOnlyPressure pins the companion score the
// AIMD grow gate reads: CPU stalls alone must raise the main score but not the
// CPU-excluded one, or a busy single-core box can never regrow its pool.
func TestPressureMonitorNoCPUExcludesCPUOnlyPressure(t *testing.T) {
	resetPressureForTest(t)
	mon := newPressureMonitor()
	mon.sample = func() pressureSample {
		return pressureSample{
			PSICPU:       90,
			MemAvailFrac: 0.5, // half free: below the 25% ramp, so not pressure
			Goroutines:   100,
			Cores:        4,
		}
	}

	mon.step(true)

	if got := currentPressure(); got < 0.99 {
		t.Fatalf("currentPressure() = %v for saturated CPU PSI, want ~1", got)
	}
	if got := currentPressureNoCPU(); got != 0 {
		t.Fatalf("currentPressureNoCPU() = %v for a CPU-only sample, want 0", got)
	}
}

// TestPressureMonitorSelfHealOffPublishesCalm pins the relay semantics: with
// self-heal off the monitor still ticks but publishes zero, so every consumer
// behaves exactly like the pre-pressure code.
func TestPressureMonitorSelfHealOffPublishesCalm(t *testing.T) {
	resetPressureForTest(t)
	mon := newPressureMonitor()
	mon.sample = func() pressureSample { return saturatedSample() }

	mon.step(true)
	if got := currentPressure(); got < 0.99 {
		t.Fatalf("setup: currentPressure() = %v, want ~1", got)
	}

	mon.step(false)
	if got := currentPressure(); got != 0 {
		t.Fatalf("self-heal off: currentPressure() = %v, want 0", got)
	}
	if got := currentPressureNoCPU(); got != 0 {
		t.Fatalf("self-heal off: currentPressureNoCPU() = %v, want 0", got)
	}
}

// TestRunPressureMonitorSamplesOnItsInterval proves the loop itself is wired:
// the sampler is injectable and the interval is short enough that the test
// waits on the publish, never on a wall-clock sleep.
func TestRunPressureMonitorSamplesOnItsInterval(t *testing.T) {
	resetPressureForTest(t)

	oldInterval := pressureSampleInterval
	oldSampler := pressureSampler
	oldPublished := pressurePublished
	pressureSampleInterval = time.Millisecond
	pressureSampler = func() pressureSample { return saturatedSample() }
	published := make(chan float64, 1)
	pressurePublished = func(score float64) {
		select {
		case published <- score:
		default:
		}
	}
	t.Cleanup(func() {
		pressureSampleInterval = oldInterval
		pressureSampler = oldSampler
		pressurePublished = oldPublished
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPressureMonitor(ctx, true)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	select {
	case score := <-published:
		if score < 0.99 {
			t.Fatalf("loop published %v for a saturated sample, want ~1", score)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runPressureMonitor never sampled and published")
	}
}

// TestComputePressureFallsBackWithoutPSI covers the kernels with no PSI: the
// PSI fields stay zero and the load/memory sensors still produce a score.
func TestComputePressureFallsBackWithoutPSI(t *testing.T) {
	score, comps := computePressure(pressureSample{
		LoadPerCore: 3,
		Goroutines:  100,
		Cores:       2,
	})
	if score < 0.99 {
		t.Fatalf("load-only score = %v, want ~1 (comps %v)", score, comps)
	}
	if comps["psi_cpu"] != 0 || comps["psi_io"] != 0 || comps["psi_mem"] != 0 {
		t.Fatalf("PSI components should be zero without PSI: %v", comps)
	}
}

// TestParsePSISome covers the /proc/pressure parser, including the avg10 read
// the future watchdog will need.
func TestParsePSISome(t *testing.T) {
	const content = "some avg10=1.25 avg60=12.50 avg300=30.00 total=123456\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
	avg10, avg60, err := parsePSISome(content)
	if err != nil {
		t.Fatal(err)
	}
	if avg10 != 1.25 || avg60 != 12.50 {
		t.Fatalf("avg10=%v avg60=%v, want 1.25/12.5", avg10, avg60)
	}
	if _, _, err := parsePSISome("full avg10=0.00\n"); err == nil {
		t.Fatal("content without a 'some' line must error")
	}
}
