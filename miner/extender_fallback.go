package miner

// extender_fallback.go is the activation ledger a launch uses while the real
// record in provider_state.db cannot be opened.
//
// There is one budget, not two. The fallback file holds the same per-account
// state as the real record, and the first real read or write that finds it
// merges it in (mergeFallback), attempts and refusal holds together, then
// removes it. The real record stays the authority; the fallback is its
// overflow while the store is out.
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

// extenderFallbackRecord is the fallback file. Stamp orders its writes, so a
// merge that was committed but whose file could not be removed is not applied
// a second time.
type extenderFallbackRecord struct {
	Stamp    int64                              `json:"stamp"`
	Accounts map[string]extenderActivationState `json:"accounts"`
}

var (
	extenderUncountedMu       sync.Mutex
	extenderUncountedLaunches int

	extenderFallbackStampMu   sync.Mutex
	extenderFallbackLastStamp int64
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
// lock. A file that is not ours to decode is moved aside, since it can never
// be read and would otherwise stop every later launch from being counted. Any
// other read error is returned.
func loadExtenderFallback(path string) (extenderFallbackRecord, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return extenderFallbackRecord{}, false, nil
	}
	if err != nil {
		return extenderFallbackRecord{}, false, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, extenderFallbackMaxBytes+1))
	_ = f.Close()
	if readErr != nil {
		return extenderFallbackRecord{}, false, readErr
	}
	var record extenderFallbackRecord
	if len(data) > extenderFallbackMaxBytes || json.Unmarshal(data, &record) != nil {
		moved := fmt.Sprintf("%s.corrupt.%d", path, time.Now().Unix())
		if err := os.Rename(path, moved); err != nil {
			return extenderFallbackRecord{}, false, err
		}
		tlog("⚠️ [extender] activation fallback record unreadable; moved to %s\n", moved)
		return extenderFallbackRecord{}, false, nil
	}
	return record, true, nil
}

// nextExtenderFallbackStamp is the wall clock, never repeated within a process
// and always past the stamp the file being rewritten carries. It is not the
// decision's time: a fallback file recreated after a merge must still outrank
// the stamp the real record remembers.
func nextExtenderFallbackStamp(previous int64) int64 {
	extenderFallbackStampMu.Lock()
	defer extenderFallbackStampMu.Unlock()
	stamp := time.Now().UnixNano()
	for _, floor := range []int64{previous, extenderFallbackLastStamp} {
		if stamp <= floor {
			stamp = floor + 1
		}
	}
	extenderFallbackLastStamp = stamp
	return stamp
}

func writeExtenderFallback(path string, record extenderFallbackRecord) error {
	record.Stamp = nextExtenderFallbackStamp(record.Stamp)
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
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// mergeFallback folds the fallback ledger into the real record and reports
// whether it applied. The stamp makes it idempotent.
func (record *extenderActivationRecord) mergeFallback(fallback extenderFallbackRecord, now time.Time) bool {
	if fallback.Stamp <= record.FallbackStamp {
		return false
	}
	if record.Accounts == nil {
		record.Accounts = map[string]extenderActivationState{}
	}
	for account, pending := range fallback.Accounts {
		record.Accounts[account] = mergeExtenderActivationState(record.Accounts[account], pending, now)
	}
	record.FallbackStamp = fallback.Stamp
	return true
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
	record := extenderActivationRecord{Accounts: fallback.Accounts}
	if record.Accounts == nil {
		record.Accounts = map[string]extenderActivationState{}
	}
	allow, wait, used = decideExtenderActivationOn(&record, account, now, posts)
	if !allow {
		return false, wait, used, nil
	}
	fallback.Accounts = record.Accounts
	if err := writeExtenderFallback(path, fallback); err != nil {
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
	if fallback.Accounts == nil {
		fallback.Accounts = map[string]extenderActivationState{}
	}
	account := extenderActivationAccountKey(userId)
	state := fallback.Accounts[account]
	// The streak is counted by the retry that reaches the real record; only
	// the hold travels, so it is not counted twice.
	if until := now.Add(wait).Unix(); state.BackoffUntilUnix < until {
		state.BackoffUntilUnix = until
	}
	fallback.Accounts[account] = state
	_ = writeExtenderFallback(path, fallback)
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
