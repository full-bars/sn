package miner

import (
	"testing"
)

type fakeCarrierReleaser struct {
	released int
}

func (self *fakeCarrierReleaser) ReleaseExtenderCarriers() {
	self.released++
}

// The swap releases the role's carrier ports before the image is replaced, so
// the successor can bind tcp 443 instead of inheriting a descriptor it does
// not know it owns.
func TestReleaseExtenderCarriersForExecReleasesTheRole(t *testing.T) {
	releaser := &fakeCarrierReleaser{}
	setExtenderRoleReleaser(releaser)
	t.Cleanup(func() { setExtenderRoleReleaser(nil) })

	releaseExtenderCarriersForExec()

	if releaser.released != 1 {
		t.Fatalf("released %d times, want 1", releaser.released)
	}
}

// A process that holds no role swaps without releasing anything, and without
// panicking on the empty registry.
func TestReleaseExtenderCarriersForExecWithoutRole(t *testing.T) {
	setExtenderRoleReleaser(nil)

	releaseExtenderCarriersForExec()
}

// A slot that has ended is not released by a later swap.
func TestExtenderRoleReleaserClearsWithTheSlot(t *testing.T) {
	releaser := &fakeCarrierReleaser{}
	setExtenderRoleReleaser(releaser)
	setExtenderRoleReleaser(nil)

	releaseExtenderCarriersForExec()

	if releaser.released != 0 {
		t.Fatalf("a cleared registry released %d times", releaser.released)
	}
}
