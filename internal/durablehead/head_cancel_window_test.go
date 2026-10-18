//go:build linux || darwin

package durablehead

// The publication window: a cancel or kill after the create marker exists must
// leave a head that reopens. Reconcile clears the pair the marker names; a
// temporary with no marker is unknown and stays refused.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/urnetwork/connect/durablevolume"

	"github.com/urfoundation/sn/internal/durablepath"
)

// A publication fsyncs several files, so a repeated race run on a busy machine
// needs a wider reopen window than a single test run.
const cancelWindowTimeout = 30 * time.Second

var publishCancelBoundaries = []string{
	"temporary-created",
	"pending-written",
	"pending-synced",
	"temporary-written",
	"temporary-synced",
	"snapshot-renamed",
	"snapshot-directory-synced",
	"predecessor-removed",
}

// Boundaries after the next bytes are complete and the checkpoint names them
// finish on reconcile; earlier ones abandon the uncommitted publication.
var publishCancelCompletes = map[string]bool{
	"temporary-written":         true,
	"temporary-synced":          true,
	"snapshot-renamed":          true,
	"snapshot-directory-synced": true,
	"predecessor-removed":       true,
}

func tryOpenOwner(t *testing.T, root string, ctx context.Context) (*durablepath.Directory, *os.File, *Owner, error) {
	t.Helper()
	directory, err := durablepath.Open(ctx, root, durablevolume.ReadWrite, false)
	if err != nil {
		return nil, nil, nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, testSpec.LockName), os.O_RDWR, 0)
	if err != nil {
		directory.Close()
		return nil, nil, nil, err
	}
	owner, err := Open(ctx, directory, lock, testSpec)
	if err != nil {
		lock.Close()
		directory.Close()
		return nil, nil, nil, err
	}
	return directory, lock, owner, nil
}

func tryReconcileOwner(t *testing.T, root string, ctx context.Context) (*durablepath.Directory, *os.File, *Owner, error) {
	t.Helper()
	directory, err := durablepath.Open(ctx, root, durablevolume.ReadWrite, false)
	if err != nil {
		return nil, nil, nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, testSpec.LockName), os.O_RDWR, 0)
	if err != nil {
		directory.Close()
		return nil, nil, nil, err
	}
	owner, err := Reconcile(ctx, directory, lock, testSpec)
	if err != nil {
		lock.Close()
		directory.Close()
		return nil, nil, nil, err
	}
	return directory, lock, owner, nil
}

func closeOwner(t *testing.T, directory *durablepath.Directory, lock *os.File, owner *Owner) {
	t.Helper()
	if err := errors.Join(owner.Close(), lock.Close(), directory.Close()); err != nil {
		t.Fatal(err)
	}
}

func durableTemporaryNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".durable-head-") {
			names = append(names, entry.Name())
		}
	}
	return names
}

func readOwnerValue(t *testing.T, owner *Owner) string {
	t.Helper()
	raw, present, err := owner.Read()
	if err != nil || !present {
		t.Fatalf("read committed value: %v present=%t", err, present)
	}
	return string(raw)
}

// Every cancel point must leave a head that reopens: either the committed
// value with no temporary, or an anchored pending that Reconcile finishes.
func TestPublishCancelAtEveryStepLeavesARecoverableHead(t *testing.T) {
	for _, boundary := range publishCancelBoundaries {
		t.Run(boundary, func(t *testing.T) {
			self := newFixture(t)
			if err := self.owner.Publish([]byte("committed"), nil); err != nil {
				t.Fatal(err)
			}
			if err := self.close(); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(self.volume.Context)
			directory, lock, owner, err := tryOpenOwner(t, self.root, ctx)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			owner.at = func(name string) error {
				if name == boundary {
					cancel()
				}
				return nil
			}
			if err := owner.Publish([]byte("next"), nil); err == nil {
				closeOwner(t, directory, lock, owner)
				t.Fatalf("cancel at %s did not stop the publication", boundary)
			}
			cancel()
			closeOwner(t, directory, lock, owner)

			reopenCtx, reopenCancel := context.WithTimeout(self.volume.Context, cancelWindowTimeout)
			defer reopenCancel()
			reopenDir, reopenLock, reopened, openErr := tryOpenOwner(t, self.root, reopenCtx)
			if openErr == nil {
				// A late cancel can land after the publication is durable.
				value := readOwnerValue(t, reopened)
				if value != "next" || !publishCancelCompletes[boundary] {
					closeOwner(t, reopenDir, reopenLock, reopened)
					t.Fatalf("plain open at %s published %q", boundary, value)
				}
				if names := durableTemporaryNames(t, self.root); len(names) != 0 {
					closeOwner(t, reopenDir, reopenLock, reopened)
					t.Fatalf("cancel at %s left %v", boundary, names)
				}
				closeOwner(t, reopenDir, reopenLock, reopened)
				return
			}
			if !errors.Is(openErr, ErrUncertain) {
				t.Fatalf("plain open at %s: %v", boundary, openErr)
			}
			reconcileDir, reconcileLock, reconciled, reconcileErr := tryReconcileOwner(t, self.root, reopenCtx)
			if reconcileErr != nil {
				t.Fatalf("reconcile after cancel at %s: %v", boundary, reconcileErr)
			}
			want := "committed"
			if publishCancelCompletes[boundary] {
				want = "next"
			}
			if value := readOwnerValue(t, reconciled); value != want {
				closeOwner(t, reconcileDir, reconcileLock, reconciled)
				t.Fatalf("reconcile at %s published %q, want %q", boundary, value, want)
			}
			if names := durableTemporaryNames(t, self.root); len(names) != 0 {
				closeOwner(t, reconcileDir, reconcileLock, reconciled)
				t.Fatalf("reconcile at %s left %v", boundary, names)
			}
			if err := reconciled.Publish([]byte("after"), nil); err != nil {
				closeOwner(t, reconcileDir, reconcileLock, reconciled)
				t.Fatalf("publish after reconcile at %s: %v", boundary, err)
			}
			closeOwner(t, reconcileDir, reconcileLock, reconciled)
		})
	}
}

// A kill between the create marker and the pending checkpoint leaves the pair;
// Reconcile clears it and keeps the committed value.
func TestReconcileClearsACrashedCreatePair(t *testing.T) {
	self := newFixture(t)
	if err := self.owner.Publish([]byte("committed"), nil); err != nil {
		t.Fatal(err)
	}
	prefix := self.owner.prefix()
	if err := self.close(); err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("ab", 16)
	if err := os.WriteFile(filepath.Join(self.root, prefix+"create-"+nonce), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// The real crash pair is empty: the pending checkpoint is what announces
	// the bytes, and this publication never reached it.
	if err := os.WriteFile(filepath.Join(self.root, prefix+nonce), nil, 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(self.volume.Context, cancelWindowTimeout)
	defer cancel()
	if _, _, _, err := tryOpenOwner(t, self.root, ctx); !errors.Is(err, ErrUncertain) {
		t.Fatalf("plain open accepted a create pair: %v", err)
	}
	directory, lock, owner, err := tryReconcileOwner(t, self.root, ctx)
	if err != nil {
		t.Fatalf("reconcile a create pair: %v", err)
	}
	if names := durableTemporaryNames(t, self.root); len(names) != 0 {
		closeOwner(t, directory, lock, owner)
		t.Fatalf("reconcile left %v", names)
	}
	if value := readOwnerValue(t, owner); value != "committed" {
		closeOwner(t, directory, lock, owner)
		t.Fatalf("reconcile changed the committed value to %q", value)
	}
	if err := owner.Publish([]byte("after"), nil); err != nil {
		closeOwner(t, directory, lock, owner)
		t.Fatalf("publish after reconcile: %v", err)
	}
	closeOwner(t, directory, lock, owner)
}

// A temporary no marker names is evidence of tampering: Reconcile refuses it.
func TestReconcileRejectsAnUnknownTemporary(t *testing.T) {
	self := newFixture(t)
	if err := self.owner.Publish([]byte("committed"), nil); err != nil {
		t.Fatal(err)
	}
	prefix := self.owner.prefix()
	if err := self.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(self.root, prefix+strings.Repeat("cd", 16)), []byte("planted"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(self.volume.Context, cancelWindowTimeout)
	defer cancel()
	if _, _, _, err := tryReconcileOwner(t, self.root, ctx); !errors.Is(err, ErrUncertain) {
		t.Fatalf("reconcile accepted an unknown temporary: %v", err)
	}
}

// A marked temporary holding bytes with no checkpoint that names them is
// custody loss or tampering, never something to delete: both opens refuse and
// the bytes stay as evidence.
func TestReconcileRefusesANonEmptyUnanchoredTemporary(t *testing.T) {
	self := newFixture(t)
	if err := self.owner.Publish([]byte("committed"), nil); err != nil {
		t.Fatal(err)
	}
	prefix := self.owner.prefix()
	if err := self.close(); err != nil {
		t.Fatal(err)
	}
	nonce := strings.Repeat("cd", 16)
	marker := filepath.Join(self.root, prefix+"create-"+nonce)
	temporary := filepath.Join(self.root, prefix+nonce)
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(self.volume.Context, cancelWindowTimeout)
	defer cancel()
	if _, _, _, err := tryOpenOwner(t, self.root, ctx); !errors.Is(err, ErrUncertain) {
		t.Fatalf("plain open accepted non-empty unanchored bytes: %v", err)
	}
	if _, _, _, err := tryReconcileOwner(t, self.root, ctx); !errors.Is(err, ErrUncertain) {
		t.Fatalf("reconcile deleted non-empty unanchored bytes: %v", err)
	}
	if raw, err := os.ReadFile(temporary); err != nil || string(raw) != "partial" {
		t.Fatalf("evidence bytes changed: %q %v", raw, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker removed: %v", err)
	}
}

// A marker left by a publish whose temporary never existed names no member; a
// plain open clears it and keeps the committed value.
func TestStaleCreateMarkerClearsOnOpen(t *testing.T) {
	self := newFixture(t)
	if err := self.owner.Publish([]byte("committed"), nil); err != nil {
		t.Fatal(err)
	}
	prefix := self.owner.prefix()
	if err := self.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(self.root, prefix+"create-"+strings.Repeat("ef", 16)), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(self.volume.Context, cancelWindowTimeout)
	defer cancel()
	directory, lock, owner, err := tryOpenOwner(t, self.root, ctx)
	if err != nil {
		t.Fatalf("plain open with a stale marker: %v", err)
	}
	if names := durableTemporaryNames(t, self.root); len(names) != 0 {
		closeOwner(t, directory, lock, owner)
		t.Fatalf("stale marker was not cleared: %v", names)
	}
	if value := readOwnerValue(t, owner); value != "committed" {
		closeOwner(t, directory, lock, owner)
		t.Fatalf("committed value = %q", value)
	}
	closeOwner(t, directory, lock, owner)
}

// A prefixed member that is neither a marker nor a temporary is unknown; both
// open modes refuse it.
func TestUnrecognizedSnapshotMemberRefused(t *testing.T) {
	self := newFixture(t)
	if err := self.owner.Publish([]byte("committed"), nil); err != nil {
		t.Fatal(err)
	}
	prefix := self.owner.prefix()
	if err := self.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(self.root, prefix+"junk"), []byte("planted"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(self.volume.Context, cancelWindowTimeout)
	defer cancel()
	if _, _, _, err := tryOpenOwner(t, self.root, ctx); !errors.Is(err, ErrUncertain) {
		t.Fatalf("plain open accepted an unrecognized member: %v", err)
	}
	if _, _, _, err := tryReconcileOwner(t, self.root, ctx); !errors.Is(err, ErrUncertain) {
		t.Fatalf("reconcile accepted an unrecognized member: %v", err)
	}
}

// A pending that names a temporary which is gone keeps its marker as evidence.
func TestMissingPendingTemporaryKeepsEvidence(t *testing.T) {
	self := newFixture(t)
	if err := self.owner.Publish([]byte("committed"), nil); err != nil {
		t.Fatal(err)
	}
	self.owner.at = func(stage string) error {
		if stage == "pending-written" {
			return unix.EIO
		}
		return nil
	}
	if err := self.owner.Publish([]byte("next"), nil); !errors.Is(err, ErrUncertain) {
		t.Fatalf("faulted publication = %v", err)
	}
	retained := make([]byte, 4096)
	n, err := unix.Fgetxattr(int(self.lock.Fd()), Attribute(testSpec.Kind, testSpec.Name), retained)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint Checkpoint
	if err := json.Unmarshal(retained[:n], &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.Pending == nil {
		t.Fatal("no anchored pending after the fault")
	}
	pendingTemporary := checkpoint.Pending.Temporary
	if err := os.Remove(filepath.Join(self.root, pendingTemporary)); err != nil {
		t.Fatal(err)
	}
	prefix := self.owner.prefix()
	if err := self.close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(self.volume.Context, cancelWindowTimeout)
	defer cancel()
	if _, _, _, err := tryReconcileOwner(t, self.root, ctx); !errors.Is(err, ErrUncertain) {
		t.Fatalf("reconcile accepted a missing pending temporary: %v", err)
	}
	found := false
	for _, name := range durableTemporaryNames(t, self.root) {
		if strings.HasPrefix(name, prefix+"create-") {
			found = true
		}
	}
	if !found {
		t.Fatal("the evidence marker was cleared")
	}
}

// A context cancelled before the publication starts creates nothing.
func TestPublishCancelBeforeStartingCreatesNoTemporary(t *testing.T) {
	self := newFixture(t)
	if err := self.owner.Publish([]byte("committed"), nil); err != nil {
		t.Fatal(err)
	}
	if err := self.close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(self.volume.Context)
	directory, lock, owner, err := tryOpenOwner(t, self.root, ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if err := owner.Publish([]byte("next"), nil); err == nil {
		t.Fatal("a cancelled context published")
	}
	closeOwner(t, directory, lock, owner)
	if names := durableTemporaryNames(t, self.root); len(names) != 0 {
		t.Fatalf("cancelled publication left temporaries %v", names)
	}
	reopenCtx, reopenCancel := context.WithTimeout(self.volume.Context, cancelWindowTimeout)
	defer reopenCancel()
	reopenDir, reopenLock, reopened, err := tryOpenOwner(t, self.root, reopenCtx)
	if err != nil {
		t.Fatal(err)
	}
	if value := readOwnerValue(t, reopened); value != "committed" {
		t.Fatalf("committed value = %q", value)
	}
	closeOwner(t, reopenDir, reopenLock, reopened)
}

func readCheckpointRaw(t *testing.T, self *fixture) []byte {
	t.Helper()
	raw := make([]byte, 4096)
	n, err := unix.Fgetxattr(int(self.lock.Fd()), Attribute(testSpec.Kind, testSpec.Name), raw)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), raw[:n]...)
}

func restoreCheckpointRaw(t *testing.T, root string, raw []byte) {
	t.Helper()
	if err := unix.Setxattr(filepath.Join(root, testSpec.LockName), Attribute(testSpec.Kind, testSpec.Name), raw, unix.XATTR_REPLACE); err != nil {
		t.Fatal(err)
	}
}

// The repair that abandons an empty anchored publication is crash idempotent:
// a fault at every step boundary, with unsynced metadata lost the way a
// machine crash loses it, still reopens and settles on the committed value.
func TestAbandonRepairIsCrashIdempotentAtEveryStep(t *testing.T) {
	steps := []struct {
		stage string
		// revert restores the pre-repair checkpoint bytes, modelling a crash
		// that lost metadata written but never synced. Only the steps before
		// the abandon checkpoint's sync need it.
		revert bool
	}{
		{"abandon-written", true},
		{"abandon-synced", false},
		{"abandon-temporary-removed", false},
		{"abandon-temporary-synced", false},
		{"abandon-marker-removed", false},
	}
	for _, step := range steps {
		t.Run(step.stage, func(t *testing.T) {
			self := newFixture(t)
			if err := self.owner.Publish([]byte("committed"), nil); err != nil {
				t.Fatal(err)
			}
			// Anchor an empty publication: the pending checkpoint is durable
			// and its temporary exists with no byte written, which is exactly
			// the state Reconcile abandons.
			self.owner.at = func(point string) error {
				if point == "pending-synced" {
					return unix.EIO
				}
				return nil
			}
			if err := self.owner.Publish([]byte("next"), nil); !errors.Is(err, ErrUncertain) {
				t.Fatalf("fixture publication = %v", err)
			}
			self.owner.at = nil
			retained := readCheckpointRaw(t, self)
			if err := self.close(); err != nil {
				t.Fatal(err)
			}
			openAt = func(point string) error {
				if point == step.stage {
					return unix.EIO
				}
				return nil
			}
			err := self.open(true)
			openAt = nil
			if err == nil {
				t.Fatalf("fault at %s did not stop the repair", step.stage)
			}
			if step.revert {
				restoreCheckpointRaw(t, self.root, retained)
			}
			if err := self.open(true); err != nil {
				t.Fatalf("repair after a crash at %s is not recoverable: %v", step.stage, err)
			}
			if value := readOwnerValue(t, self.owner); value != "committed" {
				t.Fatalf("value = %q, want the committed generation", value)
			}
			if names := durableTemporaryNames(t, self.root); len(names) != 0 {
				t.Fatalf("repair left %v", names)
			}
			if err := self.owner.Publish([]byte("after"), nil); err != nil {
				t.Fatalf("publish after recovery: %v", err)
			}
		})
	}
}

// A marker cleanup failure after a durable commit is not a publication
// failure: the value is committed, the owner stays usable, and the stale
// marker is cleared by the next open.
func TestPublishSurvivesMarkerCleanupFailure(t *testing.T) {
	self := newFixture(t)
	if err := self.owner.Publish([]byte("committed"), nil); err != nil {
		t.Fatal(err)
	}
	self.owner.at = func(point string) error {
		if point == "marker-remove" {
			return unix.EIO
		}
		return nil
	}
	if err := self.owner.Publish([]byte("next"), nil); err != nil {
		t.Fatalf("a marker cleanup failure failed the publication: %v", err)
	}
	self.owner.at = nil
	if value := readOwnerValue(t, self.owner); value != "next" {
		t.Fatalf("value = %q, want the committed next generation", value)
	}
	if names := durableTemporaryNames(t, self.root); len(names) == 0 {
		t.Fatal("fixture did not leave the marker behind")
	}
	if err := self.owner.Publish([]byte("again"), nil); err != nil {
		t.Fatalf("owner refused a later publication: %v", err)
	}
	if err := self.close(); err != nil {
		t.Fatal(err)
	}
	if err := self.open(false); err != nil {
		t.Fatalf("open after a leftover marker: %v", err)
	}
	if names := durableTemporaryNames(t, self.root); len(names) != 0 {
		t.Fatalf("stale marker was not cleared: %v", names)
	}
}
