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

// clearExtenderRoleReleaser clears the registration when it is still the given
// slot's, and leaves it alone otherwise. A slot that ends while a relaunch has
// already registered its successor must not wipe the successor's releaser, or
// the next hot swap would release nothing.
func clearExtenderRoleReleaser(releaser extenderCarrierReleaser) {
	extenderRoleReleaser.Lock()
	defer extenderRoleReleaser.Unlock()
	if extenderRoleReleaser.releaser == releaser {
		extenderRoleReleaser.releaser = nil
	}
}

// releaseExtenderCarriersForSuccessor frees the carrier ports for the
// successor, on either hand-off path.
//
// In place, the listeners are inherited across execve, so without this the
// successor cannot bind tcp 443: the port it needs is held by a descriptor it
// does not know it owns, and an in-place swap never performs the process exit
// that would free it. On the baton path the successor is a separate process,
// but the carriers are bound without SO_REUSEPORT, so it cannot bind until
// this process lets go; releasing at the ACK hands the port over without
// waiting for the end of the drain.
func releaseExtenderCarriersForSuccessor() {
	extenderRoleReleaser.Lock()
	releaser := extenderRoleReleaser.releaser
	extenderRoleReleaser.Unlock()
	if releaser == nil {
		return
	}
	releaser.ReleaseExtenderCarriers()
	hotswapLog("⚡ [hotswap] released the extender carrier ports for the successor\n")
}
