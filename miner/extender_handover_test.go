package miner

import (
	"bufio"
	"io"
	"strings"
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

	releaseExtenderCarriersForSuccessor()

	if releaser.released != 1 {
		t.Fatalf("released %d times, want 1", releaser.released)
	}
}

// A process that holds no role swaps without releasing anything, and without
// panicking on the empty registry.
func TestReleaseExtenderCarriersForExecWithoutRole(t *testing.T) {
	setExtenderRoleReleaser(nil)

	releaseExtenderCarriersForSuccessor()
}

// A slot that has ended is not released by a later swap.
func TestExtenderRoleReleaserClearsWithTheSlot(t *testing.T) {
	releaser := &fakeCarrierReleaser{}
	setExtenderRoleReleaser(releaser)
	setExtenderRoleReleaser(nil)

	releaseExtenderCarriersForSuccessor()

	if releaser.released != 0 {
		t.Fatalf("a cleared registry released %d times", releaser.released)
	}
}

// The baton path never execs, so it never reaches the in-place release. The
// successor's role could not bind tcp 443 until this parent exited; the ACK is
// the point where the parent stops serving and the port is handed over.
func TestBatonAckReleasesTheExtenderCarriers(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", shortStateDir(t))
	closeProviderStore()
	t.Cleanup(closeProviderStore)

	releaser := &fakeCarrierReleaser{}
	setExtenderRoleReleaser(releaser)
	t.Cleanup(func() { setExtenderRoleReleaser(nil) })

	session := &HotswapParentSession{
		Reader: bufio.NewReader(strings.NewReader(`{"type":"ACK"}` + "\n")),
		Writer: io.Discard,
	}
	if err := handoffBatonSendTakeover(session, 1, 2); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if releaser.released != 1 {
		t.Fatalf("carriers released %d times after the ACK, want 1", releaser.released)
	}
}

// A handoff the parent abandons leaves it serving, so its carriers stay bound.
func TestBatonAbortKeepsTheExtenderCarriers(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", shortStateDir(t))
	closeProviderStore()
	t.Cleanup(closeProviderStore)

	releaser := &fakeCarrierReleaser{}
	setExtenderRoleReleaser(releaser)
	t.Cleanup(func() { setExtenderRoleReleaser(nil) })

	session := &HotswapParentSession{
		Reader: bufio.NewReader(strings.NewReader(`{"type":"ERROR","error":"nope"}` + "\n")),
		Writer: io.Discard,
	}
	if err := handoffBatonSendTakeover(session, 1, 2); err == nil {
		t.Fatal("expected the handoff to abort")
	}
	if releaser.released != 0 {
		t.Fatalf("an aborted handoff released the carriers %d times", releaser.released)
	}
}
