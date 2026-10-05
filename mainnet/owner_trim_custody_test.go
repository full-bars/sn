// Original native bytes and the numbered attempt stay under one physical owner.
// Deterministic callbacks replace custody at the actual effect boundaries.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/urnetwork/connect/durablevolume"
)

// The authority boundary is synchronous, so no scheduler timing selects a fault.
type ownerTrimCustodyTestAuthority struct{ before func() }

func (self *ownerTrimCustodyTestAuthority) authorize(context.Context, ownerTrimExecutionConfig, ownerTrimActionReconciliation) error {
	self.before()
	return nil
}

// The real sixth journal retains an imported synthetic signature; the execution
// owner has no signing port and can only send those original bytes.
func newOwnerTrimCustodyTestOwner(t *testing.T) (*ownerTrimExecutor, *ownerTrimStore, *ownerTrimTestChain, []byte) {
	t.Helper()
	template := newOwnerTrimActionTestFixture(t)
	preparation, f := ownerTrimPreparedTestFixture(t, template.pair.Public())
	store, err := openOwnerTrimStore(f.storage.Context, preparation.preparation, f.config, f.key, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.close() })
	original, err := os.ReadFile(f.config.Action.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := hex.DecodeString(f.config.Action.Payload[2:])
	signature, err := f.pair.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.config.Action.signed(signature)
	if err != nil {
		t.Fatal(err)
	}
	record.Phase, record.Signature = "signed", hex.EncodeToString(signature)
	record.RawExtrinsic, record.ExtrinsicHash = "0x"+hex.EncodeToString(raw), rootExtrinsicHash(raw)
	owner, _, _, chain := ownerTrimTestOwner(t, f)
	owner.store, owner.signer = store, nil
	if err := owner.persist(record); err != nil {
		t.Fatal(err)
	}
	return owner, store, chain, original
}

// Identical marker bytes do not retain the inode carrying the exclusive flock.
func TestOwnerTrimExclusiveMarkerReplacementRefusesSend(t *testing.T) {
	owner, store, chain, _ := newOwnerTrimCustodyTestOwner(t)
	var restore func()
	owner.authority = &ownerTrimCustodyTestAuthority{before: func() {
		restore = bootstrapReadinessTestReplace(t, store.config.Action.StatePath+".lock")
	}}
	_, err := owner.step(t.Context())
	if restore == nil {
		t.Fatal("authority boundary was not reached", err)
	}
	restore()
	if !errors.Is(err, durablevolume.ErrIdentity) || chain.sends != 0 {
		t.Fatalf("replaced exclusive marker admitted native send: sends=%d error=%v", chain.sends, err)
	}
	if _, err := store.load(); !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("restored marker renewed a failed native owner", err)
	}
}

// A cached signed record cannot recreate completed custody after deletion.
func TestOwnerTrimDeletedSignedJournalIsNeverRecreated(t *testing.T) {
	owner, store, chain, _ := newOwnerTrimCustodyTestOwner(t)
	owner.authority = &ownerTrimCustodyTestAuthority{before: func() {
		if err := os.Remove(store.config.Action.StatePath); err != nil {
			t.Fatal(err)
		}
	}}
	_, err := owner.step(t.Context())
	_, statErr := os.Lstat(store.config.Action.StatePath)
	if !errors.Is(err, durablevolume.ErrIdentity) || chain.sends != 0 || !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("deleted signed custody was recreated or sent: sends=%d error=%v state=%v", chain.sends, err, statErr)
	}
}

// Loss after the counted record's directory sync must still stop transport.
func TestOwnerTrimLostCountedJournalRefusesSend(t *testing.T) {
	owner, store, chain, _ := newOwnerTrimCustodyTestOwner(t)
	fired := false
	store.syncDirectory = func(directory *os.File) error {
		raw, err := os.ReadFile(store.config.Action.StatePath)
		var record ownerTrimRecord
		if err == nil {
			err = decodePlanJson(raw, &record)
		}
		if err != nil {
			return err
		}
		if record.Broadcasts == 1 {
			fired = true
			if err := os.Remove(store.config.Action.StatePath); err != nil {
				return err
			}
		}
		return directory.Sync()
	}
	_, err := owner.step(t.Context())
	if !fired || !errors.Is(err, durablevolume.ErrIdentity) || chain.sends != 0 {
		t.Fatalf("lost counted native custody admitted send: fault=%t sends=%d error=%v", fired, chain.sends, err)
	}
}

// A correctly hashed earlier record cannot erase this owner's retained bytes.
func TestOwnerTrimEarlierValidJournalIsIntegrityFailure(t *testing.T) {
	_, store, _, earlier := newOwnerTrimCustodyTestOwner(t)
	current, err := os.ReadFile(store.config.Action.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.config.Action.StatePath, earlier, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = store.load()
	if !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("earlier valid reservation erased retained native signature", err)
	}
	if err := os.WriteFile(store.config.Action.StatePath, current, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.load(); !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("restoring signed bytes erased native integrity failure", err)
	}
}

// Permission, link and byte faults share the same final exclusive-owner guard.
func TestOwnerTrimExclusiveCustodyRejectsFilesystemChanges(t *testing.T) {
	for _, kind := range []string{"marker-bytes", "marker-mode", "marker-link", "journal-mode", "journal-link", "journal-bytes"} {
		owner, store, chain, _ := newOwnerTrimCustodyTestOwner(t)
		owner.authority = &ownerTrimCustodyTestAuthority{before: func() {
			path := store.config.Action.StatePath
			if strings.HasPrefix(kind, "marker-") {
				path += ".lock"
			}
			var err error
			switch {
			case strings.HasSuffix(kind, "-mode"):
				err = os.Chmod(path, 0644)
			case strings.HasSuffix(kind, "-link"):
				err = os.Link(path, path+".synthetic-alias")
			default:
				var raw []byte
				raw, err = os.ReadFile(path)
				if err == nil {
					err = os.WriteFile(path, append(raw, '\n'), 0600)
				}
			}
			if err != nil {
				t.Fatal(kind, err)
			}
		}}
		_, err := owner.step(t.Context())
		if !errors.Is(err, durablevolume.ErrIdentity) || chain.sends != 0 {
			t.Fatalf("%s retained native effect authority: sends=%d error=%v", kind, chain.sends, err)
		}
	}
}

// The old prefix-only marker can resume its untouched first reservation. No
// signed journal, consumed attempt or completed claim is reset by this path.
func TestOwnerTrimIncompleteExclusiveClaimRemainsRecoverable(t *testing.T) {
	for _, retained := range []bool{false, true} {
		preparation, f := ownerTrimPreparedTestFixture(t)
		store, err := openOwnerTrimStore(f.storage.Context, preparation.preparation, f.config, f.key, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.close(); err != nil {
			t.Fatal(err)
		}
		marker := rootObjectHash(f.config) + "\n" + f.key + "\n"
		if err := os.WriteFile(f.config.Action.StatePath+".lock", []byte(marker), 0600); err != nil {
			t.Fatal(err)
		}
		if !retained {
			if err := os.Remove(f.config.Action.StatePath); err != nil {
				t.Fatal(err)
			}
		}
		store, err = openOwnerTrimStore(f.storage.Context, preparation.preparation, f.config, f.key, false)
		if err != nil {
			t.Fatal("intact original incomplete claim could not resume", retained, err)
		}
		record, err := store.load()
		if err != nil || record.Phase != "reserved" || record.Signature != "" || record.Broadcasts != 0 {
			t.Fatal("incomplete claim invented native progress", retained, record.Phase, err)
		}
		if err := store.close(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.load(); err == nil {
			t.Fatal("closed native owner retained custody")
		}
	}
}
