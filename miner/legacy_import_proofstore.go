// The durable record of the 3.23-fix import's successful proofs.
//
// Proving a saved identity costs a live call per slot, and the work was thrown
// away whenever the start ended before the credentials were committed (a
// canceled or killed start, a failed marker or credential write). The store
// keeps each proof the moment it resolves, so the next start adopts the slot
// from disk and spends no call on it.
//
// A stored proof is a cache of a verdict, never the verdict itself. It is only
// consulted for the exact claim it was made about (client, network, retained
// key, saved token) and only while it is young. Validation is lazy: nothing
// re-checks a proof eagerly. When the platform rejects an adopted identity at
// the first real use, noteLegacyImportProofRejected drops the proof and the
// slot is proven live again, never trusted from the cache a second time.
//
// The file is keyed by the slot's credential file name (a hash of the slot
// identity, the same name the journal records as the target), so it carries no
// proxy address, user or token: only fingerprints of them.
package miner

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/internal/durablesync"
)

const (
	legacyImportProofStoreName   = ".provider.import-proofs.json"
	legacyImportProofStoreSchema = "urnetwork-legacy-import-proofs-v1"
	// legacyImportStoredProof is the journal proof name of a slot adopted from
	// the store without a live call.
	legacyImportStoredProof = "stored"
)

const (
	// legacyImportProofMaxAge bounds how long a stored proof is trusted. The
	// server may revoke a client at any time, so an old proof must not stand
	// forever; lazy validation catches a revocation sooner than this does.
	legacyImportProofMaxAge = 14 * 24 * time.Hour
	// legacyImportProofClockSkew is how far in the future a proof's timestamp
	// may sit before the record is treated as malformed.
	legacyImportProofClockSkew = 5 * time.Minute
)

// legacyImportProofRecord is one slot's stored proof. ProvenAt == 0 marks a
// rejected proof: the record is kept for the failure it carries but is never
// usable.
type legacyImportProofRecord struct {
	ClientID  string `json:"client_id"`
	NetworkID string `json:"network_id"`
	KeyFP     string `json:"key_fp"`
	TokenFP   string `json:"token_fp"`
	Proof     string `json:"proof"`
	ProvenAt  int64  `json:"proven_at"`
	// The failure the stored proof met when it was used.
	Rejections      int    `json:"rejections,omitempty"`
	LastRejection   string `json:"last_rejection,omitempty"`
	LastRejectionAt int64  `json:"last_rejection_at,omitempty"`
}

type legacyImportProofFile struct {
	Schema string                             `json:"schema"`
	Slots  map[string]legacyImportProofRecord `json:"slots"`
}

// legacyImportProofClaim is what a proof is a proof of. A record is usable only
// when every field matches the slot as it is now.
type legacyImportProofClaim struct {
	ClientID  string
	NetworkID string
	KeyFP     string
	TokenFP   string
}

func legacyImportFingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// legacyImportProofClaimFor is the claim a live proof of slot would establish.
func legacyImportProofClaimFor(slot *legacyImportSlot, public ed25519.PublicKey) legacyImportProofClaim {
	return legacyImportProofClaim{
		ClientID:  slot.clientID.String(),
		NetworkID: slot.entry.NetworkID,
		KeyFP:     legacyImportFingerprint(public),
		TokenFP:   legacyImportFingerprint([]byte(slot.entry.ByClientJWT)),
	}
}

// legacyImportProofKey names a slot in the store: its credential file name.
func legacyImportProofKey(slot *legacyImportSlot) string {
	return filepath.Base(slot.target)
}

// wellFormed rejects a record that cannot be a proof this code wrote. Nothing
// malformed is ever trusted; it is dropped and proven live.
func (self legacyImportProofRecord) wellFormed() bool {
	if id, err := connect.ParseId(self.ClientID); err != nil || id.String() != self.ClientID {
		return false
	}
	if id, err := connect.ParseId(self.NetworkID); err != nil || id.String() != self.NetworkID {
		return false
	}
	if !legacyImportHexSHA256(self.KeyFP) || !legacyImportHexSHA256(self.TokenFP) {
		return false
	}
	switch self.Proof {
	case "server_key", "refresh":
	default:
		return false
	}
	return self.ProvenAt >= 0 && self.Rejections >= 0
}

func legacyImportHexSHA256(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == sha256.Size && hex.EncodeToString(raw) == value
}

// legacyImportProofStore is one start's view of the file. Reads and records go
// through the in-memory map; flush publishes the dirty keys atomically.
type legacyImportProofStore struct {
	path string
	now  func() time.Time

	mu       sync.Mutex
	slots    map[string]legacyImportProofRecord
	dirty    map[string]bool
	reproved map[string]bool
	// corrupt names why the file could not be used (empty when it was read or
	// absent); dropped counts records discarded as malformed.
	corrupt string
	dropped int
}

// loadLegacyImportProofStore never fails: a missing file is an empty store, and
// an unreadable, unparseable or older-format file is an empty store that says
// so, which is exactly today's behaviour (every slot proves live).
func loadLegacyImportProofStore(stateDir string) *legacyImportProofStore {
	store := &legacyImportProofStore{
		path:     filepath.Join(stateDir, legacyImportProofStoreName),
		now:      time.Now,
		slots:    map[string]legacyImportProofRecord{},
		dirty:    map[string]bool{},
		reproved: map[string]bool{},
	}
	slots, dropped, corrupt := readLegacyImportProofFile(store.path)
	store.slots = slots
	store.dropped = dropped
	store.corrupt = corrupt
	return store
}

// readLegacyImportProofFile returns the well-formed records of the file at path.
func readLegacyImportProofFile(path string) (map[string]legacyImportProofRecord, int, string) {
	empty := map[string]legacyImportProofRecord{}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, 0, ""
	}
	if err != nil {
		return empty, 0, "unreadable"
	}
	var file legacyImportProofFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return empty, 0, "malformed"
	}
	if file.Schema != legacyImportProofStoreSchema || file.Slots == nil {
		return empty, 0, "unknown_schema"
	}
	dropped := 0
	for key, record := range file.Slots {
		if key == "" || !record.wellFormed() {
			delete(file.Slots, key)
			dropped++
		}
	}
	return file.Slots, dropped, ""
}

// usable reports whether key holds a proof of exactly claim that is still
// young. A proof dated in the future, rejected, or made about anything else
// is not usable.
func (self *legacyImportProofStore) usable(key string, claim legacyImportProofClaim) bool {
	if self == nil {
		return false
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	record, ok := self.slots[key]
	if !ok || record.ProvenAt <= 0 {
		return false
	}
	if record.ClientID != claim.ClientID || record.NetworkID != claim.NetworkID ||
		record.KeyFP != claim.KeyFP || record.TokenFP != claim.TokenFP {
		return false
	}
	age := self.now().Sub(time.Unix(record.ProvenAt, 0))
	return age >= -legacyImportProofClockSkew && age <= legacyImportProofMaxAge
}

// rejected reports whether key's stored proof was rejected when it was used.
func (self *legacyImportProofStore) rejected(key string) bool {
	if self == nil {
		return false
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	record, ok := self.slots[key]
	return ok && record.ProvenAt == 0 && record.Rejections > 0
}

// record stores a live proof. A fresh proof replaces whatever the key held.
func (self *legacyImportProofStore) record(key string, claim legacyImportProofClaim, proof string) {
	if self == nil {
		return
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	if prev, ok := self.slots[key]; ok && (prev.Rejections > 0 || prev.ProvenAt == 0) {
		if self.reproved == nil {
			self.reproved = map[string]bool{}
		}
		self.reproved[key] = true
	}
	self.slots[key] = legacyImportProofRecord{
		ClientID:  claim.ClientID,
		NetworkID: claim.NetworkID,
		KeyFP:     claim.KeyFP,
		TokenFP:   claim.TokenFP,
		Proof:     proof,
		ProvenAt:  self.now().Unix(),
	}
	self.dirty[key] = true
}

// flush publishes the dirty keys: under the file's cross-process lock it
// re-reads the file, applies this start's changes over it and replaces it
// atomically (temporary file in the same directory, fsync, rename, directory
// fsync). A corrupt file is replaced, not merged.
func (self *legacyImportProofStore) flush() error {
	if self == nil {
		return nil
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	if len(self.dirty) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(self.path), 0o700); err != nil {
		return err
	}
	release, err := acquireFileLock(self.path)
	if err != nil {
		return err
	}
	defer release()
	merged, _, _ := readLegacyImportProofFile(self.path)
	for key := range self.dirty {
		if record, ok := self.slots[key]; ok {
			if existing, onDisk := merged[key]; onDisk && (existing.Rejections > 0 || existing.ProvenAt == 0) {
				// Rejection is sticky and authoritative against stale in-memory records.
				// A proof record carrying a rejection can never be replaced by a proof record
				// without one, unless it was a fresh live proof explicitly executed to reprove
				// after seeing that rejection and no newer rejection has landed on disk.
				if !self.reproved[key] || existing.LastRejectionAt > record.ProvenAt {
					self.slots[key] = existing
					continue
				}
			}
			merged[key] = record
		}
	}
	raw, err := json.Marshal(legacyImportProofFile{Schema: legacyImportProofStoreSchema, Slots: merged})
	if err != nil {
		return err
	}
	if err := writeLegacyImportStateFile(filepath.Dir(self.path), legacyImportProofStoreName, raw); err != nil {
		return err
	}
	self.dirty = map[string]bool{}
	return nil
}

// noteLegacyImportProofRejected is the lazy validation: the platform refused an
// identity at its first real use. If that identity was adopted on a stored
// proof, the proof is withdrawn (it stays on file as a rejection, with the
// reason and time) so the next start proves the slot live. A slot with no
// stored proof is left alone: nothing here vouched for it.
func noteLegacyImportProofRejected(stateDir, key, reason string) error {
	if stateDir == "" || key == "" {
		return nil
	}
	path := filepath.Join(stateDir, legacyImportProofStoreName)
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	release, err := acquireFileLock(path)
	if err != nil {
		return err
	}
	defer release()
	slots, _, _ := readLegacyImportProofFile(path)
	record, ok := slots[key]
	if !ok {
		return nil
	}
	record.ProvenAt = 0
	record.Rejections++
	record.LastRejection = reason
	record.LastRejectionAt = time.Now().Unix()
	slots[key] = record
	raw, err := json.Marshal(legacyImportProofFile{Schema: legacyImportProofStoreSchema, Slots: slots})
	if err != nil {
		return err
	}
	return writeLegacyImportStateFile(stateDir, legacyImportProofStoreName, raw)
}

// writeLegacyImportStateFile replaces stateDir/name atomically with mode 0600:
// a named temporary beside it is fsynced, renamed over the target, and the
// directory is fsynced.
func writeLegacyImportStateFile(stateDir, name string, raw []byte) error {
	path := filepath.Join(stateDir, name)
	tmp, err := os.CreateTemp(stateDir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return durablesync.SyncParent(path)
}
