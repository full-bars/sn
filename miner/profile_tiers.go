package miner

import (
	"math"
	"os"
	"runtime/debug"
	"strings"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// This file ports the 3.23-fix v33 provider's profile tiers
// (provider/main.go: applyLowmodeSettings, applyTurboSettings,
// applyTurboMemoryLimit, applyEcoSettings, applyPoolAutoSize,
// ensureMemoryLimit, persistedRuntimeTuningActive) onto the miner.
//
// URNETWORK_PROFILE selects one tier for the whole process:
//
//	lowmem    smallest footprint: 256 KiB initial contracts, shallow packet
//	          queues, a 32 KiB TCP window, and ramlogs by default
//	eco       GOGC 50 and an explicit GOMEMLIMIT at 75% of effective RAM
//	turbo-v4  a 4 MiB per-connection accordion window, 8 MiB transfer queues
//	turbo-v8  same at 8 MiB / 16 MiB
//	auto      leave the connect/client defaults alone; the startup audit may
//	          switch logs to ramlogs on a slow disk (see the audit port)
//
// Environment-only, exactly like v33: the tier is read from URNETWORK_PROFILE
// at the points that consume it. The control-socket `profile` key seeds that
// env var before initGlog via seedEnvFromControlState (startup_env_seed.go),
// and the socket canonicalizes v4/v8 to turbo-v4/turbo-v8 on set
// (canonicalProfileName below), so both entry points agree.
//
// Operator precedence for the Go runtime knobs is: GOMEMLIMIT/GOGC env >
// a persisted control-socket value (urnet-tools set gomemlimit|gogc) > the
// profile's default > ensureMemoryLimit's generic RAM-percentage fallback.
// persistedRuntimeTuningActive is the middle gate, checked at every site. The
// persisted value has to be installed before the tiers run - the startup
// replays do that (replayPersistedGoMemLimit, replayPersistedGoGC;
// gomemlimit_startup.go, gogc_startup.go) - otherwise the gate says "the
// operator pinned this" while a default is what the runtime runs under.
//
// GOGC is process-wide, so the tier's GOGC default is installed once by
// applyProfileGCPercent in the startup step; the per-provider settings
// constructors only carry the per-provider sizing.

const (
	profileAuto    = "auto"
	profileEco     = "eco"
	profileLowmem  = "lowmem"
	profileTurboV4 = "turbo-v4"
	profileTurboV8 = "turbo-v8"
)

// canonicalProfileName maps the client's short aliases onto the canonical
// tier names. The control socket has accepted v4/v8 since P2; the same
// canonicalization is applied to the env var here so a raw
// `URNETWORK_PROFILE=v4` behaves like the socket-set value instead of
// silently doing nothing.
func canonicalProfileName(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "v4":
		return profileTurboV4
	case "v8":
		return profileTurboV8
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

// effectiveProfile is the profile in force for this process: URNETWORK_PROFILE
// (which seedEnvFromControlState has already overlaid with the persisted or
// queued socket value), canonicalized.
func effectiveProfile() string {
	return canonicalProfileName(os.Getenv("URNETWORK_PROFILE"))
}

// profileWantsRamlogs reports whether the profile alone asks for the ramlog
// redirect: lowmem and eco both do in v33 (initGlog), whatever
// URNETWORK_RAMLOGS says.
func profileWantsRamlogs(profile string) bool {
	return profile == profileLowmem || profile == profileEco
}

// profileBenchmarksDisk reports whether the startup audit may run the
// destructive disk-write benchmark: only the auto profile acts on its result
// (a slow disk switches logs to RAM), so only auto asks for it.
func profileBenchmarksDisk(profile string) bool {
	return profile == profileAuto
}

// persistedRuntimeTuningActive reports whether urnet-tools set this runtime
// tuning key (gomemlimit/gogc) over the control socket. Such a value is
// operator-explicit and must beat any profile default; the v33 comment on the
// port explains why every defaulting site has to check it.
//
// "off" is the clear value and is not a pin, and the comparison is
// case-insensitive because the socket's validation accepts "OFF" and stores it
// verbatim. An exact match here made a persisted "OFF" suppress the tier
// defaults while both startup replays skipped it - the gate saying "pinned"
// while nothing was pinned, which is the bug the replays exist to prevent.
func persistedRuntimeTuningActive(key string) bool {
	v, ok := globalControlState.get(key)
	return ok && v != "" && !strings.EqualFold(v, "off")
}

// detectEffectiveRAMLimitBytes returns the effective RAM ceiling in bytes:
// the smaller of physical memory and any cgroup limit (provider_memory.go's
// hostMemoryByteCount, which is the miner's existing reader).
func detectEffectiveRAMLimitBytes() int64 {
	return int64(hostMemoryByteCount())
}

// applyLowmodeSettings applies the lowmem tier to one provider's device
// settings. v33 mutates connect.ClientSettings and connect.LocalUserNatSettings
// directly; the miner's device settings are built by the SDK
// (sdk.DeviceLocalSettings), which derives LocalUserNatSettings from the
// memory target itself, so the reachable subset is applied here:
//
//   - initial contract size 2 MiB -> 256 KiB (ClientSettings.Contract-)
//   - packet queue depth 256 -> 16 (DeviceLocalSettings.SequenceBufferSize)
//
// The v33 TCP accordion window (1 MiB -> 32 KiB) lives in
// connect.TcpBufferSettings, which the SDK owns internally and does not
// expose; the memory-target sizing the SDK applies for the provider role is
// the equivalent bound in the miner. TODO(P2 remainder): expose the nat
// window settings through the SDK if operator parity there is required.
func applyLowmodeSettings(settings *sdk.DeviceLocalSettings) {
	if settings == nil || os.Getenv("URNETWORK_PROFILE") != profileLowmem {
		return
	}

	// 1. Initial Contract Size: 2 MiB -> 256 KiB
	settings.ClientSettings.ContractManagerSettings.InitialContractTransferByteCount = 256 * 1024

	// 2. IP Buffer Depth: 256 -> 16
	settings.SequenceBufferSize = 16
}

// applyTurboSettings applies the turbo-v4/turbo-v8 transfer sizing to one
// provider's device settings, plus the tier's GOGC default. See
// applyLowmodeSettings for what the SDK-owned LocalUserNatSettings can and
// cannot carry: the window sizes and packet queue depth below are the
// reachable half (client transfer queues and the device sequence depth), and
// the WebRTC receive buffer.
func applyTurboSettings(settings *sdk.DeviceLocalSettings) {
	profile := os.Getenv("URNETWORK_PROFILE")
	var windowSize uint32
	var queueBytes connect.ByteCount
	switch profile {
	case profileTurboV4:
		windowSize = 4 * 1024 * 1024
		queueBytes = 8 * 1024 * 1024
	case profileTurboV8:
		windowSize = 8 * 1024 * 1024
		queueBytes = 16 * 1024 * 1024
	default:
		return
	}

	// IP-layer packet queue depth
	settings.SequenceBufferSize = 512

	// Transfer-layer send/receive queues - must scale with the window or they
	// become the bottleneck.
	settings.ClientSettings.SendBufferSettings.ResendQueueMaxByteCount = queueBytes
	settings.ClientSettings.ReceiveBufferSettings.ReceiveQueueMaxByteCount = queueBytes

	// Transfer-layer goroutine queue depth
	settings.ClientSettings.SendBufferSettings.SequenceBufferSize = 64
	settings.ClientSettings.ReceiveBufferSettings.SequenceBufferSize = 64

	// WebRTC per-peer DataChannel buffer
	settings.ClientSettings.WebRtcSettings.ReceiveBufferSize = connect.ByteCount(windowSize) * 2

	// Faster contract ramp: reach StandardContractTransferByteCount in 3
	// contracts instead of 4.
	settings.ClientSettings.ContractManagerSettings.ContractTransferByteSeqScale = 3

	// v33 also wires SendBufferSettings.RetentionEventCallback to the
	// persistent health event log here; the pinned connect no longer carries
	// that field (the fork applies the hook where it instantiates its own
	// transfer settings), so nothing is assigned for it.

	// The tier's GOGC default (200 here) is deliberately NOT set in this
	// per-provider constructor: GOGC is process-wide, so applyProfileGCPercent
	// installs it once at startup (applyProfileProcessTuning). Setting it here
	// re-applied the tier value for every new provider after a live change,
	// and it ran after the one startup line that reports the value in force,
	// so that line could not name it.
}

// applyProfileDeviceSettings applies whichever tier is in force to one
// provider's device settings. Every construction site of the miner's
// provider device settings calls it, which is the per-proxy application
// point in v33 (applyLowmodeSettings + applyTurboSettings next to each
// connect settings construction).
func applyProfileDeviceSettings(settings *sdk.DeviceLocalSettings) {
	applyLowmodeSettings(settings)
	applyTurboSettings(settings)
}

// applyTurboMemoryLimit sets GOMEMLIMIT to 80% of effective RAM for the turbo
// profiles, unless an operator-explicit value already wins: the GOMEMLIMIT
// env var, --max-memory, or a persisted control-socket gomemlimit (see
// persistedRuntimeTuningActive for the full precedence order). Called once
// per provide start, after the control state is loaded and after the startup
// replay installed a persisted pin, so this guard sees both. Reports whether
// it installed the limit, so the startup line can name the source.
func applyTurboMemoryLimit(profile string, maxMemory connect.ByteCount) bool {
	if profile != profileTurboV4 && profile != profileTurboV8 {
		return false
	}
	if os.Getenv("GOMEMLIMIT") != "" || maxMemory != 0 || persistedRuntimeTuningActive("gomemlimit") {
		return false
	}
	ramBytes := detectEffectiveRAMLimitBytes()
	debug.SetMemoryLimit(ramBytes * 80 / 100)
	return true
}

// applyPoolAutoSize scales the message pool free-list capacity to RAM/32 at
// startup. The pool default (1 MiB) is badly undersized for 4000+ proxies -
// almost every packet misses the pool and falls back to a GC allocation.
// Skipped when lowmem is active (it manages its own footprint) or when
// --max-memory was set (that path already resizes via maxMemory/8).
//
// DESIGN ADAPTATION: connect.ResizeMessagePoolsPerClass was removed in the
// pinned v2026 connect (only ResizeMessagePools(packetByteCount, ...) remains,
// which sizes the packet/object pools, not the per-class free list). The v33
// sn fork carries the same adaptation; sizing the pools from a profile here
// would change process memory beyond the v33 behavior, so the pool resize is
// a logged no-op until a per-class API exists.
func applyPoolAutoSize(maxMemory connect.ByteCount) {
	if maxMemory > 0 {
		return
	}
	if os.Getenv("URNETWORK_PROFILE") == profileLowmem {
		return
	}
	ram := detectEffectiveRAMLimitBytes()
	if ram <= 0 {
		return
	}
	poolBytes := connect.ByteCount(ram) / 32
	const floor = 8 * 1024 * 1024
	const ceiling = 256 * 1024 * 1024
	if poolBytes < floor {
		poolBytes = floor
	}
	if poolBytes > ceiling {
		poolBytes = ceiling
	}
	tlog("[pool] message pool target %dMiB (RAM=%dMiB); per-class resize unavailable in the pinned connect\n",
		poolBytes/1024/1024, connect.ByteCount(ram)/1024/1024)
}

// applyEcoSettings applies the eco tier's Go soft memory limit: an explicit
// GOMEMLIMIT at 75% of effective RAM, only when no operator-explicit value
// (env, --max-memory, persisted control value) wins. The tier's GOGC default
// (50) is process-wide and installed by applyProfileGCPercent. Reports whether
// it installed the GOMEMLIMIT, so the startup line can name the source.
func applyEcoSettings(maxMemory connect.ByteCount) bool {
	if os.Getenv("URNETWORK_PROFILE") != profileEco {
		return false
	}

	if os.Getenv("GOMEMLIMIT") == "" && maxMemory == 0 && !persistedRuntimeTuningActive("gomemlimit") {
		ramBytes := detectEffectiveRAMLimitBytes()
		ecoLimit := ramBytes * 75 / 100
		debug.SetMemoryLimit(ecoLimit)
		return true
	}
	return false
}

// ensureMemoryLimit guarantees every provider code path runs under a finite
// GOMEMLIMIT so heap growth is always bounded by the runtime (the ATL2
// outage class: turbo/Tier3/Tier4 and bare `provide` left the process with no
// limit, so nothing pushed back and the kernel chose swap). Call it once
// AFTER all profile/tier eco/turbo application so turbo and eco limits are
// already in place and win. Operator overrides always win.
//
// Order of precedence (first match wins):
//  1. GOMEMLIMIT set in the environment (operator explicit)
//  2. a persisted control-socket gomemlimit (operator explicit, replayed at
//     startup by replayPersistedGoMemLimit - including the value 0, which
//     means no soft limit at all)
//  3. --max-memory flag (maxMemory > 0, applied earlier)
//  4. a finite limit already set by any tier/profile - in the miner this
//     includes the default provider memory plan (provider_memory.go), which
//     is why this is normally a no-op here
//  5. this function's default: 80% of effective RAM, absolute headroom
//     capped at 1 GiB so a RAM-rich box does not claim RAM other tenants need.
func ensureMemoryLimit(maxMemory connect.ByteCount) {
	if os.Getenv("GOMEMLIMIT") != "" || maxMemory > 0 {
		return // operator precedence, already applied
	}
	// A persisted pin is operator-explicit and outranks this fallback even when
	// the runtime limit does not look finite: 0 means "no soft limit", and
	// installing a default here would silently reverse that pin.
	if persistedRuntimeTuningActive("gomemlimit") {
		return
	}
	// A finite limit from a tier/profile means the box is already protected.
	if cur := debug.SetMemoryLimit(-1); cur > 0 && cur < math.MaxInt64 {
		return
	}
	ram := detectEffectiveRAMLimitBytes()
	if ram <= 0 {
		tlog("[mem] memory limit: cannot detect effective RAM; leaving unset\n")
		return
	}
	// tighter of 80% and (ram - 1 GiB) reserves headroom for other tenants on
	// large boxes while still bounding runaway growth.
	const gib = int64(1) << 30
	const mib = int64(1) << 20
	// ram-gib is negative below 1 GiB; clamp so min() can never pick a
	// negative limit (the tiny-box guard below would correct it, but not by
	// design).
	limit := min(ram*80/100, max(ram-gib, 0))
	if limit < 256*mib {
		// tiny-box guard: a sub-256MiB default would choke a small host.
		limit = ram * 80 / 100
	}
	debug.SetMemoryLimit(limit)
	tlog("[mem] memory limit: %.0fMiB (RAM=%.0fMiB, no explicit GOMEMLIMIT/--max-memory)\n",
		float64(limit)/float64(mib), float64(ram)/float64(mib))
}

// applyProfileGCPercent installs the tier's GOGC default - eco 50, turbo-v4/v8
// 200 - unless an operator-explicit value wins: GOGC in the environment, or a
// persisted control-socket gogc (already replayed by
// applyStartupMemoryTuning before this runs, so the pin is in force and the
// tier skips for exactly the value the operator set). Reports whether it
// installed a tier default, so the startup line can name the source.
//
// This is the one place a profile tier writes GOGC. The knob is process-wide,
// so per-provider settings constructors must not set it: doing so re-applied
// the tier value on every new provider and happened after the startup line
// that reports the value in force.
func applyProfileGCPercent(profile string) bool {
	if os.Getenv("GOGC") != "" || persistedRuntimeTuningActive("gogc") {
		return false
	}
	switch profile {
	case profileEco:
		debug.SetGCPercent(50)
	case profileTurboV4, profileTurboV8:
		debug.SetGCPercent(200)
	default:
		return false
	}
	return true
}

// applyProfileProcessTuning applies the process-wide half of the profile
// tiers in v33's order: the tier's GOGC default, turbo memory limit, eco
// settings, then the generic fallback. Call it once per provide start after the
// control state has been loaded and after both startup replays
// (replayPersistedGoMemLimit, replayPersistedGoGC), so
// persistedRuntimeTuningActive sees the operator's socket-set values and the
// tier guards and the fallback skip.
//
// It returns the label of the tier that installed each knob ("profile tier
// eco"), or "" when no tier did - the one startup line names both sources from
// that.
func applyProfileProcessTuning(maxMemory connect.ByteCount) (limitTier string, gcTier string) {
	profile := effectiveProfile()
	if applyProfileGCPercent(profile) {
		gcTier = profileTierSource(profile)
	}
	if applyTurboMemoryLimit(profile, maxMemory) {
		limitTier = profileTierSource(profile)
	}
	if applyEcoSettings(maxMemory) {
		limitTier = profileTierSource(profile)
	}
	ensureMemoryLimit(maxMemory)
	return limitTier, gcTier
}
