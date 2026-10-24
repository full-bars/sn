package miner

import (
	"fmt"
	"math"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/urfoundation/sn/internal/connectx"
	"github.com/urnetwork/connect"
)

// resource_config_warn.go compares the memory limits this miner was started
// with against an estimate of what the proxy pool it is about to launch needs,
// and reports a mismatch once, at startup. Ported from the provider's
// provider/resource_config_warn.go: two 1 GB boxes ran 1,300 proxies under a
// hand-pinned GOMEMLIMIT=400MiB and a 450M cgroup MemoryHigh, the live heap sat
// above its own soft limit, and nothing said so. The miner accepts a proxy
// count and had no equivalent sanity check, so the same misconfiguration just
// failed later in a confusing way (GC-bound CPU, swapping, OOM kills).
//
// The miner never changes these values (they are the operator's, or systemd's);
// it only makes the mismatch visible. Warnings only: nothing here can refuse a
// start, change a limit, or drop a proxy. The work is a few file reads plus
// arithmetic, done once per process start.
//
// Two separate checks, because the two numbers answer different questions:
//
//   - GOMEMLIMIT is a HEAP limit. It is compared against the Go heap the pool
//     needs, never against the whole-process footprint.
//   - The box's RAM is compared against the whole-process footprint, because
//     that is what the kernel has to hold: a long-running provider identity
//     also carries its connections, buffers, goroutine stacks and the clients
//     routed through it.
//
// Measuring one number and holding it against both limits was the bug this file
// previously had in the provider: it warned on the fleet's top earner, which was
// healthy, and stayed silent on a box thrashing its memory at pressure 1.0.
//
// Miner mapping of the provider's inputs:
//
//   - The pool size is the number of proxy slots THIS start launches.
//     provide() has already applied the operator's `proxy trim` cap to the
//     launched list (proxy_manager.go), so a held-back slot is not counted as a
//     proxy this box has to carry.
//   - The heap limit is the Go soft limit in force, whichever source set it:
//     GOMEMLIMIT in the environment, a persisted `urnet-tools set gomemlimit`
//     (replayed at startup, gomemlimit_startup.go), --max-memory, a profile
//     tier (eco 75%, turbo 80%) or the default provider memory plan
//     (provider_memory.go). It is read after applyProfileProcessTuning, so it
//     is the limit the process actually runs under, not the pre-tier value.
//   - The RAM ceiling is the tightest cgroup v2 memory.max/memory.high on this
//     process's cgroup or an ancestor, else cgroup v1, else the host's physical
//     memory (see warningRAMCeiling). That can be TIGHTER than what the tier
//     code sizes against (hostMemoryByteCount reads memory.max only), and the
//     difference is exactly what an operator needs to see: systemd MemoryHigh
//     throttles a process it never kills.
//
// One provider check is deliberately NOT ported: "GOGC is pinned in the
// environment, which disables the adaptive GC governor under
// URNETWORK_PROFILE=auto". The miner has no GC governor
// (metrics_resources.go reports Tightening=false and
// proxy_manager_pressure.go records that it was not ported), so GOGC in the
// environment disables nothing here and the warning would be false. When the
// governor is ported, that check belongs next to this one.
//
// The provider's ramCeilingLogLine is ported too (see the bottom of this file):
// it is not a check, but it is the only line that says which limit produced the
// RAM number a warning names.

const (
	// resourceBaseOverhead is the fixed cost of the process with no proxies
	// (control socket, metrics, stores, pools, the direct slot).
	resourceBaseOverhead = 100 << 20
	// resourceHeapBytesPerProxy is the Go heap one long-running provider
	// identity carries. Sampled from the provider fleet after days of uptime,
	// not fresh restarts: heap per proxy ran from about 0.4 to 1.2 MiB with a
	// median near 0.65 and the upper quartile near 1. It already includes the
	// average share of the connected clients, so no separate client allowance
	// is added on top. The miner launches the same provider identities through
	// the same connect SDK, so the sample carries over.
	resourceHeapBytesPerProxy = 665600
	// resourceFootprintBytesPerProxy is the whole-process footprint (RSS plus
	// swap) one long-running proxy carries: a median near 1.03 MiB across the
	// provider's main fleet and 1.15 on its Taco fleet. 0.9 MiB keeps the
	// estimate under the measured median while still clearing the reserve on
	// healthy boxes.
	resourceFootprintBytesPerProxy = 943718
	// resourceRAMReserveFloor is the smallest slice of RAM held back for the OS
	// and co-tenants; resourceRAMReservePercent is the share of RAM held back
	// on a box large enough for the percentage to matter.
	resourceRAMReserveFloor     = 300 << 20
	resourceRAMReservePercent   = 10
	resourceRAMReservePercentOf = 100
)

// resourceConfigInput is everything the check needs; zero means "not set".
type resourceConfigInput struct {
	Proxies int // proxy slots this start launches
	// GoMemLimit is a finite GOMEMLIMIT in bytes, 0 = none.
	GoMemLimit int64
	// EffectiveRAMBytes is the RAM ceiling this process actually runs under
	// (warningRAMCeiling). 0 = unknown.
	EffectiveRAMBytes int64
}

// estimatedPoolHeap is the Go heap a pool of n proxies is expected to need. It
// is what GOMEMLIMIT is compared against, and only that.
func estimatedPoolHeap(n int) int64 {
	if n < 0 {
		n = 0
	}
	return resourceBaseOverhead + int64(n)*resourceHeapBytesPerProxy
}

// estimatedPoolFootprint is the whole-process footprint a pool of n proxies is
// expected to need. It is what the box's RAM is compared against, and only
// that. Note it includes swap while a cgroup memory.max does not, so a
// cgroup-limited box is the least validated case of this check: no sampled
// node had a MemoryMax, so no sample pins that path.
func estimatedPoolFootprint(n int) int64 {
	if n < 0 {
		n = 0
	}
	return resourceBaseOverhead + int64(n)*resourceFootprintBytesPerProxy
}

// ramReserve is what the process leaves for the OS and every other tenant on
// the box: a floor on small boxes, a share of RAM on large ones. Never more
// than the box actually has, so a provider in a container capped below the
// floor reserves what it has rather than reporting a negative remainder.
func ramReserve(ram int64) int64 {
	if ram <= 0 {
		return 0
	}
	byPercent := ram * resourceRAMReservePercent / resourceRAMReservePercentOf
	if byPercent > resourceRAMReserveFloor {
		return byPercent
	}
	if resourceRAMReserveFloor > ram {
		return ram
	}
	return resourceRAMReserveFloor
}

// resourceConfigWarnings returns one human-readable warning per problem, in a
// stable order: heap against the soft memory limit, footprint against the box's
// RAM. A sane config returns nil.
func resourceConfigWarnings(in resourceConfigInput) []string {
	var out []string
	if in.GoMemLimit > 0 {
		need := estimatedPoolHeap(in.Proxies)
		if in.GoMemLimit < need {
			// Never advise raising the limit above what the box has. When the
			// box RAM is unknown, name no number rather than say "0 MiB".
			raise := "Raise it, but never above what this box actually has, or lower the pool (`urnet-tools proxy trim <count>`)"
			if ram := ramCeilingMiB(in); ram > 0 {
				raise = fmt.Sprintf("Raise it towards the %d MiB this box actually has (edit GOMEMLIMIT or --max-memory in the systemd unit, or `urnet-tools set gomemlimit` if the unit sets neither), or lower the pool (`urnet-tools proxy trim <count>`)", ram)
			}
			out = append(out, fmt.Sprintf(
				"GOMEMLIMIT=%d MiB is below the ~%d MiB of heap this pool of %d proxies is expected to need, so the runtime will spend its CPU on GC. %s",
				in.GoMemLimit>>20, need>>20, in.Proxies, raise))
		}
	}
	if in.EffectiveRAMBytes > 0 {
		need := estimatedPoolFootprint(in.Proxies)
		avail := in.EffectiveRAMBytes - ramReserve(in.EffectiveRAMBytes)
		if need > avail {
			out = append(out, fmt.Sprintf(
				"this pool of %d proxies is expected to need about %d MiB of memory but this box gives the provider about %d MiB (%d MiB left after holding back %d MiB for the OS and other tenants), so expect swapping and stalls. Lower the pool (`urnet-tools proxy trim <count>`) or move the provider to a larger box",
				in.Proxies, need>>20, in.EffectiveRAMBytes>>20, avail>>20, ramReserve(in.EffectiveRAMBytes)>>20))
		}
	}
	return out
}

// ramCeilingMiB is the box's own RAM ceiling, or 0 when unknown, for wording.
func ramCeilingMiB(in resourceConfigInput) int64 {
	return in.EffectiveRAMBytes >> 20
}

// resourceConfigInputFrom builds the warning input from values the caller
// already holds, so the check can be exercised without a real cgroup, a real
// process limit, or a start.
func resourceConfigInputFrom(proxies int, goMemLimit, effectiveRAM int64) resourceConfigInput {
	return resourceConfigInput{
		Proxies:           proxies,
		GoMemLimit:        goMemLimit,
		EffectiveRAMBytes: effectiveRAM,
	}
}

// warningRAMCeiling is the RAM ceiling the warning compares the pool against,
// with the name of the limit that set it. It is connectx.EffectiveRAMLimit (the
// tightest cgroup v2 memory.max/memory.high on this process's cgroup or an
// ancestor, then cgroup v1, then host physical memory) rather than
// detectEffectiveRAMLimitBytes, because the miner's own reader does not see
// memory.high and the incident this check exists for ran under a 450M
// MemoryHigh.
//
// The 850 MiB default connectx falls back to when nothing can be read is
// reported as unknown (0) here: a warning that names a RAM figure the box does
// not have is worse than no warning at all.
func warningRAMCeiling() (int64, string) {
	ram, source := connectx.EffectiveRAMLimit()
	if strings.HasPrefix(source, "fallback default") {
		return 0, source
	}
	return ram, source
}

// resourceConfigLaunchCount is the pool size this start is about to launch. It
// is stored before the warning runs so the check reports what is actually being
// started, and it stays an atomic so a later move of the call into a launch
// goroutine (as the provider does) cannot race it.
var resourceConfigLaunchCount atomic.Int64

// resourceConfigWarnOnce makes the startup warning fire exactly once, after the
// tier/profile memory limits are in place. Read earlier (before
// applyProfileProcessTuning) it would report the memory plan's default instead
// of the limit the tier or operator actually installed.
var resourceConfigWarnOnce sync.Once

// resourceConfigGather reads the live values the startup warning reports. It is
// a variable so a test can pin them: the real one reads the process's own
// limits, which no test can set without changing the whole test binary's
// runtime.
var resourceConfigGather = resourceConfigInputCurrent

// resourceConfigInputCurrent is the real gatherer: the pool size this start is
// about to launch, the soft memory limit in force right now, and the box's own
// RAM ceiling.
func resourceConfigInputCurrent() resourceConfigInput {
	var limit int64
	if l := debug.SetMemoryLimit(-1); l > 0 && l != math.MaxInt64 {
		limit = l
	}
	ram, _ := warningRAMCeiling()
	return resourceConfigInputFrom(int(resourceConfigLaunchCount.Load()), limit, ram)
}

// resourceConfigWarningsOnce says once, at startup, when the limits this
// process runs under are short for the pool it is about to launch. The soft
// memory limit is read here, after applyProfileProcessTuning has run, so what
// it reports is the limit actually in force.
func resourceConfigWarningsOnce() {
	resourceConfigWarnOnce.Do(func() {
		for _, w := range resourceConfigWarnings(resourceConfigGather()) {
			importantLogf("[proxy][resources] warning: %s\n", w)
		}
	})
}

// applyStartupMemoryTuning runs the startup steps that have to stay in this
// order: replay the persisted operator pin, install the profile/operator Go
// runtime knobs, then report once what RAM ceiling this process tunes against,
// which soft limit is actually in force, and whether that limit is short for
// the pool it is about to launch.
//
// The order is load-bearing. resourceConfigWarningsOnce reads
// debug.SetMemoryLimit(-1); on eco/turbo nodes the tier code in
// applyProfileProcessTuning is what installs the final soft limit, and on
// default/auto nodes ensureMemoryLimit may install one. Run before that, the
// warning would report the memory plan's default instead of the limit the
// process actually runs under. Keeping both steps in one function makes that
// order a runtime property a test drives directly instead of source line
// order.
//
// The replay comes first for the same reason the tiers have to see it: a
// persisted `urnet-tools set gomemlimit` is already treated as "the operator
// pinned this" by persistedRuntimeTuningActive, so it must be in force before
// anything that would otherwise install a default, or the pin is skipped and
// lost at the same time (gomemlimit_startup.go). The environment still
// outranks it, and the memory plan's default has already been installed by the
// caller before the control state is loaded.
func applyStartupMemoryTuning(maxMemory connect.ByteCount, proxies int) {
	replayed := replayPersistedGoMemLimit()
	tier := applyProfileProcessTuning(maxMemory)
	ceilingBytes, ceilingSource := warningRAMCeiling()
	importantLogf("%s\n", ramCeilingLogLine(ceilingBytes, ceilingSource))
	// The line that answers "which soft limit am I actually running": it is
	// read after every site that can install one, so it names the limit the
	// process runs under rather than an intention.
	importantLogf("%s\n", goMemLimitLogLine(debug.SetMemoryLimit(-1), goMemLimitSource(replayed, tier)))
	resourceConfigLaunchCount.Store(int64(proxies))
	resourceConfigWarningsOnce()
}

// ramCeilingLogLine is the one line, logged once at startup, that says what RAM
// ceiling this process tunes itself against and which limit set it. The number
// is derived from the process's own cgroup and can differ from the host's RAM
// by a lot, so without this line a box that suddenly tunes for a smaller
// ceiling gives the operator nothing to explain why. An unreadable ceiling says
// so instead of naming a figure the box does not have.
func ramCeilingLogLine(ceilingBytes int64, source string) string {
	if ceilingBytes <= 0 {
		return fmt.Sprintf("[proxy][resources] effective RAM ceiling unknown (%s)", source)
	}
	return fmt.Sprintf("[proxy][resources] effective RAM ceiling %d MiB (%s)", ceilingBytes>>20, source)
}
