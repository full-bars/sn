package miner

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"time"

	"github.com/urfoundation/sn/internal/connectx"
)

// The resource-pressure MONITOR, ported from the v33 provider's
// resource_pressure.go.
//
// proxy_manager_pressure.go ported the pacing math and the AIMD pool
// controller but deliberately left the monitor as P2: nothing wrote
// globalPressure, so currentPressure() always read calm and every
// pressure-driven actuator (fetch stretch, probe concurrency, stale
// thresholds, pool shrink) ran as if the box were idle. This file is the
// missing writer: it samples the host, converts the readings into one smoothed
// score in [0,1], and publishes it through setPressure/setPressureNoCPU, the
// same atomics those actuators read.
//
// The semantics and thresholds are the relay's, unchanged: the worst component
// wins, asymmetric EWMA (fast rise, slow decay), emergency heap/goroutine pins
// that bypass smoothing, and the same PSI ramps. The GC governor and the
// thrash watchdog are separate actuators and are not part of the score.
//
// Sensors fail open. PSI is read from /proc/pressure when the kernel provides
// it; on kernels without PSI the load and memory sensors still score, which is
// the relay's fallback. On non-Linux hosts the /proc and cgroup reads fail, so
// only the memory sensor (where the platform reports it, e.g. macOS) and the
// process's own heap/goroutine signals score. A sensor that cannot be read
// contributes zero, never a false emergency, so a broken reader cannot pin the
// pool.

// pressureSampleInterval is how often the monitor takes a sample. A variable
// so tests can drive the real loop in milliseconds.
var pressureSampleInterval = 30 * time.Second

// pressureSampler is the monitor's sensor entry point. A variable so tests can
// feed a fabricated sample through the real monitor without touching the host.
var pressureSampler = collectPressureSample

// pressurePublished observes each published score after the atomics are
// written. A test seam so the loop's publish can be awaited instead of polled.
var pressurePublished func(float64)

const (
	// PSI `some avgXX` is "% of wall time at least one task stalled on this
	// resource". 10% = noticeable contention, 60% = severe. Same meaning on
	// any core count: PSI is self-normalizing.
	psiRampLo = 10.0
	psiRampHi = 60.0

	// CPU PSI on a SINGLE-CORE box sits in a busy-but-healthy band (~30-50)
	// during ordinary dialing, so the shared ramp scored normal operation as
	// sustained pressure and helped pin the pool. The quieter 1-core ramp;
	// multi-core keeps the shared PSI ramp, which self-normalizes across
	// cores there.
	cpu1CoreRampLo = 40.0
	cpu1CoreRampHi = 90.0

	// MemAvailable/MemTotal: plenty of page cache headroom above 25%,
	// reclaim death spiral territory below 5%.
	memAvailRampLo = 0.25 // score 0 at or above this fraction free
	memAvailRampHi = 0.05 // score 1 at or below this fraction free

	// loadavg per core (fallback sensor where PSI is unavailable).
	loadRampLo = 1.0
	loadRampHi = 3.0

	// Disk/IO PSI (same ramp as mem/cpu: some avg60 percent).
	ioRampLo = 10.0
	ioRampHi = 60.0

	// Self-signals: the provider's own runaway growth. With running proxies
	// the goroutine sensor is PER PROXY, not absolute: a healthy proxy costs
	// 20-27 goroutines, so a fixed ceiling pinned any healthy pool above
	// ~1,000 proxies at 1.00. The process-wide overhead (control socket,
	// monitors, metrics, GC workers) is subtracted first so a small pool is
	// not charged for it. The absolute ramp still applies when nothing is
	// running (direct-only node), and below goroutineMinPoolForPerProxy,
	// where dividing by a small pool is not meaningful: several self-heal
	// consumers of this score SHRINK the pool under pressure, which shrinks
	// the denominator and raises per-proxy further, a possible
	// positive-feedback loop on a small URL-sourced pool.
	goroutineFixedOverhead      = 1000
	goroutineMinPoolForPerProxy = 50
	goroutinePerProxyRampLo     = 60.0
	goroutinePerProxyRampHi     = 150.0
	emergencyGoroutinesPerProxy = 200.0
	goroutineRampLo             = 5000
	goroutineRampHi             = 25000
	heapRampLo                  = 0.60 // fraction of the max-memory soft limit
	heapRampHi                  = 0.90

	// Emergency pins: bypass EWMA smoothing entirely.
	emergencyHeapFrac   = 0.90
	emergencyGoroutines = 25000

	// Asymmetric EWMA: react to onset within a sample or two, take minutes of
	// sustained calm to relax. This is the hysteresis.
	ewmaAlphaRise  = 0.5
	ewmaAlphaDecay = 0.1
)

// pressureSample is one raw reading of every sensor. Zero values mean "no
// data" and normalize to zero pressure (fail-open).
type pressureSample struct {
	PSIMem         float64 // /proc/pressure/memory some avg60 (percent)
	PSICPU         float64 // /proc/pressure/cpu some avg60 (percent)
	PSIIO          float64 // /proc/pressure/io some avg60 (percent); 0 = unavailable
	MemAvailFrac   float64 // MemAvailable/MemTotal; 0 = unknown
	LoadPerCore    float64 // loadavg1 / NumCPU; 0 = unknown
	Goroutines     int
	RunningProxies int     // proxies this miner runs; the goroutine sensor judges per proxy when > 0
	HeapFrac       float64 // heap in use / max-memory soft limit; 0 = no limit set
	FDFrac         float64 // open FDs / RLIMIT_NOFILE; 0 = unavailable
	Cores          float64 // effective CPU count the CPU-PSI ramp is scaled by; 0 = unknown
	SensorErrs     map[string]error
}

// scoreExcludingCPU is the worst component with psi_cpu removed. The main
// score's emergency pins are heap/goroutine conditions, and those saturate
// their own components (the ramps top out exactly where the pins fire), so
// this loop alone honours emergencies; a saturated psi_cpu alone no longer
// blocks growth (the relay's scoring fix: CPU stalls may shrink the pool, but
// must not pin it below its ceiling forever).
func scoreExcludingCPU(comps map[string]float64) float64 {
	best := 0.0
	for k, v := range comps {
		if k == "psi_cpu" {
			continue
		}
		if v > best {
			best = v
		}
	}
	return best
}

// goroutinesPerProxy is the per-proxy goroutine cost after removing the fixed
// process overhead; ok is false when there are no running proxies to divide
// by, or too few for the division to be meaningful.
func goroutinesPerProxy(s pressureSample) (perProxy float64, ok bool) {
	if s.RunningProxies < goroutineMinPoolForPerProxy {
		return 0, false
	}
	return float64(max(0, s.Goroutines-goroutineFixedOverhead)) / float64(s.RunningProxies), true
}

// goroutineComponent scores runaway goroutine growth in [0,1].
func goroutineComponent(s pressureSample) float64 {
	if per, ok := goroutinesPerProxy(s); ok {
		return normalizeRamp(per, goroutinePerProxyRampLo, goroutinePerProxyRampHi)
	}
	return normalizeRamp(float64(s.Goroutines), goroutineRampLo, goroutineRampHi)
}

// goroutineEmergency reports a self-inflicted goroutine blowout that bypasses
// smoothing.
func goroutineEmergency(s pressureSample) bool {
	if per, ok := goroutinesPerProxy(s); ok {
		return per >= emergencyGoroutinesPerProxy
	}
	return s.Goroutines >= emergencyGoroutines
}

// computePressure converts one sample into a score plus its per-component
// breakdown. The worst component wins: averaging would dilute a memory crisis
// with a healthy CPU reading.
func computePressure(s pressureSample) (float64, map[string]float64) {
	comps := map[string]float64{
		"psi_mem": normalizeRamp(s.PSIMem, psiRampLo, psiRampHi),
		"psi_cpu": cpuPressureComponent(s.PSICPU, s.Cores),
		"psi_io":  normalizeRamp(s.PSIIO, ioRampLo, ioRampHi),
		"load":    normalizeRamp(s.LoadPerCore, loadRampLo, loadRampHi),
		"goro":    goroutineComponent(s),
	}
	if s.FDFrac > 0 {
		// FDFrac is the fraction of RLIMIT_NOFILE currently USED. 50% used =
		// score 0, 90% used = score 1: approaching FD exhaustion (a proxy's
		// load-bearing resource) raises pressure.
		comps["fd"] = normalizeRamp(s.FDFrac, 0.5, 0.9)
	}
	if s.MemAvailFrac > 0 {
		comps["mem"] = normalizeRamp(s.MemAvailFrac, memAvailRampLo, memAvailRampHi)
	}
	if s.HeapFrac > 0 {
		comps["heap"] = normalizeRamp(s.HeapFrac, heapRampLo, heapRampHi)
	}

	// Emergency pin: self-inflicted blowout bypasses smoothing.
	if (s.HeapFrac >= emergencyHeapFrac && s.HeapFrac > 0) || goroutineEmergency(s) {
		return 1.0, comps
	}

	score := 0.0
	for _, v := range comps {
		if v > score {
			score = v
		}
	}
	return score, comps
}

// ewmaUpdate applies asymmetric exponential smoothing.
func ewmaUpdate(prev, raw float64) float64 {
	alpha := ewmaAlphaDecay
	if raw > prev {
		alpha = ewmaAlphaRise
	}
	return prev + alpha*(raw-prev)
}

// cpuPressureComponent is the one CPU-PSI ramp, shared by computePressure and
// the relay's GC veto. cores selects the 1-core ramp.
func cpuPressureComponent(raw, cores float64) float64 {
	if raw <= 0 {
		return 0
	}
	if cores > 0 && cores <= 1.5 {
		return normalizeRamp(raw, cpu1CoreRampLo, cpu1CoreRampHi)
	}
	return normalizeRamp(raw, psiRampLo, psiRampHi)
}

// effectiveCores is the CPU count the pressure math should believe: NumCPU,
// capped by a cgroup cpu.max quota and by an explicit GOMAXPROCS, because both
// genuinely limit this process on an otherwise-big box.
func effectiveCores() float64 {
	cores := float64(runtime.NumCPU())
	if q, ok := cgroupCPUQuotaCores(); ok && q > 0 && q < cores {
		cores = q
	}
	if g := float64(runtime.GOMAXPROCS(0)); g > 0 && g < cores {
		cores = g
	}
	return cores
}

// parsePSISome extracts avg10 and avg60 from the "some" line of a
// /proc/pressure file.
func parsePSISome(content string) (avg10, avg60 float64, err error) {
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, field := range strings.Fields(line)[1:] {
			k, v, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch k {
			case "avg10":
				avg10, err = strconv.ParseFloat(v, 64)
			case "avg60":
				avg60, err = strconv.ParseFloat(v, 64)
			}
			if err != nil {
				return 0, 0, err
			}
		}
		return avg10, avg60, nil
	}
	return 0, 0, fmt.Errorf("no 'some' line in PSI content")
}

// readPSI returns the some avg60 pressure for one resource. It fails on
// kernels built without PSI and on non-Linux hosts, where /proc/pressure does
// not exist; callers leave that component at zero and the fallback sensors
// score instead.
// psiResourcePath resolves a PSI resource under /proc/pressure. It is a seam so
// a test can prove which resource a reader opens: "some" is a LINE inside the
// memory file, not a resource, so asking for it by name opens a path that never
// exists.
var psiResourcePath = func(resource string) string {
	return "/proc/pressure/" + resource
}

func readPSI(resource string) (avg60 float64, err error) {
	b, err := os.ReadFile(psiResourcePath(resource))
	if err != nil {
		return 0, err
	}
	_, avg60, err = parsePSISome(string(b))
	return avg60, err
}

// readMemAvailableMiB returns MemAvailable in MiB, or -1 when the host reader
// is unavailable.
func readMemAvailableMiB() int64 {
	v, ok := connectx.HostMemoryAvailableBytes()
	if !ok {
		return -1
	}
	return v / 1024 / 1024
}

// readCgroupAvailableMiB returns the free headroom within the active cgroup
// memory limit in MiB, or -1 if no cgroup limit is set. Necessary inside
// containers, where /proc/meminfo MemAvailable reflects host RAM, not the
// container limit.
func readCgroupAvailableMiB() int64 {
	const oneTiB = int64(1) << 40

	// cgroup v2: the process's own cgroup and its ancestors (systemd
	// MemoryMax=/MemoryHigh= live there, not at the mount root).
	if room, ok := connectx.CgroupMemoryHeadroomBytes(); ok && room < oneTiB {
		return room / 1024 / 1024
	}

	// cgroup v1
	limitData, limitErr := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")
	usageData, usageErr := os.ReadFile("/sys/fs/cgroup/memory/memory.usage_in_bytes")
	if limitErr == nil && usageErr == nil {
		limit, err1 := strconv.ParseInt(strings.TrimSpace(string(limitData)), 10, 64)
		usage, err2 := strconv.ParseInt(strings.TrimSpace(string(usageData)), 10, 64)
		if err1 == nil && err2 == nil && limit > 0 && limit < oneTiB {
			if avail := (limit - usage) / 1024 / 1024; avail >= 0 {
				return avail
			}
			return 0
		}
	}

	return -1
}

// readMemAvailFrac returns the available memory fraction this process can see,
// taking the tighter of host MemAvailable and cgroup headroom against the
// effective RAM. Host-only MemAvailable misleads inside Docker.
func readMemAvailFrac() (float64, error) {
	ram := detectEffectiveRAMLimitBytes()
	host := readMemAvailableMiB()
	if host < 0 && ram <= 0 {
		return 0, fmt.Errorf("cannot read host memory")
	}
	var availMiB float64
	cgroup := readCgroupAvailableMiB()
	// Use >= for host so a legitimately read MemAvailable==0 (memory fully
	// exhausted, right before OOM) still counts as max pressure rather than
	// indistinguishable from no-data. readMemAvailableMiB returns -1 on error,
	// so 0 is real data.
	switch {
	case host >= 0 && cgroup >= 0:
		availMiB = min(float64(host), float64(cgroup))
	case host >= 0:
		availMiB = float64(host)
	case cgroup >= 0:
		availMiB = float64(cgroup)
	default:
		return 0, fmt.Errorf("cannot read host or cgroup memory availability")
	}
	if ram <= 0 {
		return 0, fmt.Errorf("no RAM baseline for fraction")
	}
	return max(0, availMiB) * 1024 * 1024 / float64(ram), nil
}

// getSystemLoad reads /proc/loadavg and returns the 1-minute and 5-minute load
// averages. Returns an error on hosts without /proc; callers fail open.
func getSystemLoad() (load1, load5 float64, err error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(string(data))
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("unexpected /proc/loadavg format: %q", string(data))
	}
	load1, err = strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, 0, err
	}
	load5, err = strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0, 0, err
	}
	return load1, load5, nil
}

// cgroupV2SelfDir returns this process's cgroup v2 directory under
// /sys/fs/cgroup, or "" when /proc/self/cgroup has no v2 line (cgroup v1
// host, or no procfs).
func cgroupV2SelfDir() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		rel := strings.TrimSpace(strings.TrimPrefix(line, "0::"))
		if rel == "" {
			rel = "/"
		}
		return filepath.Join("/sys/fs/cgroup", rel)
	}
	return ""
}

// cgroupCPUQuotaCores reads the effective cgroup v2 CPU quota in cores: the
// TIGHTEST cpu.max among this process's own cgroup and every ancestor up to
// /sys/fs/cgroup, because each level constrains its subtree (a systemd unit
// lives in a slice; the quota may sit there, not at the unit's own cgroup or
// the mount root). ok=false when nothing in the chain is quota'd.
func cgroupCPUQuotaCores() (float64, bool) {
	best := 0.0
	check := func(dir string) {
		b, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
		if err != nil {
			return
		}
		f := strings.Fields(string(b))
		if len(f) != 2 || f[0] == "max" {
			return
		}
		quota, err1 := strconv.ParseFloat(f[0], 64)
		period, err2 := strconv.ParseFloat(f[1], 64)
		if err1 != nil || err2 != nil || period <= 0 || quota <= 0 {
			return
		}
		if cores := quota / period; best == 0 || cores < best {
			best = cores
		}
	}
	dir := cgroupV2SelfDir()
	if dir == "" {
		dir = "/sys/fs/cgroup"
	}
	for strings.HasPrefix(dir, "/sys/fs/cgroup") {
		check(dir)
		if dir == "/sys/fs/cgroup" {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if best > 0 {
		return best, true
	}
	return 0, false
}

// readLiveHeapBytes returns the runtime's live-heap estimate (non-STW) via
// /gc/heap/live:bytes. Returns 0 if unavailable.
func readLiveHeapBytes() uint64 {
	samples := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(samples)
	if samples[0].Value.Kind() == metrics.KindUint64 {
		return samples[0].Value.Uint64()
	}
	return 0
}

// readStackBytes returns the bytes of memory the runtime holds for goroutine
// stacks, or 0 if unavailable.
func readStackBytes() uint64 {
	samples := []metrics.Sample{{Name: "/memory/classes/heap/stacks:bytes"}}
	metrics.Read(samples)
	if samples[0].Value.Kind() == metrics.KindUint64 {
		return samples[0].Value.Uint64()
	}
	return 0
}

// heapFracFrom is the one definition of the heap fraction: live heap plus
// stacks over the memory budget. 0 when either the live reading or the limit
// is unavailable.
func heapFracFrom(live, stacks uint64, limit int64) float64 {
	if live == 0 || limit <= 0 {
		return 0
	}
	return float64(live+stacks) / float64(limit)
}

// liveHeapFrac is the heap fraction of the memory budget. It is deliberately
// NOT Sys-HeapReleased: that tracks the heap goal, which moves with GOGC, so a
// box merely sitting at its soft limit would read as an emergency. The limit
// is the runtime soft limit when one is set, else the effective RAM.
func liveHeapFrac() float64 {
	limit := debug.SetMemoryLimit(-1) // query only: -1 returns the current limit
	if limit <= 0 || limit >= math.MaxInt64 {
		if ram := detectEffectiveRAMLimitBytes(); ram > 0 {
			limit = ram
		}
	}
	return heapFracFrom(readLiveHeapBytes(), readStackBytes(), limit)
}

// collectPressureSample reads every sensor, recording errors per sensor so one
// missing source (PSI on old kernels, everything but the runtime signals on
// Windows/macOS) never blanks the others. Self-signals always work.
func collectPressureSample() pressureSample {
	s := pressureSample{
		SensorErrs:     map[string]error{},
		Goroutines:     runtime.NumGoroutine(),
		RunningProxies: len(runningProxyAddresses()),
		Cores:          effectiveCores(),
	}

	if v, err := readPSI("memory"); err == nil {
		s.PSIMem = v
	} else {
		s.SensorErrs["psi_mem"] = err
	}
	if v, err := readPSI("cpu"); err == nil {
		s.PSICPU = v
	} else {
		s.SensorErrs["psi_cpu"] = err
	}
	if v, err := readPSI("io"); err == nil {
		s.PSIIO = v
	} else {
		s.SensorErrs["psi_io"] = err
	}
	if v := readFDFrac(); v >= 0 {
		s.FDFrac = v
	} else {
		s.SensorErrs["fd"] = fmt.Errorf("cannot read FD usage")
	}
	if v, err := readMemAvailFrac(); err == nil {
		s.MemAvailFrac = v
	} else {
		s.SensorErrs["mem"] = err
	}
	if l1, _, err := getSystemLoad(); err == nil && runtime.NumCPU() > 0 {
		s.LoadPerCore = l1 / float64(runtime.NumCPU())
	} else if err != nil {
		s.SensorErrs["load"] = err
	}
	s.HeapFrac = liveHeapFrac()
	return s
}

// pressureRegime buckets the score for change-only logging.
func pressureRegime(score float64) int {
	switch {
	case score >= 0.75:
		return 3
	case score >= 0.5:
		return 2
	case score >= 0.25:
		return 1
	default:
		return 0
	}
}

// formatComponents renders the per-sensor breakdown in a stable order for the
// regime-change log line.
func formatComponents(comps map[string]float64) string {
	parts := make([]string, 0, len(comps))
	for _, k := range []string{"psi_mem", "psi_cpu", "psi_io", "mem", "load", "goro", "heap", "fd"} {
		if v, ok := comps[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%.2f", k, v))
		}
	}
	return strings.Join(parts, " ")
}

// pressureMonitor owns the smoothing state between samples. The zero value is
// not usable: newPressureMonitor installs the sampler seam.
type pressureMonitor struct {
	sample        func() pressureSample
	smoothed      float64
	smoothedNoCPU float64
	lastRegime    int
}

func newPressureMonitor() *pressureMonitor {
	return &pressureMonitor{sample: pressureSampler}
}

// step takes one sample, smooths it, and publishes it. With self-heal off it
// publishes zero and idles, exactly like the relay: every consumer then
// behaves as the pre-pressure code did.
func (m *pressureMonitor) step(selfHealEnabled bool) {
	if !resolveSelfHealEnabled(selfHealEnabled) {
		m.smoothed = 0
		m.smoothedNoCPU = 0
		m.lastRegime = 0
		setPressure(0)
		setPressureNoCPU(0)
		if pressurePublished != nil {
			pressurePublished(0)
		}
		return
	}

	sample := m.sample()
	raw, comps := computePressure(sample)
	if raw >= 1.0 {
		m.smoothed = 1.0 // emergency pin bypasses the slow rise
	} else {
		m.smoothed = ewmaUpdate(m.smoothed, raw)
	}
	setPressure(m.smoothed)

	// CPU-excluded companion score for the pool GROW gate: CPU stalls may
	// shrink the pool, but must not block regrowth on a busy-but-healthy box.
	rawNoCPU := scoreExcludingCPU(comps)
	if rawNoCPU >= 1.0 {
		m.smoothedNoCPU = 1.0
	} else {
		m.smoothedNoCPU = ewmaUpdate(m.smoothedNoCPU, rawNoCPU)
	}
	setPressureNoCPU(m.smoothedNoCPU)

	if pressurePublished != nil {
		pressurePublished(m.smoothed)
	}
	if r := pressureRegime(m.smoothed); r != m.lastRegime {
		tlog("[proxy][pressure] pressure %.2f (%s)\n", m.smoothed, formatComponents(comps))
		m.lastRegime = r
	}
}

// runPressureMonitor samples every pressureSampleInterval, smooths the score,
// publishes it, and logs on regime changes. The sampler and the interval are
// both seams, so a test drives this loop without waiting on the host.
//
// On exit it publishes calm: a dead monitor must fail neutral instead of
// freezing its last (possibly emergency) reading in force.
func runPressureMonitor(ctx context.Context, selfHealEnabled bool) {
	m := newPressureMonitor()

	// Log the active sensor set once at startup, as the relay does.
	first := m.sample()
	active := []string{"goro"}
	if first.HeapFrac > 0 {
		active = append(active, "heap")
	}
	for _, name := range []string{"psi_mem", "psi_cpu", "psi_io", "mem", "load", "fd"} {
		if _, bad := first.SensorErrs[name]; !bad {
			active = append(active, name)
		}
	}
	tlog("[proxy][pressure] monitor started, sensors: %s (self-heal %v)\n",
		strings.Join(active, ", "), resolveSelfHealEnabled(selfHealEnabled))

	defer func() {
		setPressure(0)
		setPressureNoCPU(0)
	}()

	ticker := time.NewTicker(pressureSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		m.step(selfHealEnabled)
	}
}
