package miner

import (
	"sync"
)

// extenderCarrierReleaser is the device the extender role runs on, as far as
// the hot swap needs it.
type extenderCarrierReleaser interface {
	ReleaseExtenderCarriers()
}

// extenderRoleReleaser records the one slot that holds the extender role in
// this process, or nil when no slot holds it.
//
// The role lives inside the slot's provide goroutine, and the in-place hot
// swap runs on the main path, so the two cannot call each other directly. A
// registry of one is enough: only the direct slot can hold the role, and it
// holds it for the life of the process or not at all.
var extenderRoleReleaser struct {
	sync.Mutex
	releaser extenderCarrierReleaser
}

// setExtenderRoleReleaser records the slot that holds the role, or clears it
// when that slot ends.
func setExtenderRoleReleaser(releaser extenderCarrierReleaser) {
	extenderRoleReleaser.Lock()
	defer extenderRoleReleaser.Unlock()
	extenderRoleReleaser.releaser = releaser
}

// releaseExtenderCarriersForExec frees the carrier ports before the image is
// replaced.
//
// The listeners are inherited across execve, so without this the successor
// cannot bind tcp 443: the port it needs is held by a descriptor it does not
// know it owns. Releasing here hands the port to the successor instead of
// waiting for a process exit that an in-place swap never performs.
func releaseExtenderCarriersForExec() {
	extenderRoleReleaser.Lock()
	releaser := extenderRoleReleaser.releaser
	extenderRoleReleaser.Unlock()
	if releaser == nil {
		return
	}
	releaser.ReleaseExtenderCarriers()
	hotswapLog("⚡ [hotswap] released the extender carrier ports for the successor\n")
}
