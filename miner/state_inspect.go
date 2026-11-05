package miner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"

	"go.etcd.io/bbolt"
)

// Read-only inspection of the provider store: `state verify` and `state
// identities`. Both are answered by the OWNER process from the handle it
// already holds, so an inspection can never be the open that creates, stamps,
// recovers or quarantines the file. Nothing here writes, repairs or calls
// tx.Check() (its goroutine panic is not contained).
//
// A status other than stateClean always means "not shown to be healthy": the
// zero value of a report is stateUnchecked, an error is never folded into
// "clean", and an uninitialized migration family is "unchecked", not "equal".
//
// Redaction happens where a value is built: a report type has no field that
// could hold a JWT, seed, proxy password or file path, and a slot is a
// redactedSlot from the moment it leaves the store. Error text is never copied
// into a report, because store errors carry file paths.

type stateStatus string

const (
	stateClean       stateStatus = "clean"
	stateUnchecked   stateStatus = "unchecked"
	statePending     stateStatus = "pending"
	stateDiverged    stateStatus = "diverged"
	stateReleased    stateStatus = "released"
	stateUnavailable stateStatus = "unavailable"
	stateLocked      stateStatus = "locked"
	stateConflict    stateStatus = "conflict"
	stateUnreadable  stateStatus = "unreadable"
	stateNewer       stateStatus = "newer"
	stateCorrupt     stateStatus = "corrupt"
)

// stateSeverity orders statuses so a report carries its worst one. An unknown
// or empty status ranks as unchecked, never as clean.
func stateSeverity(s stateStatus) int {
	switch s {
	case stateClean:
		return 0
	case stateUnchecked:
		return 1
	case statePending:
		return 2
	case stateDiverged:
		return 3
	case stateReleased:
		return 4
	case stateUnavailable:
		return 5
	case stateLocked:
		return 6
	case stateConflict:
		return 7
	case stateUnreadable:
		return 8
	case stateNewer:
		return 9
	case stateCorrupt:
		return 10
	}
	return 1
}

func worseState(a, b stateStatus) stateStatus {
	if stateSeverity(b) > stateSeverity(a) {
		return b
	}
	return a
}

// stateIdentityFamily is the one family S1 knows: the client JWT file against
// its copy in migration_records. Nothing writes either side yet.
const stateIdentityFamily = "identity"

type stateFamilyReport struct {
	Name       string      `json:"name"`
	Mode       string      `json:"mode"`
	Seal       string      `json:"seal"`
	Generation uint64      `json:"generation,omitempty"`
	Decodable  string      `json:"decodable"`
	Reconcile  string      `json:"reconcile"`
	Status     stateStatus `json:"status"`
}

type stateVerifyReport struct {
	Status        stateStatus         `json:"status"`
	Store         stateStatus         `json:"store"`
	Schema        string              `json:"schema"`
	SchemaVersion int                 `json:"schema_version,omitempty"`
	Records       int                 `json:"records"`
	Undecodable   int                 `json:"undecodable"`
	Families      []stateFamilyReport `json:"families"`
	ReadOnly      bool                `json:"read_only"`
}

// newStateVerifyReport is the value a report starts from: unchecked.
func newStateVerifyReport() stateVerifyReport {
	return stateVerifyReport{Status: stateUnchecked, Store: stateUnchecked, Schema: string(stateUnchecked), ReadOnly: true}
}

// redactedSlot is a stable, non-reversible label for a slot. Slot names derive
// from a proxy address and user, so they are hashed before they are reported.
type redactedSlot string

func redactSlot(raw string) redactedSlot {
	sum := sha256.Sum256([]byte("state-inspect-slot-v1\x00" + raw))
	return redactedSlot("slot-" + hex.EncodeToString(sum[:4]))
}

type stateIdentitySlot struct {
	Slot    redactedSlot `json:"slot"`
	Outcome string       `json:"outcome"`
	Events  int          `json:"events"`
}

type stateIdentitiesReport struct {
	Status      stateStatus         `json:"status"`
	Slots       []stateIdentitySlot `json:"slots"`
	Events      int                 `json:"events"`
	Undecodable int                 `json:"undecodable"`
	ReadOnly    bool                `json:"read_only"`
}

func newStateIdentitiesReport() stateIdentitiesReport {
	return stateIdentitiesReport{Status: stateUnchecked, ReadOnly: true}
}

var errStateStoreNotOpen = errors.New("provider state store is not open in this process")

// ownerStoreForInspection returns the handle the owner already holds. It never
// opens: a store that is not open is reported as such, with the cached reason
// the last open failed.
func ownerStoreForInspection() (*providerStore, error) {
	globalProviderStoreMu.Lock()
	defer globalProviderStoreMu.Unlock()
	if globalProviderStoreReleased {
		return nil, errProviderStoreReleased
	}
	if globalProviderStore != nil {
		return globalProviderStore, nil
	}
	if globalProviderStoreFailedErr != nil {
		return nil, globalProviderStoreFailedErr
	}
	return nil, errStateStoreNotOpen
}

// classifyStoreError maps a store error to a status without keeping its text.
func classifyStoreError(err error) stateStatus {
	switch {
	case err == nil:
		return stateClean
	case errors.Is(err, errProviderStoreNewer):
		return stateNewer
	case errors.Is(err, errProviderStoreReleased):
		return stateReleased
	case errors.Is(err, bbolt.ErrTimeout):
		return stateLocked
	case isProviderStoreCorrupt(err):
		return stateCorrupt
	}
	return stateUnavailable
}

// inspectView is migrationView without its schema gate, so a missing or newer
// stamp is reported by the caller instead of hiding the rest of the report.
// It takes the same mutex and the same panic barrier as every other reader.
func inspectView(s *providerStore, fn func(*bbolt.Tx) error) error {
	if s == nil {
		return errProviderStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		if s.released {
			return errProviderStoreReleased
		}
		return errProviderStoreClosed
	}
	return runProviderStoreView(s.db, fn)
}

// stateIdentityFileSide reads the retained identity file as raw bytes. It does
// not go through clientJWTStore, whose load snapshots and prunes.
var stateIdentityFileSide = func() migrationSide {
	store := loadGlobalClientJWTStore()
	if store == nil || store.path == "" {
		return migrationSide{}
	}
	data, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return migrationSide{}
	}
	if err != nil {
		return migrationSide{Err: errors.New("identity file unreadable")}
	}
	return migrationSide{Present: true, Record: migrationRecord{Format: "client-jwts/json-file", Bytes: data}}
}

func safeFamilyName(name string) string {
	if name == "" || len(name) > 32 {
		return "unknown"
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return "unknown"
		}
	}
	return name
}

func comparisonStatus(c migrationComparison) stateStatus {
	switch c {
	case migrationEqual:
		return stateClean
	case migrationOnlyFile, migrationOnlyStore:
		return stateDiverged
	case migrationConflict:
		return stateConflict
	case migrationUnreadable:
		return stateUnreadable
	}
	return statePending
}

// buildStateVerifyReport inspects the store it is given. storeErr is why the
// caller has no store; with a store, any read failure is classified the same
// way and the report stays not-clean.
func buildStateVerifyReport(store *providerStore, storeErr error, fileSide func() migrationSide) stateVerifyReport {
	report := newStateVerifyReport()
	if store == nil {
		if storeErr == nil {
			storeErr = errStateStoreNotOpen
		}
		report.Store = classifyStoreError(storeErr)
		report.Status = report.Store
		return report
	}
	err := inspectView(store, func(tx *bbolt.Tx) error {
		// schema
		meta := tx.Bucket([]byte(providerStoreBucketMeta))
		var stamp []byte
		if meta != nil {
			stamp = meta.Get([]byte(providerStoreKeySchema))
		}
		switch {
		case stamp == nil:
			report.Schema = "unstamped"
			report.Store = stateUnchecked
		default:
			var s providerStoreSchema
			if json.Unmarshal(stamp, &s) == nil {
				report.SchemaVersion = s.Version
			}
			if verr := providerStoreStampVerdict(stamp, ""); verr != nil {
				report.Schema = "newer"
				report.Store = stateNewer
				return nil
			}
			report.Schema = "supported"
			report.Store = stateClean
		}

		// records: every raw record must decode
		if b := tx.Bucket([]byte(providerMigrationRecordsBucket)); b != nil {
			c := b.Cursor()
			for k, _ := c.First(); k != nil; k, _ = c.Next() {
				report.Records++
				if _, _, rerr := readMigrationRecord(tx, string(k)); rerr != nil {
					report.Undecodable++
				}
			}
		}

		// families: the known one always appears, recorded ones are added
		names := map[string]bool{stateIdentityFamily: true}
		if b := tx.Bucket([]byte(providerMigrationBucket)); b != nil {
			c := b.Cursor()
			for k, _ := c.First(); k != nil; k, _ = c.Next() {
				names[safeFamilyName(string(k))] = true
			}
		}
		keys := make([]string, 0, len(names))
		for name := range names {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			report.Families = append(report.Families, inspectFamily(tx, name, fileSide))
		}
		return nil
	})
	if err != nil {
		// The read itself failed: nothing above can be trusted as clean.
		report.Store = classifyStoreError(err)
		if report.Store == stateClean {
			report.Store = stateUnavailable
		}
		report.Schema = string(stateUnchecked)
		report.Families = nil
	}
	report.Status = report.Store
	if report.Undecodable > 0 {
		report.Status = worseState(report.Status, stateCorrupt)
	}
	if err == nil && report.Store == stateNewer {
		// An unreadable layout makes the family records meaningless.
		report.Families = nil
	}
	for _, f := range report.Families {
		report.Status = worseState(report.Status, f.Status)
	}
	return report
}

func inspectFamily(tx *bbolt.Tx, name string, fileSide func() migrationSide) stateFamilyReport {
	out := stateFamilyReport{Name: name, Mode: string(migrationUninitialized), Seal: "none", Decodable: "yes", Reconcile: string(stateUnchecked), Status: stateUnchecked}
	family, found, err := readMigrationFamily(tx, name)
	if err != nil {
		out.Mode, out.Seal, out.Decodable = "unknown", "unreadable", "no"
		out.Reconcile, out.Status = string(stateUnchecked), stateCorrupt
		return out
	}
	if !found {
		out.Decodable = "absent"
		return out
	}
	out.Mode = string(family.Mode)
	pending := false
	if family.Operation != nil {
		out.Seal = string(family.Operation.State)
		out.Generation = family.Operation.Generation
		pending = family.Operation.State == migrationPending
	}
	if family.Mode == migrationUninitialized || name != stateIdentityFamily {
		// Nothing was copied, or this build has no source to compare against.
		return out
	}
	store := migrationSide{}
	rec, present, rerr := readMigrationRecord(tx, name)
	switch {
	case rerr != nil:
		store.Err = rerr
		out.Decodable = "no"
	default:
		store.Present, store.Record = present, rec
	}
	cmp := compareMigrationRecords(fileSide(), store, pending)
	out.Reconcile = string(cmp)
	out.Status = comparisonStatus(cmp)
	if out.Decodable == "no" {
		out.Status = worseState(out.Status, stateCorrupt)
	}
	return out
}

// buildStateIdentitiesReport lists slots from the identity event bucket, newest
// outcome per slot. Slots are redacted as each event is read.
func buildStateIdentitiesReport(store *providerStore, storeErr error) stateIdentitiesReport {
	report := newStateIdentitiesReport()
	if store == nil {
		if storeErr == nil {
			storeErr = errStateStoreNotOpen
		}
		report.Status = classifyStoreError(storeErr)
		return report
	}
	type agg struct {
		outcome string
		events  int
	}
	slots := map[redactedSlot]*agg{}
	err := inspectView(store, func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(providerStoreBucketIdentityEvents))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var event providerIdentityEvent
			if json.Unmarshal(v, &event) != nil || event.Outcome == "" {
				report.Undecodable++
				continue
			}
			report.Events++
			id := redactSlot(event.Slot)
			a := slots[id]
			if a == nil {
				a = &agg{}
				slots[id] = a
			}
			a.events++
			a.outcome = event.Outcome
		}
		return nil
	})
	if err != nil {
		report.Status = classifyStoreError(err)
		if report.Status == stateClean {
			report.Status = stateUnavailable
		}
		report.Slots, report.Events, report.Undecodable = nil, 0, 0
		return report
	}
	for id, a := range slots {
		report.Slots = append(report.Slots, stateIdentitySlot{Slot: id, Outcome: a.outcome, Events: a.events})
	}
	sort.Slice(report.Slots, func(i, j int) bool { return report.Slots[i].Slot < report.Slots[j].Slot })
	// clean here means every record decoded, not that anything was reconciled.
	report.Status = stateClean
	if report.Undecodable > 0 {
		report.Status = stateCorrupt
	}
	return report
}

// stateInspectControl serves the "state" control request. The reply is one
// JSON value; the CLI renders it.
func stateInspectControl(action string) (string, error) {
	store, storeErr := ownerStoreForInspection()
	var v any
	switch strings.TrimSpace(action) {
	case "verify":
		v = buildStateVerifyReport(store, storeErr, stateIdentityFileSide)
	case "identities":
		v = buildStateIdentitiesReport(store, storeErr)
	default:
		return "", errors.New("state: unknown action (want verify or identities)")
	}
	b, err := json.Marshal(v)
	return string(b), err
}
