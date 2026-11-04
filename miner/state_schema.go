package miner

// state_schema.go is the version stamp on provider_state.db and the guard that
// keeps an older build from reading a layout it does not understand.
//
// The stamp is the release gate for every stage that moves data into the store
// (STATE-MIGRATION-SPEC section 2.3, "The store is newer than the binary"). A
// build that ships this guard fails closed on a store a later build stamped
// with a higher min_reader: the open is refused, so no consumer reads the new
// layout as if it were the old one and nothing is written to it. Builds that
// predate the guard cannot be protected, which is why it ships first.
//
// The stamp lives in its own bucket so a damaged or unreadable stamp can never
// be confused with the extender activation budget in bucket "state".

import (
	"encoding/json"
	"errors"
	"fmt"

	"go.etcd.io/bbolt"
)

const (
	providerStoreBucketMeta = "meta"
	providerStoreKeySchema  = "schema"

	// providerStoreSchemaVersion is the layout version this build writes.
	providerStoreSchemaVersion = 1
	// providerStoreSchemaSupported is the highest min_reader this build can
	// serve. A stamp whose min_reader is above it was written by a build whose
	// layout this one does not know, and the store is refused.
	providerStoreSchemaSupported = 1
)

// providerStoreSchema is the stamp. Stages records which stage of the move has
// completed, so a later build can tell what the store holds.
type providerStoreSchema struct {
	Version   int               `json:"version"`
	MinReader int               `json:"min_reader"`
	Stages    map[string]string `json:"stages,omitempty"`
}

// errProviderStoreNewer matches the refusal of a store a newer build wrote, or
// whose stamp this build cannot read and so cannot rule out as newer.
var errProviderStoreNewer = errors.New("provider state store was written by a newer build")

type providerStoreNewerError struct {
	path      string
	minReader int
	// unreadable is set when the stamp did not decode or named no version. A
	// stamp this build cannot read is treated as newer, not as absent: reading
	// on would mis-read the layout, and writing would overwrite the stamp.
	unreadable error
}

func (e *providerStoreNewerError) Error() string {
	if e.unreadable != nil {
		return fmt.Sprintf("state store %s has a schema stamp this build cannot read (%v); it may have been written by a newer build, refusing to start slots or mint new identities",
			e.path, e.unreadable)
	}
	return fmt.Sprintf("state store %s was written by a newer build (schema min_reader %d, this build reads up to %d); refusing to start slots or mint new identities",
		e.path, e.minReader, providerStoreSchemaSupported)
}

func (e *providerStoreNewerError) Is(target error) bool { return target == errProviderStoreNewer }

// providerStoreSchemaNewerCategory is the persistent error category for a store
// refused by the guard, so the refusal is on the scrape and not only in a log.
const providerStoreSchemaNewerCategory = "state_store_newer"

// minReaderOf is the version a reader needs, from a decoded stamp. A stamp
// without min_reader needs its own version.
func (s providerStoreSchema) minReaderOf() int {
	if s.MinReader > 0 {
		return s.MinReader
	}
	return s.Version
}

// checkProviderStoreSchema refuses a store that is newer than this build and
// stamps one that carries no stamp yet. It runs in one transaction that
// writes only when it has nothing to refuse, so a refused store is left
// byte-for-byte as found.
func checkProviderStoreSchema(store *providerStore) error {
	if store == nil || store.db == nil {
		return errProviderStoreClosed
	}
	return runProviderStoreUpdate(store.db, func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(providerStoreBucketMeta))
		var raw []byte
		if b != nil {
			raw = b.Get([]byte(providerStoreKeySchema))
		}
		if raw == nil {
			// A store no stamp has touched: new, or written by a build older
			// than the guard. It is stamped with the layout this build writes.
			return putProviderStoreSchema(tx, providerStoreSchema{
				Version:   providerStoreSchemaVersion,
				MinReader: providerStoreSchemaVersion,
			})
		}
		var stamp providerStoreSchema
		if err := json.Unmarshal(raw, &stamp); err != nil {
			return &providerStoreNewerError{path: store.path, unreadable: err}
		}
		if stamp.minReaderOf() < 1 {
			return &providerStoreNewerError{path: store.path, unreadable: errors.New("no version")}
		}
		if need := stamp.minReaderOf(); need > providerStoreSchemaSupported {
			return &providerStoreNewerError{path: store.path, minReader: need}
		}
		return nil
	})
}

func putProviderStoreSchema(tx *bbolt.Tx, stamp providerStoreSchema) error {
	b, err := tx.CreateBucketIfNotExists([]byte(providerStoreBucketMeta))
	if err != nil {
		return err
	}
	raw, err := json.Marshal(stamp)
	if err != nil {
		return err
	}
	return b.Put([]byte(providerStoreKeySchema), raw)
}

// markProviderStoreStage records that a stage of the move has completed. The
// stamp is read as a generic object and only its "stages" member is changed,
// so a field a newer build added (one it left readable to this build by
// keeping min_reader low) is carried through unchanged.
func markProviderStoreStage(store *providerStore, stage, mode string) error {
	if store == nil {
		return errProviderStoreClosed
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.db == nil {
		return errProviderStoreClosed
	}
	return runProviderStoreUpdate(store.db, func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(providerStoreBucketMeta))
		if b == nil {
			return fmt.Errorf("schema stamp missing")
		}
		raw := b.Get([]byte(providerStoreKeySchema))
		if raw == nil {
			return fmt.Errorf("schema stamp missing")
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		stages := map[string]string{}
		if existing, ok := object["stages"]; ok {
			if err := json.Unmarshal(existing, &stages); err != nil {
				return err
			}
		}
		if stages[stage] == mode {
			return nil
		}
		stages[stage] = mode
		encoded, err := json.Marshal(stages)
		if err != nil {
			return err
		}
		object["stages"] = encoded
		out, err := json.Marshal(object)
		if err != nil {
			return err
		}
		return b.Put([]byte(providerStoreKeySchema), out)
	})
}
