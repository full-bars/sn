package miner

import (
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

// Provider process memory.
//
// The default provider has no memory ceiling. Three surfaces are sized here:
//
//   - the per-provider DeviceLocal target (DeviceLocalSettings.
//     MemoryTargetByteCount). Its carrier, NAT and transfer budgets derive
//     from it, and they keep scaling with it: the H3 stream window is 3T/32
//     of the target (6 MiB at 64 MiB, 24 MiB at 256 MiB, 96 MiB at 1 GiB),
//     the connection window T/8, and the provider client's transfer pair is a
//     fraction of the provider share. There is no reference above which a
//     larger target buys nothing.
//   - the connect process budget (connect.SetMemoryBudget). It sizes the
//     process defaults, including H3's reservation and receive windows. The
//     pinned transfer defaults retain their bounded per-sequence constants;
//     SDK device targets supply the provider's explicit transfer pools. A
//     positive process budget does not select provider flow caps: those are
//     controlled by the separate provider memory target.
//   - the Go runtime soft limit (debug.SetMemoryLimit). The runtime holds
//     roughly three bytes per live byte, so a soft limit equal to the summed
//     targets makes GC run continuously once live memory reaches a third of
//     it. The default soft limit is therefore three times the summed targets,
//     and the targets are derived so that this is four fifths of the usable
//     host memory (providerHostMemoryFraction), leaving the rest to the
//     operating system and co-resident processes. Three times the targets can
//     be larger than that heap when the 20 MiB floor lifts a small share, so
//     the soft limit is clamped to the heap: a limit above physical memory is
//     no limit at all, and the runtime would never collect under pressure.
//
// The budget is the soft limit. That keeps the two constraints the default
// plan encodes: the budget is at least three times one target (the runtime
// amplification above), and one target is at most 20/34 of the budget (the
// SDK's split of a process budget into 12 packet pool : 2 large object pool :
// 20 device target parts, see sdk.SetMemoryLimit).
//
// Both are per-device constraints, and they survive the clamp: the clamped
// budget is the heap, which still holds three 20 MiB targets on any host at or
// above 75 MiB and 20/34 of one target on any host at or above 42.5 MiB.
//
// What more providers than the host heap can hold at 20 MiB each breaks is the
// aggregate. The targets no longer fit the budget at all (an 8 GiB host holds
// 327 of them at the floor, not 2000), so 3 x the summed targets is no longer
// four fifths of the host and the soft limit is clamped to the heap instead.
// The target stays at the floor so the device budgets keep the SDK minimum.
// That state is not silent -- the plan logs one warning naming the provider
// count, the per-provider share the heap affords, the floor, and the host heap,
// so an operator can see the hosts are being asked to do more than they can.
//
// A host figure is not available everywhere: provider_host_memory_other.go
// reports zero on FreeBSD, Windows and every platform without /proc/meminfo or
// sysctl. The previous default composed 3 x count x 64 MiB there with nothing
// to check it against -- 2000 providers asked for 375 GiB whatever the host,
// which is a limit above physical memory and so no limit at all, silently.
// With no measurement to clamp against, that composition is bounded by
// providerUnknownHostAssumedHostByteCount, the documented smallest host the
// default profile targets, and the plan says once at startup that the bound is
// an assumption rather than a reading. A host reading that leaves no usable
// heap after the 4/5 fraction (integer truncation to zero) installs no limit
// and warns, rather than installing a fictional one.
//
// --max-memory keeps its existing meaning: the process soft limit, divided
// evenly into the per-provider targets, whatever the host; the deployment
// that passes it owns that split. It is now also the process budget.

// providerUnknownHostDeviceMemoryTargetByteCount is the per-provider target
// when the usable host memory cannot be read (no /proc/meminfo, no sysctl):
// there is nothing to derive from, so the plan keeps the previous default.
const providerUnknownHostDeviceMemoryTargetByteCount = connect.ByteCount(64 * 1024 * 1024)

// providerUnknownHostAssumedHostByteCount is the host memory the plan assumes
// when the platform cannot report it: the bottom of the 2-4 GiB range
// docs/Configuration.md gives the default provider profile, i.e. the smallest
// host a default (non-lowmem) provider is expected to run on. It bounds the
// composed soft limit only -- the per-device target stays at the 64 MiB above,
// because deriving that from the assumption would drop a many-provider farm
// under the SDK's carrier minimum -- so an unreadable host never asks the
// runtime for a limit far above the memory it actually has. It is an
// assumption, not a reading, and the plan says so once at startup.
const providerUnknownHostAssumedHostByteCount = connect.ByteCount(2 * 1024 * 1024 * 1024)

// providerMinDeviceMemoryTargetByteCount is the SDK's desktop/server default,
// which every provider got before. A small host never goes below it.
const providerMinDeviceMemoryTargetByteCount = connect.ByteCount(20 * 1024 * 1024)

// providerRuntimeBytesPerLiveByte is the runtime overhead factor applied to
// the summed targets for the default soft limit.
const providerRuntimeBytesPerLiveByte = 3

// providerHostMemoryFraction (numerator / denominator, 4/5) is the fraction of
// the usable host memory the provider may let its heap climb to: the default
// soft limit is this much of the host. The remaining fifth is headroom for the
// operating system and co-resident processes. It is a separate quantity from
// the runtime factor above (which relates the soft limit to the live targets),
// and must not be folded into it.
const providerHostMemoryFractionNumerator = 4
const providerHostMemoryFractionDenominator = 5

type providerMemoryPlan struct {
	// per provider device; 0 keeps the SDK default
	DeviceMemoryTargetByteCount connect.ByteCount
	// Go soft limit; 0 leaves it unset
	SoftLimitByteCount connect.ByteCount
	// connect process budget; 0 leaves it unset
	MemoryBudgetByteCount connect.ByteCount
	// one-argument ResizeMessagePools cap; 0 leaves the pools unchanged
	MessagePoolByteCount connect.ByteCount
}

// newProviderAuthMemoryPlan: the auth command is unchanged. Only an explicit
// --max-memory sizes it, with the historical /8 message pool cap.
func newProviderAuthMemoryPlan(explicitMaxMemory connect.ByteCount) providerMemoryPlan {
	if explicitMaxMemory <= 0 {
		return providerMemoryPlan{}
	}
	return providerMemoryPlan{
		SoftLimitByteCount:   explicitMaxMemory,
		MessagePoolByteCount: explicitMaxMemory / 8,
	}
}

// newProviderMemoryPlan sizes the provide command. The provide path never
// resized the message pools and still does not.
//
// Explicit --max-memory: soft limit = budget = max-memory, target =
// max-memory / count.
//
// Absent: no ceiling. target = (4/5 x host) / (3 x count) when the usable host
// memory is known, so the default soft limit (3 x count x target) is four
// fifths of the host and never exceeds it. The 20 MiB floor still applies to
// the target, and when it lifts the target past the share the configuration is
// over-subscribed: the soft limit is clamped to the host heap (never above it),
// the target stays at the floor, and a warning names the mismatch. A host that
// cannot be read keeps the previous 64 MiB target and bounds the soft limit
// with providerUnknownHostAssumedHostByteCount, announced once, because there
// is no measurement to clamp against. budget = soft limit.
func newProviderMemoryPlan(
	explicitMaxMemory connect.ByteCount,
	hostByteCount connect.ByteCount,
	providerCount int,
) providerMemoryPlan {
	providerCount = max(1, providerCount)
	count := connect.ByteCount(providerCount)
	if 0 < explicitMaxMemory {
		return providerMemoryPlan{
			DeviceMemoryTargetByteCount: explicitMaxMemory / count,
			SoftLimitByteCount:          explicitMaxMemory,
			MemoryBudgetByteCount:       explicitMaxMemory,
		}
	}
	target := providerUnknownHostDeviceMemoryTargetByteCount
	softLimit := providerRuntimeBytesPerLiveByte * count * target
	if 0 < hostByteCount {
		heapByteCount := hostByteCount *
			providerHostMemoryFractionNumerator /
			providerHostMemoryFractionDenominator
		shareByteCount := heapByteCount / (providerRuntimeBytesPerLiveByte * count)
		target = max(shareByteCount, providerMinDeviceMemoryTargetByteCount)
		softLimit = providerRuntimeBytesPerLiveByte * count * target
		// The heap is the ceiling on the soft limit, not a target to aim the
		// derivation at: when the floor lifted the target past the share,
		// 3 x the targets no longer fits and the limit is clamped instead of
		// handed to the runtime above physical memory. A reading that leaves
		// no heap at all (a host of a few bytes, truncated by the 4/5
		// fraction) clamps to zero, which leaves the runtime limit unset; the
		// warning below still fires, so neither case is silent.
		if heapByteCount < softLimit {
			warnProviderMemoryPlanOverSubscribed(
				providerCount,
				shareByteCount,
				providerMinDeviceMemoryTargetByteCount,
				heapByteCount,
				hostByteCount,
				softLimit,
			)
			softLimit = heapByteCount
		}
	} else {
		// No host figure: bound the historical 3 x count x 64 MiB composition
		// by the documented assumption instead of asking for a limit that
		// cannot be checked against any host, and say that it is assumed.
		assumedHeapByteCount := providerUnknownHostAssumedHostByteCount *
			providerHostMemoryFractionNumerator /
			providerHostMemoryFractionDenominator
		if assumedHeapByteCount < softLimit {
			warnProviderMemoryPlanUnknownHost(
				providerCount,
				target,
				softLimit,
				assumedHeapByteCount,
			)
			softLimit = assumedHeapByteCount
		}
	}
	return providerMemoryPlan{
		DeviceMemoryTargetByteCount: target,
		SoftLimitByteCount:          softLimit,
		MemoryBudgetByteCount:       softLimit,
	}
}

// warnProviderMemoryPlanOverSubscribed logs the over-subscribed case once at
// startup: the 20 MiB floor put the device targets above the host heap, so the
// soft limit was clamped below 3 x the targets. Without the line the operator
// sees a budget smaller than the runtime amplification implies and no reason
// for it.
func warnProviderMemoryPlanOverSubscribed(
	providerCount int,
	shareByteCount connect.ByteCount,
	floorByteCount connect.ByteCount,
	heapByteCount connect.ByteCount,
	hostByteCount connect.ByteCount,
	unclampedSoftLimitByteCount connect.ByteCount,
) {
	importantLogf(
		"[mem] over-subscribed: %d providers share %.1fMiB each of the %.0fMiB host heap (4/5 of %.0fMiB), under the %.0fMiB floor; each keeps the floor, so 3 x targets is %.0fMiB and the soft limit is clamped to the %.0fMiB heap\n",
		providerCount,
		mibf(shareByteCount),
		mibf(heapByteCount),
		mibf(hostByteCount),
		mibf(floorByteCount),
		mibf(unclampedSoftLimitByteCount),
		mibf(heapByteCount),
	)
}

// warnProviderMemoryPlanUnknownHost logs the unknown-host bound once at
// startup: the platform cannot report host memory, so the composed soft limit
// is bounded by providerUnknownHostAssumedHostByteCount instead of a reading.
// Without the line an operator on those platforms cannot tell an assumed limit
// from a measured one, and the limit itself would silently be 3 x count x the
// 64 MiB target.
func warnProviderMemoryPlanUnknownHost(
	providerCount int,
	targetByteCount connect.ByteCount,
	unclampedSoftLimitByteCount connect.ByteCount,
	assumedHeapByteCount connect.ByteCount,
) {
	importantLogf(
		"[mem] host memory unknown on this platform: %d providers at the %.0fMiB target compose a %.0fMiB soft limit; bounded to the %.0fMiB heap of the documented %.0fMiB unknown-host assumption, which is not a reading of this host. Pass --max-memory to size the limit for this host\n",
		providerCount,
		mibf(targetByteCount),
		mibf(unclampedSoftLimitByteCount),
		mibf(assumedHeapByteCount),
		mibf(providerUnknownHostAssumedHostByteCount),
	)
}

// mibf renders a byte count as MiB for the memory plan's log lines.
func mibf(byteCount connect.ByteCount) float64 {
	return float64(byteCount) / (1024 * 1024)
}

// applyProviderProcessMemory installs the process-wide half of a plan.
func applyProviderProcessMemory(plan providerMemoryPlan) {
	if 0 < plan.MessagePoolByteCount {
		connect.ResizeMessagePools(plan.MessagePoolByteCount)
	}
	if 0 < plan.MemoryBudgetByteCount {
		// before any device exists: the budget is sampled when connect's
		// default settings are constructed
		connect.SetMemoryBudget(plan.MemoryBudgetByteCount)
	}
	if 0 < plan.SoftLimitByteCount {
		debug.SetMemoryLimit(plan.SoftLimitByteCount)
	}
}

// parseProviderMaxMemory returns the explicit --max-memory, or 0 when absent.
func parseProviderMaxMemory(opts docopt.Opts) connect.ByteCount {
	humanReadable, err := opts.String("--max-memory")
	if err != nil {
		return 0
	}
	maxMemory, err := connect.ParseByteCount(humanReadable)
	if err != nil {
		panic(fmt.Errorf("Bad mem argument: %s", humanReadable))
	}
	return maxMemory
}

// effectiveHostMemory is the smaller positive of physical memory and a
// container limit. Either may be unknown (<= 0).
func effectiveHostMemory(physical connect.ByteCount, limit connect.ByteCount) connect.ByteCount {
	if physical <= 0 {
		return max(0, limit)
	}
	if 0 < limit && limit < physical {
		return limit
	}
	return physical
}

// parseMeminfoTotal reads MemTotal (KiB) from /proc/meminfo content.
func parseMeminfoTotal(meminfo string) connect.ByteCount {
	for _, line := range strings.Split(meminfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kib <= 0 {
			return 0
		}
		return connect.ByteCount(kib) * 1024
	}
	return 0
}

// parseCgroupLimit reads a cgroup v2 memory.max or v1 memory.limit_in_bytes.
// "max" and the v1 unlimited sentinel (a page-rounded int64 max) are no limit.
func parseCgroupLimit(content string) connect.ByteCount {
	value := strings.TrimSpace(content)
	if value == "" || value == "max" {
		return 0
	}
	byteCount, err := strconv.ParseInt(value, 10, 64)
	if err != nil || byteCount <= 0 || (1<<62) <= byteCount {
		return 0
	}
	return connect.ByteCount(byteCount)
}
