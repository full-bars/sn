package miner

import (
	"path/filepath"
	"testing"
	"time"
)

// blockedFor reports whether done stays open for the whole wait. A refusal
// publication that is correctly ordered behind an admission never finishes in
// this time; one that is not finishes at once.
func blockedFor(done <-chan struct{}, wait time.Duration) bool {
	select {
	case <-done:
		return false
	case <-time.After(wait):
		return true
	}
}

// A refusal published after the admission read what was held, and before the
// admission committed, used to be missed: the admission recorded a post into a
// wait the refusal had already set. The publication now waits for the
// admission, and the next transaction charges it.
func TestRefusalPublishedMidAdmissionWaitsForTheCommit(t *testing.T) {
	testExtenderState(t)
	if providerStoreInstance() == nil {
		t.Fatal("store did not open")
	}
	published := make(chan struct{})
	extenderAdmissionSnapshotHook = func() {
		go func() {
			holdExtenderRefusal(extenderTestAccountA, "scope.901")
			close(published)
		}()
		if !blockedFor(published, 200*time.Millisecond) {
			t.Error("a refusal was published while an admission that had not read it was in flight")
		}
	}
	t.Cleanup(func() { extenderAdmissionSnapshotHook = nil })

	if allow, _, _ := extenderActivationAllow(time.Now(), 1, extenderTestAccountA); !allow {
		t.Fatal("the admission that ran before the refusal was decided should pass")
	}
	extenderAdmissionSnapshotHook = nil
	select {
	case <-published:
	case <-time.After(10 * time.Second):
		t.Fatal("the publication never ran after the admission committed")
	}
	if allow, wait, _ := extenderActivationAllow(time.Now(), 1, extenderTestAccountA); allow || wait <= 0 {
		t.Fatalf("admission after the publication = %t (wait %s), want held by the refusal", allow, wait)
	}
	if got := readRefusals(t, extenderTestAccountA); got != 1 {
		t.Fatalf("refusals = %d, want the published refusal charged once", got)
	}
}

// A hotswap candidate and its draining parent are two processes, so the
// ordering needs a lock they share. Another holder of the lock file stands for
// the candidate's admission: the parent's publication must wait for it.
func TestRefusalPublicationWaitsForAnotherProcessesAdmission(t *testing.T) {
	testExtenderState(t)
	dir, err := providerStateDir()
	if err != nil {
		t.Fatal(err)
	}
	other, err := acquireFileLock(filepath.Join(dir, extenderRefusalBarrierName))
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			other()
		}
	})
	published := make(chan struct{})
	go func() {
		holdExtenderRefusal(extenderTestAccountA, "scope.902")
		close(published)
	}()
	if !blockedFor(published, 200*time.Millisecond) {
		t.Fatal("a refusal was published while another process held the barrier")
	}
	other()
	released = true
	select {
	case <-published:
	case <-time.After(10 * time.Second):
		t.Fatal("the publication never ran after the other process released the barrier")
	}
}
