package miner

// extender_fallback.go is the activation ledger a launch uses while the real
// record in provider_state.db cannot be opened.
//
// There is one budget, not two. The fallback file is a log of the launches and
// refusal holds the real record could not take. The first real write that finds
// it merges the entries it has not merged yet (mergeFallback), attempts and
// refusal holds together. The real record stays the authority; the fallback is
// its overflow while the store is out.
//
// The file also carries a copy of the real record as of its last write (Base).
// An outage therefore starts from what the store already spent, and a restart
// during the outage does not repair that by forgetting it.
//
// Every entry has a sequence number within the file's epoch, and the real record
// remembers the last epoch and sequence it merged, so a merge applies each entry
// once whether the process died before or after the file was dealt with.
//
// Every read-decide-record step on either ledger runs under one cross-process
// file lock, so two processes cannot each believe there is room.
//
// A bookkeeping fault must never be why the extender role stays off, so a
// launch the fallback cannot count still proceeds. That is bounded in memory,
// not by the file the fault may be in: a process lets at most
// extenderUncountedLaunchMax launches through uncounted.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const (
	// extenderLedgerLockTimeout bounds the wait for the cross-process lock. The
	// sections it guards are one small file or one bbolt transaction.
	extenderLedgerLockTimeout = probeOpenTimeout
	// extenderUncountedLaunchMax is how many launches one process may let
	// through when neither ledger could count them.
	extenderUncountedLaunchMax = 2
	// extenderFallbackMaxBytes caps the file read; a larger one is not ours.
	extenderFallbackMaxBytes = 65536
)

// extenderFallbackVersion is the shape of the file written now. A file without
// it is the launch log an earlier build wrote, or something this build does not
// understand; neither is read as empty spending.
const extenderFallbackVersion = 2

// extenderFallbackEntry is one launch's posts, or one refusal hold, in the order
// they were taken.
type extenderFallbackEntry struct {
	Seq           int64   `json:"seq"`
	Account       string  `json:"account"`
	Attempts      []int64 `json:"attempts,omitempty"`
	HoldUntilUnix int64   `json:"hold_until_unix,omitempty"`
}

// extenderFallbackRecord is the fallback file. Epoch names this log and Seq
// orders its entries within it.
type extenderFallbackRecord struct {
	Version int    `json:"version"`
	Epoch   string `json:"epoch"`
	NextSeq int64  `json:"next_seq"`
	// Base is the real record's spending as of the last time it was written,
	// so a fallback decision starts from what the store already holds instead
	// of a blank allowance. Entries are what was spent after it.
	Base    map[string]extenderActivationState `json:"base,omitempty"`
	Entries []extenderFallbackEntry            `json:"entries"`

	// source is how an older or unknown file was read. It is not stored: the
	// next write replaces the file, and keeps the old bytes beside it.
	source string
}

// extenderFallbackLegacyLaunch is the launch log of the first fallback file.
type extenderFallbackLegacyLaunch struct {
	Unix  int64 `json:"unix"`
	Posts int   `json:"posts"`
}

var (
	extenderUncountedMu       sync.Mutex
	extenderUncountedLaunches int
)

func extenderFallbackPath() (string, error) {
	dir, err := providerStateDir()
	if err != nil {
		return "", err
	}
	return dir + ".extender-activation-fallback.json", nil
}

// lockExtenderLedger takes the cross-process lock both ledgers share.
func lockExtenderLedger() (func(), error) {
	path, err := extenderFallbackPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock := flock.New(path + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), extenderLedgerLockTimeout)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("activation ledger lock unavailable: %w", err)
	}
	if !locked {
		return nil, errors.New("activation ledger lock unavailable")
	}
	return func() { _ = lock.Unlock() }, nil
}

// loadExtenderFallback reads the fallback file; the caller holds the ledger
// lock. The file is never read as empty unless it is absent: the launch log of
// an earlier build is converted, and a file this build cannot interpret is
// spending-unknown, counted as the whole budget spent when it was last written
// so a deployment cannot loosen the cap. Any read error is returned.
func loadExtenderFallback(path string) (extenderFallbackRecord, bool, error) {
	data, err := readExtenderFallbackBytes(path)
	if errors.Is(err, os.ErrNotExist) {
		return extenderFallbackRecord{}, false, nil
	}
	if err != nil {
		return extenderFallbackRecord{}, false, err
	}
	var shape struct {
		Version  int                            `json:"version"`
		Launches []extenderFallbackLegacyLaunch `json:"launches"`
	}
	if data != nil && json.Unmarshal(data, &shape) == nil {
		switch {
		case shape.Version == extenderFallbackVersion:
			var record extenderFallbackRecord
			if json.Unmarshal(data, &record) == nil && record.Epoch != "" {
				return record, true, nil
			}
		case shape.Version == 0 && shape.Launches != nil:
			if record, ok := convertLegacyFallback(path, shape.Launches); ok {
				return record, true, nil
			}
		}
	}
	return unknownFallback(path), true, nil
}

// readExtenderFallbackBytes returns nil data for a file too large to be ours.
func readExtenderFallbackBytes(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, extenderFallbackMaxBytes+1))
	_ = f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if len(data) > extenderFallbackMaxBytes {
		return nil, nil
	}
	return data, nil
}

func extenderFallbackModTime(path string) (time.Time, int64) {
	if info, err := os.Stat(path); err == nil {
		return info.ModTime(), info.ModTime().UnixNano()
	}
	return time.Now(), 0
}

// convertLegacyFallback reads the first build's launch log. Its launches carry
// no account, so they are charged to the unidentified key, which the next
// decision adopts onto the account that makes it.
func convertLegacyFallback(path string, launches []extenderFallbackLegacyLaunch) (extenderFallbackRecord, bool) {
	_, nano := extenderFallbackModTime(path)
	record := extenderFallbackRecord{
		Version: extenderFallbackVersion,
		Epoch:   fmt.Sprintf("legacy-%d", nano),
		NextSeq: 1,
		source:  "legacy",
	}
	for _, launch := range launches {
		if launch.Unix <= 0 || launch.Posts <= 0 || extenderActivationBudget < launch.Posts {
			return extenderFallbackRecord{}, false
		}
		attempts := make([]int64, launch.Posts)
		for i := range attempts {
			attempts[i] = launch.Unix
		}
		record.appendEntry(extenderFallbackEntry{Account: extenderActivationUnidentifiedAccount, Attempts: attempts})
	}
	return record, true
}

// unknownFallback stands in for a file whose spending cannot be read: the whole
// budget, spent when the file was last written.
func unknownFallback(path string) extenderFallbackRecord {
	modified, nano := extenderFallbackModTime(path)
	attempts := make([]int64, extenderActivationBudget)
	for i := range attempts {
		attempts[i] = modified.Unix()
	}
	record := extenderFallbackRecord{
		Version: extenderFallbackVersion,
		Epoch:   fmt.Sprintf("unknown-%d", nano),
		NextSeq: 1,
		source:  "unknown",
	}
	record.appendEntry(extenderFallbackEntry{Account: extenderActivationUnidentifiedAccount, Attempts: attempts})
	tlog("⚠️ [extender] activation fallback record not understood; counting its launches as the whole budget spent at %s\n", modified.Format(time.RFC3339))
	return record
}

// appendEntry adds an entry with the next sequence number.
func (record *extenderFallbackRecord) appendEntry(entry extenderFallbackEntry) {
	if record.NextSeq < 1 {
		record.NextSeq = 1
	}
	for _, existing := range record.Entries {
		if record.NextSeq <= existing.Seq {
			record.NextSeq = existing.Seq + 1
		}
	}
	entry.Seq = record.NextSeq
	record.NextSeq++
	record.Entries = append(record.Entries, entry)
}

// compact drops the entries that can no longer change a decision. Sequence
// numbers are not reused, so the real record's place in the log stays valid.
func (record *extenderFallbackRecord) compact(now time.Time) {
	cutoff := now.Add(-extenderActivationWindow).Unix()
	kept := record.Entries[:0]
	for _, entry := range record.Entries {
		live := now.Unix() < entry.HoldUntilUnix
		for _, at := range entry.Attempts {
			live = live || cutoff < at
		}
		if live {
			kept = append(kept, entry)
		}
	}
	record.Entries = kept
	for account, state := range record.Base {
		pruneExtenderAttempts(&state, now, extenderActivationWindow)
		record.Base[account] = state
	}
}

// mirrorExtenderFallback makes the fallback file a copy of the real record just
// committed, with no entries: everything it held has been merged. The next
// outage then starts from what the store spent, and a restart does not lose it.
func mirrorExtenderFallback(path string, previous extenderFallbackRecord, record extenderActivationRecord, now time.Time) error {
	base := make(map[string]extenderActivationState, len(record.Accounts))
	for account, state := range record.Accounts {
		state.Attempts = append([]int64(nil), state.Attempts...)
		base[account] = state
	}
	return writeExtenderFallback(path, extenderFallbackRecord{Base: base, source: previous.source}, now)
}

// effective is the spending the log describes, as a record a decision can run on.
func (record extenderFallbackRecord) effective() extenderActivationRecord {
	out := extenderActivationRecord{Accounts: map[string]extenderActivationState{}}
	for account, state := range record.Base {
		state.Attempts = append([]int64(nil), state.Attempts...)
		out.Accounts[account] = state
	}
	for _, entry := range record.Entries {
		state := out.Accounts[entry.Account]
		state.Attempts = append(state.Attempts, entry.Attempts...)
		if state.BackoffUntilUnix < entry.HoldUntilUnix {
			state.BackoffUntilUnix = entry.HoldUntilUnix
		}
		out.Accounts[entry.Account] = state
	}
	return out
}

func newExtenderFallbackEpoch() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

func writeExtenderFallback(path string, record extenderFallbackRecord, now time.Time) error {
	record.Version = extenderFallbackVersion
	if record.Epoch == "" {
		record.Epoch = newExtenderFallbackEpoch()
	}
	record.compact(now)
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".extender-fallback-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(append(data, '\n')); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if record.source != "" {
		// Keep what was there; the replacement counts it, not the file.
		_ = os.Link(path, fmt.Sprintf("%s.%s.%d", path, record.source, now.Unix()))
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// mergeFallback folds the entries of the fallback log that the record has not
// merged into it. It remembers the epoch and sequence it reached, so a log that
// outlives its merge, whatever the order the process died in, applies nothing
// twice.
func (record *extenderActivationRecord) mergeFallback(fallback extenderFallbackRecord, now time.Time) {
	var after int64
	if record.FallbackEpoch == fallback.Epoch {
		after = record.FallbackSeq
	}
	if record.Accounts == nil {
		record.Accounts = map[string]extenderActivationState{}
	}
	cutoff := now.Add(-extenderActivationWindow).Unix()
	reached := after
	for _, entry := range fallback.Entries {
		if entry.Seq <= after {
			continue
		}
		state := record.Accounts[entry.Account]
		for _, at := range entry.Attempts {
			if cutoff < at {
				state.Attempts = append(state.Attempts, at)
			}
		}
		if now.Unix() < entry.HoldUntilUnix && state.BackoffUntilUnix < entry.HoldUntilUnix {
			state.BackoffUntilUnix = entry.HoldUntilUnix
		}
		record.Accounts[entry.Account] = state
		if reached < entry.Seq {
			reached = entry.Seq
		}
	}
	record.FallbackEpoch, record.FallbackSeq = fallback.Epoch, reached
}

// decideExtenderFallback is the activation decision made on the fallback
// ledger, under the ledger lock. A launch it allows is recorded before it
// returns; an error means it could not be counted.
func decideExtenderFallback(now time.Time, posts int, userId string) (allow bool, wait time.Duration, used int, err error) {
	unlock, err := lockExtenderLedger()
	if err != nil {
		return false, 0, 0, err
	}
	defer unlock()
	path, err := extenderFallbackPath()
	if err != nil {
		return false, 0, 0, err
	}
	fallback, _, err := loadExtenderFallback(path)
	if err != nil {
		return false, 0, 0, err
	}
	account := extenderActivationAccountKey(userId)
	record := fallback.effective()
	allow, wait, used = decideExtenderActivationOn(&record, account, now, posts)
	if !allow {
		return false, wait, used, nil
	}
	attempts := make([]int64, posts)
	for i := range attempts {
		attempts[i] = now.Unix()
	}
	fallback.appendEntry(extenderFallbackEntry{Account: account, Attempts: attempts})
	if err := writeExtenderFallback(path, fallback, now); err != nil {
		return false, 0, 0, err
	}
	return true, 0, used, nil
}

// holdExtenderFallback keeps a refusal hold the real record could not take, so
// the launches that follow honour it and the real record inherits it.
func holdExtenderFallback(now time.Time, userId string, wait time.Duration) {
	unlock, err := lockExtenderLedger()
	if err != nil {
		return
	}
	defer unlock()
	path, err := extenderFallbackPath()
	if err != nil {
		return
	}
	fallback, _, err := loadExtenderFallback(path)
	if err != nil {
		return
	}
	// The streak is counted by the retry that reaches the real record; only
	// the hold travels, so it is not counted twice.
	fallback.appendEntry(extenderFallbackEntry{
		Account:       extenderActivationAccountKey(userId),
		HoldUntilUnix: now.Add(wait).Unix(),
	})
	_ = writeExtenderFallback(path, fallback, now)
}

// allowExtenderFallback decides a launch the real record could not. cause is
// why the real record was out. The role stays on when the fallback cannot count
// the launch, up to extenderUncountedLaunchMax per process.
func allowExtenderFallback(now time.Time, posts int, userId string, cause error) bool {
	allow, wait, used, err := decideExtenderFallback(now, posts, userId)
	if err != nil {
		return allowUncountedLaunch(cause, err)
	}
	if !allow {
		tlog("[extender] extender role off: fallback activation budget held: %d of %d posts in the last hour, next in %s\n",
			used, extenderActivationBudget, formatDuration(wait))
		return false
	}
	tlog("[extender] extender role on: activation budget record unreadable (%v); launch counted against fallback\n", cause)
	return true
}

func allowUncountedLaunch(cause, fallbackErr error) bool {
	extenderUncountedMu.Lock()
	defer extenderUncountedMu.Unlock()
	if extenderUncountedLaunchMax <= extenderUncountedLaunches {
		tlog("[extender] extender role off: activation budget could not be recorded (%v; fallback: %v) and this process has already launched %d uncounted\n",
			cause, fallbackErr, extenderUncountedLaunches)
		return false
	}
	extenderUncountedLaunches++
	tlog("[extender] extender role on: activation budget record unreadable (%v) and the fallback could not count the launch (%v); uncounted launch %d of %d\n",
		cause, fallbackErr, extenderUncountedLaunches, extenderUncountedLaunchMax)
	return true
}
