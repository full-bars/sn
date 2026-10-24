package miner

import (
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strings"

	"github.com/urnetwork/connect"
)

// gomemlimit_startup.go replays the persisted `urnet-tools set gomemlimit`
// value at startup.
//
// The control socket applies gomemlimit live (control_socket.go), and every
// profile tier skips its own default while persistedRuntimeTuningActive says
// the key is set - on the assumption that the operator's pin is in force. The
// miner had no startup replay for it, so after a restart the runtime ran with
// the memory plan's default while the tiers kept skipping: the plan silently
// beat an explicit pin, and nothing said so. The provider replays the key from
// applyPersistedRuntimeTuning (provider/control_socket.go); this is that
// replay, in the miner's startup order.
//
// Precedence, highest first (the same order profile_tiers.go documents):
//
//  1. GOMEMLIMIT in the environment - the runtime installs it before main, and
//     this replay leaves it alone.
//  2. a persisted control-socket gomemlimit - replayed here, after the memory
//     plan (provider_memory.go) installed its default. This includes a value
//     urnet-tools queued while the miner was stopped: the control-state merge
//     folds it into the same state before this runs.
//  3. --max-memory, the plan's explicit form (every tier skips for it).
//  4. the profile tier default (eco 75% of RAM, turbo 80%).
//  5. the memory plan's default, and ensureMemoryLimit's generic fallback.
//
// Position is load-bearing on both sides: the replay runs after the memory plan
// so the pin beats the plan, and before applyProfileProcessTuning so the tiers
// see persistedRuntimeTuningActive and skip, exactly as they already do. The
// value 0 means "no soft limit" (the live apply path maps it to
// math.MaxInt64), and "off" is the clear value, so neither is replayed as a
// byte count.
//
// gogc has the same shape (persisted, live-applied, no startup replay) and is
// replayed by replayPersistedGoGC at the same point (gogc_startup.go), with its
// own semantics documented there: "disabled" turns collection off, "off" is the
// clear value and is not replayed.

// replayPersistedGoMemLimit installs the persisted gomemlimit, if any, and
// reports whether it did. The environment outranks it, and an absent, cleared
// ("off"), or unparseable value leaves the memory plan's default in force.
func replayPersistedGoMemLimit() bool {
	if os.Getenv("GOMEMLIMIT") != "" {
		// The runtime already installed the operator's environment value before
		// main, and applyProviderProcessMemory skipped the plan for it. Replaying
		// the persisted value here would silently downgrade the pin that wins.
		return false
	}
	value, ok := globalControlState.get("gomemlimit")
	if !ok || value == "" || strings.EqualFold(value, "off") {
		return false
	}
	limit, err := connect.ParseByteCount(value)
	if err != nil {
		// Leave the plan's limit in force and say why: the alternative is a
		// runtime that quietly ignores a pin the operator believes is set.
		importantLogf("[proxy][resources] persisted gomemlimit=%q is not a byte count: %v; the memory plan's limit stands\n", value, err)
		return false
	}
	if limit <= 0 {
		// Zero is a zero-byte soft limit to the Go runtime, not "unlimited";
		// math.MaxInt64 is the value that means no soft limit.
		limit = math.MaxInt64
	}
	debug.SetMemoryLimit(limit)
	return true
}

// goMemLimitSourcePlan is the label for the limit no operator pin and no tier
// set: --max-memory when given, else the default plan (or, with neither, the
// generic fallback ensureMemoryLimit installs).
const goMemLimitSourcePlan = "memory plan"

// goMemLimitSource names where the soft limit in force came from, for the one
// startup line that answers "which limit am I actually running". replayed says
// the persisted value was installed; tier is the label
// applyProfileProcessTuning returns when a profile tier installed it.
func goMemLimitSource(replayed bool, tier string) string {
	switch {
	case os.Getenv("GOMEMLIMIT") != "":
		return "env GOMEMLIMIT"
	case replayed:
		return "persisted urnet-tools set gomemlimit"
	case tier != "":
		return tier
	default:
		return goMemLimitSourcePlan
	}
}

// goMemLimitLogLine is the soft-limit half of the one line, logged once at
// startup, that names the Go runtime tuning in force and its source; the whole
// line is goRuntimeTuningLogLine in gogc_startup.go, which appends the GC
// setting. Without it the only record of a pin that did not survive a restart
// is the absence of the line the socket logged when it was set, which is
// indistinguishable from a pin that was never set.
func goMemLimitLogLine(limit int64, source string) string {
	if limit <= 0 || limit == math.MaxInt64 {
		return fmt.Sprintf("[proxy][resources] Go soft memory limit unlimited (source: %s)", source)
	}
	return fmt.Sprintf("[proxy][resources] Go soft memory limit %d MiB in force (source: %s)", limit>>20, source)
}

// profileTierSource labels a tier that installed the soft limit.
func profileTierSource(profile string) string {
	return "profile tier " + profile
}
