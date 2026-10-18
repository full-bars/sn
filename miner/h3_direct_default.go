package miner

import "strings"

// h3_direct_default.go ports the 3.23-fix v33 provider's direct-only H3
// defaults (provider/h3_direct_default.go).
//
// A direct-mode node has exactly ONE identity — the native [direct] transport —
// so the resource argument that keeps H3 off by default on a pooled node (one
// QUIC/UDP socket set per proxy identity) does not apply. H3 runs BESIDE H1 and
// falls back to H1 quietly, so enabling it on a direct-only box costs one extra
// socket for a second, independently-fallback transport. On a pooled node that
// cost multiplies by the identity count, which is why the default stays off
// there.
//
// Trigger: proxy resolution has SETTLED as direct-only
// (proxyResolutionZeroValid). Deliberately not "zero proxies right now": a node
// whose proxies are still loading would otherwise flip H3 on and then off again
// as the pool arrives, and the flapping is worse than either steady state.
//
// Precedence: these are DEFAULTS only. resolveH3Mode and the direct-only apply
// consult the control keys first, so an explicit `urnet-tools set h3 off` (or a
// persisted pending override) always wins and the automatic default never
// overrides a deliberate operator choice.

// autoEnableH3 is the implicit opt-in applied to a direct-only node.
const autoEnableH3 = true

// directOnlyH3Defaults returns the default transport opt-in state for the h3
// control key on this node: on when the node has settled as direct-only,
// otherwise off (the pooled-node default).
//
// proxyResolutionNoSource means direct-only AND direct is switched off — nothing
// is served at all — so there is no identity for H3 to attach to and the
// defaults stay off.
func directOnlyH3Defaults(resolution int32) bool {
	return autoEnableH3 && resolution == proxyResolutionZeroValid
}

// defaultH3Setting is the startup fallback for the "h3" key when the operator
// has not set it: on for a settled direct-only node, else URNETWORK_H3 (off
// unless on, 1, true or yes).
func defaultH3Setting(state *controlState, resolution int32) bool {
	if directOnlyH3Defaults(resolution) {
		return true
	}
	return h3Enabled()
}

// applyDirectOnlyH3Defaults reapplies the H3 transport default when a node
// enters or leaves direct-only resolution, unless the operator set the key.
//
// WHY THIS EXISTS SEPARATELY FROM THE STARTUP PATH. The startup control replay
// runs before the first proxy resolution, so at that moment the resolution state
// is still Pending and a direct-only default cannot be known yet. This is called
// from setProxyResolutionStatus, the one choke point every resolution path goes
// through, and fires only on the transition into or out of zero-valid.
//
// What is applied is the mode in force (resolveH3Mode), not a boolean: leaving
// direct-only on a pooled node returns to the automatic-fallback default
// ("direct"), deliberately not "off", which would strip the direct identity's
// fallbacks from an operator who never touched the key. Entering direct-only
// applies "on" through defaultH3Setting.
//
// Precedence is unchanged: an explicit control key (a persisted "off" included)
// always wins. Only an unset key is updated using the current resolution. The
// value is applied to the RUNNING transport only, never persisted, so a later
// restart re-derives it the same way.
//
// The datagram keys have no apply path in this build: QUIC DATAGRAM is always on
// at the pinned connect default (see applyControlLiveSideEffect), so there is
// nothing for a direct-only node to turn on.
func applyDirectOnlyH3Defaults() {
	resolution := currentProxyResolution()
	state := globalControlState
	if state == nil {
		return
	}
	if v, set := state.get("h3"); set && strings.TrimSpace(v) != "" {
		// Operator set it (either value): leave it alone.
		return
	}
	setting := resolveH3Mode(state, resolution)
	if err := applyControlLiveSideEffect("h3", setting.String()); err != nil {
		tlog("[t]h3 default: failed to apply h3=%s: %s\n", setting, err)
		return
	}
	tlog("[t]h3 default: applied h3=%s (direct-only node: %s, the operator left the key unset)\n", setting, onOffLabel(directOnlyH3Defaults(resolution)))
}
