package miner

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// h3_mode_apply.go ports the 3.23-fix v33 provider's live h3 application
// (provider/h3_mode_apply.go plus the environment half of provider/h3_direct.go).
//
// One difference is deliberate. v33 flips the connect library's process-wide H3
// gate (connect.SetH3Enabled) so a change reaches every running transport
// without a redial. The pinned full-bars/connect does not carry that gate (see
// h3_mode.go), so the mode is read when an identity builds its platform
// transport and a change relaunches the identities whose policy moved. The
// relaunch is what the operator sees either way; the mode is applied to the
// process state here so later changes can compare against what the running
// identities were actually built with.

// h3Enabled reports whether the operator opted in to H3 (QUIC) with
// URNETWORK_H3. It is OFF unless the variable is on, 1, true or yes. This is
// only the startup default for the runtime mode: the h3 control key overrides
// it and can be changed without a restart (see resolveH3Mode).
func h3Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("URNETWORK_H3"))) {
	case "on", "1", "true", "yes":
		return true
	}
	return false
}

// h3AppliedSetting is the setting last applied, nil before the first
// application (the startup replay), which must not restart anything.
var h3AppliedSetting atomic.Pointer[h3Setting]

// h3DefaultModeName is the mode a cleared h3 key returns to: what a restart
// would use, so the live state matches the persisted one. It is the
// URNETWORK_H3 opt-in — "on" when the variable is on, 1, true or yes, else the
// automatic fallbacks ("direct"). A settled direct-only node reaches "on"
// through defaultH3Setting at its next transport build, exactly as it would
// after a restart.
func h3DefaultModeName() string {
	if h3Enabled() {
		return h3ModeOnName
	}
	return h3ModeDirectName
}

// applyH3Mode applies an h3 setting to the running miner. It records the setting
// and re-ranks the proxy set; the transport mode policy is fixed when an
// identity builds its transport, so the caller relaunches the identities the
// setting moved (restartDirectIdentity, relaunchH3ProxiesNow).
//
// The returned relaunch says whether the DIRECT identity's policy changed. The
// first application (the startup replay) reports false: nothing is running to
// relaunch.
func applyH3Mode(setting h3Setting) (previous h3Setting, relaunch bool) {
	prev := h3AppliedSetting.Swap(&setting)
	rerankH3(setting)
	if prev == nil {
		return setting, false
	}
	return *prev, prev.directPolicy() != setting.directPolicy()
}

// replayH3ModeAtStartup applies the resolved h3 setting once, before any
// identity builds its transport, so a later control change compares against the
// mode the running identities were built with. The startup application never
// relaunches.
func replayH3ModeAtStartup(state *controlState) {
	applyH3Mode(resolveH3Mode(state, currentProxyResolution()))
}

// h3IdentityRelauncher is the running provide run's per-identity relaunch
// facility: stop one identity and let its worker start it again under the
// current setting. Nil outside a provide run (offline tools, unit tests).
type h3IdentityRelauncher interface {
	// detachForRelaunch stops one running identity, or reports false when it is
	// not running. The identity's worker loop notices the request and runs it
	// again with the current setting.
	detachForRelaunch(key string) bool
	// hasClients reports whether an identity is serving clients right now. The
	// relaunch pass leaves such an identity alone and retries it later.
	hasClients(key string) bool
	// runContext is the run's lifecycle: a relaunch pass ends with it.
	runContext() context.Context
}

var (
	h3RelauncherMu sync.Mutex
	h3Relauncher   h3IdentityRelauncher
)

// setH3IdentityRelauncher publishes the running provide run's relaunch
// facility. The run clears it on the way out.
func setH3IdentityRelauncher(relauncher h3IdentityRelauncher) {
	h3RelauncherMu.Lock()
	defer h3RelauncherMu.Unlock()
	h3Relauncher = relauncher
}

// currentH3IdentityRelauncher returns the published relauncher, or nil.
func currentH3IdentityRelauncher() h3IdentityRelauncher {
	h3RelauncherMu.Lock()
	defer h3RelauncherMu.Unlock()
	return h3Relauncher
}

// restartDirectIdentity relaunches the native [direct] identity so it rebuilds
// its platform transport under the current h3 mode. The worker loop starts it
// again once its goroutine has unwound, re-reading the mode at the next
// transport build.
func restartDirectIdentity() {
	if r := currentH3IdentityRelauncher(); r != nil {
		r.detachForRelaunch(directProxyKey)
	}
}
