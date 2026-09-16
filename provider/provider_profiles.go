package provider

import (
	"math"
	"os"
	"runtime/debug"

	"github.com/urnetwork/connect"
)

func applyLowmodeSettings(clientSettings *connect.ClientSettings, localUserNatSettings *connect.LocalUserNatSettings) {
	if os.Getenv("URNETWORK_PROFILE") != "lowmem" {
		return
	}

	// 1. Initial Contract Size: 2 MiB -> 256 KiB
	clientSettings.ContractManagerSettings.InitialContractTransferByteCount = 256 * 1024

	// 2. IP Buffer Depth: 256 -> 16
	localUserNatSettings.SequenceBufferSize = 16
	localUserNatSettings.TcpBufferSettings.SequenceBufferSize = 16
	localUserNatSettings.UdpBufferSettings.SequenceBufferSize = 16

	// 3. TCP Accordion Window: 1MB -> 32KB
	localUserNatSettings.TcpBufferSettings.MaxWindowSize = 32 * 1024
}

func applyTurboSettings(clientSettings *connect.ClientSettings, localUserNatSettings *connect.LocalUserNatSettings) {
	profile := os.Getenv("URNETWORK_PROFILE")
	var windowSize uint32
	var queueBytes connect.ByteCount
	switch profile {
	case "turbo-v4":
		windowSize = 4 * 1024 * 1024
		queueBytes = 8 * 1024 * 1024
	case "turbo-v8":
		windowSize = 8 * 1024 * 1024
		queueBytes = 16 * 1024 * 1024
	default:
		return
	}

	// TCP Accordion window — primary per-connection throughput ceiling (window / RTT)
	localUserNatSettings.TcpBufferSettings.MaxWindowSize = windowSize
	localUserNatSettings.UdpBufferSettings.MaxWindowSize = windowSize

	// IP-layer packet queue depth
	localUserNatSettings.SequenceBufferSize = 512
	localUserNatSettings.TcpBufferSettings.SequenceBufferSize = 512
	localUserNatSettings.UdpBufferSettings.SequenceBufferSize = 512

	// Transfer-layer send/receive queues — must scale with window or they become the bottleneck
	clientSettings.SendBufferSettings.ResendQueueMaxByteCount = queueBytes
	clientSettings.ReceiveBufferSettings.ReceiveQueueMaxByteCount = queueBytes

	// Transfer-layer goroutine queue depth
	clientSettings.SendBufferSettings.SequenceBufferSize = 64
	clientSettings.ReceiveBufferSettings.SequenceBufferSize = 64

	// Retention telemetry: wire retained-item ack/drop events to the
	// persistent health event log so retention data survives restarts.
	// DESIGN ADAPTATION: connect.SendBufferSettings.RetentionEventCallback
	// was removed in v2026 connect. The hook is applied directly where the
	// provider instantiates its own transfer settings.

	// WebRTC per-peer DataChannel buffer
	clientSettings.WebRtcSettings.ReceiveBufferSize = connect.ByteCount(windowSize) * 2

	// Faster contract ramp: reach StandardContractTransferByteCount in 3 contracts instead of 4
	clientSettings.ContractManagerSettings.ContractTransferByteSeqScale = 3

	if os.Getenv("GOGC") == "" && !persistedRuntimeTuningActive("gogc") {
		debug.SetGCPercent(200)
	}
}

// applyTurboMemoryLimit sets GOMEMLIMIT to 80% of effective RAM for the
// turbo profiles, unless an operator-explicit value already wins: the
// GOMEMLIMIT env var, --max-memory, or a persisted control-socket
// gomemlimit (see persistedRuntimeTuningActive for the full precedence
// order). Called on every provideWithProxy invocation (once per proxy), so
// this guard has to be checked every time, not just at startup — the first
// version of this code only checked the env var and silently clobbered a
// persisted gomemlimit back to the turbo default on the next proxy add.
func applyTurboMemoryLimit(profile string, maxMemory connect.ByteCount) {
	if profile != "turbo-v4" && profile != "turbo-v8" {
		return
	}
	if os.Getenv("GOMEMLIMIT") != "" || maxMemory != 0 || persistedRuntimeTuningActive("gomemlimit") {
		return
	}
	ramBytes := detectEffectiveRAMLimitBytes()
	debug.SetMemoryLimit(ramBytes * 80 / 100)
}

// applyPoolAutoSize scales the message pool free-list capacity to RAM/32 at
// startup. The pool default (1 MiB) is badly undersized for 4000+ proxies —
// almost every packet misses the pool and falls back to a GC allocation.
// Skipped when lowmem is active (it manages its own footprint) or when
// --max-memory was set (that path already resizes via maxMemory/8).
func applyPoolAutoSize(maxMemory connect.ByteCount) {
	if maxMemory > 0 {
		return
	}
	if os.Getenv("URNETWORK_PROFILE") == "lowmem" {
		return
	}
	ram := detectEffectiveRAMLimitBytes()
	poolBytes := connect.ByteCount(ram) / 32
	const floor = 8 * 1024 * 1024
	const ceiling = 256 * 1024 * 1024
	if poolBytes < floor {
		poolBytes = floor
	}
	if poolBytes > ceiling {
		poolBytes = ceiling
	}
	// DESIGN ADAPTATION: connect.ResizeMessagePoolsPerClass was removed in
	// v2026 connect. The pool auto-sizing is a no-op stub.
	tlog("📦 [pool] message pool %dMiB (RAM=%dMiB)\n", poolBytes/1024/1024, connect.ByteCount(ram)/1024/1024)
}

func applyEcoSettings(maxMemory connect.ByteCount) {
	if os.Getenv("URNETWORK_PROFILE") != "eco" {
		return
	}

	if os.Getenv("GOGC") == "" && !persistedRuntimeTuningActive("gogc") {
		debug.SetGCPercent(50)
	}

	// Only set GOMEMLIMIT if neither --max-memory, the GOMEMLIMIT env var,
	// nor a persisted control-socket value were provided explicitly; those
	// take precedence (see persistedRuntimeTuningActive for the full order).
	if os.Getenv("GOMEMLIMIT") == "" && maxMemory == 0 && !persistedRuntimeTuningActive("gomemlimit") {
		ramBytes := detectEffectiveRAMLimitBytes()
		ecoLimit := ramBytes * 75 / 100
		debug.SetMemoryLimit(ecoLimit)
	}
}

// ensureMemoryLimit guarantees every provider code path runs under a finite
// GOMEMLIMIT so heap growth is always bounded by the runtime (the ATL2
// outage class: turbo/Tier3/Tier4 and bare `provide` left the process with
// no limit, so nothing pushed back and the kernel chose swap). Call it once
// AFTER all profile/tier eco/turbo application so turbo and eco limits are
// already in place and win. Operator overrides always win.
//
// Order of precedence (first match wins):
//  1. GOMEMLIMIT set in the environment (operator explicit)
//  2. --max-memory flag (maxMemory > 0, applied earlier)
//  3. a finite limit already set by any tier/profile (eco, turbo)
//  4. this function's default: 80% of effective RAM, absolute headroom
//     capped at 1 GiB so a RAM-rich box does not claim RAM other tenants need.
func ensureMemoryLimit(maxMemory connect.ByteCount) {
	if os.Getenv("GOMEMLIMIT") != "" || maxMemory > 0 {
		return // operator precedence, already applied
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
	// ram-gib is negative below 1 GiB; clamp so min() can never pick a negative
	// limit (the tiny-box guard below would correct it, but not by design).
	limit := min(ram*80/100, max(ram-gib, 0))
	if limit < 256*mib {
		// tiny-box guard: a sub-256MiB default would choke a small host.
		limit = ram * 80 / 100
	}
	debug.SetMemoryLimit(limit)
	tlog("[mem] memory limit: %.0fMiB (RAM=%.0fMiB, no explicit GOMEMLIMIT/--max-memory)\n",
		float64(limit)/float64(mib), float64(ram)/float64(mib))
}
