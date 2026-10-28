package miner

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// gogc_startup.go replays the persisted `urnet-tools set gogc` value at
// startup, for the same reason gomemlimit_startup.go replays gomemlimit, and
// with the same defect behind it: the control socket applies gogc live
// (control_socket.go), every profile tier skips its own GOGC default while
// persistedRuntimeTuningActive says the key is set, and the miner had no
// startup replay - so after a restart the runtime ran at its own default while
// the tiers kept skipping, and nothing said the operator's pin was gone.
//
// Value space. This is what the socket accepts and applies today, not a new
// parse: validateControlValue and applyControlLiveSideEffect both go through
// parseGoGCTarget below.
//
//   - an integer percentage >= 0, applied with debug.SetGCPercent(n). 0 is
//     legal: it collects on every allocation.
//   - "disabled" (any case) -> debug.SetGCPercent(-1): collection off, heap
//     grows unbounded.
//   - "off" (any case) -> debug.SetGCPercent(100), the runtime default. This is
//     the clear value shared with every other tuning key. urnet-tools turns it
//     into a `clear` (which deletes the key) before the socket sees it, so it
//     normally never persists; a `set gogc off` written straight to the socket
//     does persist "off", and that is not a pin. It is therefore NOT replayed:
//     the profile tier's default applies, exactly as it does for a key that was
//     never set.
//   - a negative number is rejected by validation (-1 is only reachable through
//     "disabled"), and debug.SetGCPercent would fold it to -1 anyway. A
//     hand-edited negative is unparseable here and left alone.
//
// Precedence, highest first (the same list profile_tiers.go documents):
//
//  1. GOGC in the environment - the runtime installed it before main, and this
//     replay leaves it alone. GOGC=off in the environment means collection is
//     off, which a persisted percentage must not reverse.
//  2. a persisted control-socket gogc - replayed here, before the tiers, so
//     they see persistedRuntimeTuningActive and skip. A value urnet-tools
//     queued while the miner was stopped is folded into the same state before
//     this runs.
//  3. the profile tier default: eco 50, turbo-v4/v8 200.
//  4. the runtime default, GOGC 100.

// goGCTarget is a gogc control value parsed into the runtime setting it means.
type goGCTarget struct {
	// Percent is what debug.SetGCPercent is called with: a percentage, or -1
	// for "disabled".
	Percent int
	// Clear is the "off" value: it names no pin and only restores the runtime
	// default (Percent is 100), so a startup replay must skip it rather than
	// pin a default the operator asked to clear.
	Clear bool
}

// parseGoGCTarget is the one parse of a gogc control value: validation, the
// socket's live apply, and the startup replay all call it, so a value cannot
// mean one thing when it is set and another after a restart.
func parseGoGCTarget(value string) (goGCTarget, error) {
	switch {
	case strings.EqualFold(value, "disabled"):
		return goGCTarget{Percent: -1}, nil
	case strings.EqualFold(value, "off"):
		return goGCTarget{Percent: 100, Clear: true}, nil
	}
	percent, err := strconv.Atoi(value)
	if err != nil {
		return goGCTarget{}, fmt.Errorf("must be an integer percentage, 'off' to clear, or 'disabled' to turn collection off (got %q)", value)
	}
	if percent < 0 {
		return goGCTarget{}, fmt.Errorf("must be a non-negative percentage, 'off' to clear, or 'disabled' to turn collection off (got %d)", percent)
	}
	return goGCTarget{Percent: percent}, nil
}

// replayPersistedGoGC installs the persisted gogc, if any, and reports whether
// it did. The environment outranks it; an absent or cleared ("off") value
// leaves the profile tier's default, or the runtime default, in force.
func replayPersistedGoGC() bool {
	if os.Getenv("GOGC") != "" {
		// The runtime installed the environment value before main and every
		// tier skipped for it. Replaying the persisted value here would
		// reverse the setting that wins - including GOGC=off.
		return false
	}
	value, ok := globalControlState.get("gogc")
	if !ok || value == "" {
		return false
	}
	target, err := parseGoGCTarget(value)
	if err != nil {
		// Leave the setting in force and say why. Silence would leave an
		// operator believing a pin this process never applied.
		importantLogf("[proxy][resources] persisted gogc=%q is not a GC target: %v; leaving GOGC as it is (clear the key to let the profile tier's default apply)\n", value, err)
		return false
	}
	if target.Clear {
		// "off" is the clear value, not a pin: the tiers decide.
		return false
	}
	liveTuningBaseline.rememberGCPercent(debug.SetGCPercent(target.Percent))
	return true
}

// goGCSourceDefault is the label for a GOGC no operator pin and no tier set:
// the Go runtime's own default of 100, which is what the process runs under
// when GOGC is unset in the environment.
const goGCSourceDefault = "runtime default"

// goGCSource names where the GOGC in force came from, for the one startup line
// that answers "which GC setting am I actually running". replayed says the
// persisted value was installed; tier is the label applyProfileProcessTuning
// returns when a profile tier installed it.
func goGCSource(replayed bool, tier string) string {
	switch {
	case os.Getenv("GOGC") != "":
		return "env GOGC"
	case replayed:
		return "persisted urnet-tools set gogc"
	case tier != "":
		return tier
	default:
		return goGCSourceDefault
	}
}

// goGCLogPart renders the GOGC half of the one runtime-tuning startup line.
// percent is the live reading (-1 means collection is off); ok is false when
// the runtime does not expose it, in which case the part says unknown rather
// than name a number the process may not run under.
func goGCLogPart(percent int, ok bool, source string) string {
	switch {
	case !ok:
		return fmt.Sprintf("GOGC unknown (source: %s)", source)
	case percent < 0:
		return fmt.Sprintf("GOGC off, collection disabled (source: %s)", source)
	default:
		return fmt.Sprintf("GOGC %d (source: %s)", percent, source)
	}
}

// goRuntimeTuningLogLine is the one line, logged once at startup, that names
// both Go runtime knobs in force and their sources. The soft limit and GOGC
// answer the same question, so they are one line rather than two that could
// disagree about which start they describe. It composes goMemLimitLogLine, so
// the limit half keeps its established wording.
func goRuntimeTuningLogLine(limit int64, limitSource string, gcPercent int, gcOK bool, gcSource string) string {
	return goMemLimitLogLine(limit, limitSource) + "; " + goGCLogPart(gcPercent, gcOK, gcSource)
}
