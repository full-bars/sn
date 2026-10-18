//go:build linux || darwin

package durablehead

// The publication window: a cancel or kill after the create marker exists must
// leave a head that reopens. Reconcile clears the pair the marker names; a
// temporary with no marker is unknown and stays refused.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/internal/durablepath"
	"github.com/urnetwork/connect/durablevolume"
)

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

			reopenCtx, reopenCancel := context.WithTimeout(self.volume.Context, 5*time.Second)
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
