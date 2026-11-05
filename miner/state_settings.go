package miner

// state_settings.go moves the miner-private settings files into provider_state.db
// (STATE-MIGRATION-SPEC section 6, Stage 2) and is the harness later stages
// reuse: idempotent, resumable, atomic per record, and counting what it could
// not move instead of dropping it.
//
// Moved: error_counts.json, .metrics-counters.json, direct_grade.json. Each is
// read and written only by the provider process, so the one-writer rule of the
// store (section 3, Rule 1) is kept. Not moved, because urnet-tools reads or
// writes them while the provider runs and bbolt would turn that into a lock
// fight: proxy_trim, .hotswap_declines.json, enforcement-daily.json.
//
// A value is stored as the exact bytes the file held, under bucket "settings",
// key = the setting name. No re-encoding means the migration is a copy, a
// comparison is a byte comparison, and a later export can write the file back.
//
// Crash atomicity of one record: the import is one bbolt Update (all of it or
// none of it), and the file is removed only after that commit returned. A kill
// before the commit leaves the file and no key. A kill after it leaves both,
// with equal bytes, and the next start sees "equal" and finishes the removal.
// There is no state in which the value exists in neither place.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"go.etcd.io/bbolt"
)

const providerStoreBucketSettings = "settings"

// settingMigrateSkippedCategory counts a file the migration left in place
// because it could not be read, did not validate, or could not be imported.
// The setting then keeps running from its file, exactly as before.
const settingMigrateSkippedCategory = "state_migrate_skipped"

// settingMigrateConflictCategory counts a conflicting file preserved aside.
// The stored value wins without discarding the legacy data.
const settingMigrateConflictCategory = "state_migrate_conflict"

// settingMigrateRemoveFailedCategory counts an imported file that could not be
// removed. The next start finds it equal to the stored value and tries again.
const settingMigrateRemoveFailedCategory = "state_migrate_remove_failed"

// settingMaxBytes bounds one setting file read for import. The largest real one
// is the counters file at a few hundred KB.
const settingMaxBytes = 8 << 20

type providerSetting struct {
	key  string
	file string
	// valid reports why a file's bytes cannot be moved, or nil.
	valid func(data []byte) error
}

const (
	settingErrorCounts    = "error_counts"
	settingMetricsCounter = "metrics_counters"
	settingDirectGrade    = "direct_grade"
)

var providerSettings = []providerSetting{
	{key: settingErrorCounts, file: "error_counts.json", valid: validErrorCountsSetting},
	{key: settingMetricsCounter, file: metricsCounterStoreName, valid: validMetricsCountersSetting},
	{key: settingDirectGrade, file: "direct_grade.json", valid: validDirectGradeSetting},
}

// providerSettingKeyForFile is the setting a state file name belongs to, or "".
func providerSettingKeyForFile(file string) string {
	for _, setting := range providerSettings {
		if setting.file == file {
			return setting.key
		}
	}
	return ""
}

func validErrorCountsSetting(data []byte) error {
	var counts map[string]uint64
	if !decodeChecksummedJSON(data, &counts) {
		return errors.New("checksum envelope does not verify")
	}
	return nil
}

func validMetricsCountersSetting(data []byte) error {
	var file minerMetricsCountersFile
	if err := json.Unmarshal(data, &file); err != nil {
		return err
	}
	if file.Version < 1 {
		return fmt.Errorf("unknown version %d", file.Version)
	}
	if file.Version > metricsCounterStoreVersion {
		// A newer build's file is not this build's to rewrite or to move.
		return errMetricsCountersNewer
	}
	return nil
}

func validDirectGradeSetting(data []byte) error {
	var grade directGrade
	return json.Unmarshal(data, &grade)
}

// providerSettingsPlatformSupported reports whether the files are moved here.
// Windows cannot make the new database's name crash-durable (durablesync), and
// the migration deletes the original after the import; moving there would
// trade a file that survives a power cut for a database entry that may not.
// The settings stay files on Windows. The schema stamp and its guard still
// apply there.
var providerSettingsPlatformSupported = func() bool { return runtime.GOOS != "windows" }

// armedSettings holds the setting keys served from the store. A key is armed
// after its file has been imported (or found absent, or found equal, or beaten
// by the store), and stays off for a file the migration had to skip: that one
// keeps running from its file, which its writer then replaces with a good one
// for the next start to import. Arming per key keeps one setting from ever
// being read from one place and written to the other. Nothing is armed on a
// hotswap candidate before TAKEOVER, because the parent still holds the file.
var armedSettings sync.Map

func settingArmed(key string) bool {
	armedDir, ok := armedSettings.Load(key)
	if !ok {
		return false
	}
	dir, err := providerStateDir()
	return err == nil && armedDir == dir
}

func armSetting(key string) {
	if dir, err := providerStateDir(); err == nil {
		armedSettings.Store(key, dir)
	}
}

// resetProviderSettings forgets the armed state and the once-per-process latch.
// A moved state directory or a test starts from the files again.
func resetProviderSettings() {
	armedSettings.Range(func(key, _ any) bool { armedSettings.Delete(key); return true })
	startProviderSettingsMu.Lock()
	startProviderSettingsDone = false
	startProviderSettingsDir = ""
	startProviderSettingsMu.Unlock()
}

// stateMigrateHook runs at named points of the migration so a test can end the
// run where a kill would. Nil in production.
var stateMigrateHook func(point, key string)

type settingOutcome string

const (
	settingAbsent   settingOutcome = "absent"
	settingImported settingOutcome = "imported"
	settingEqual    settingOutcome = "equal"
	settingConflict settingOutcome = "conflict"
	settingSkipped  settingOutcome = "skipped"
)

// settingsMigrationReport is what one run did, per setting.
type settingsMigrationReport struct {
	Outcomes map[string]settingOutcome
}

// startProviderSettings is the open-time step: it opens the store (which runs
// the schema guard), migrates the settings files, and arms each setting whose
// home is now the store. It is idempotent per process. The only error it
// returns is the guard's refusal, which the caller turns into a startup
// refusal; every other failure leaves the settings on their files and is
// counted.
func startProviderSettings() error {
	startProviderSettingsMu.Lock()
	defer startProviderSettingsMu.Unlock()
	dir, dirErr := providerStateDir()
	if dirErr != nil {
		return dirErr
	}
	if startProviderSettingsDone && startProviderSettingsDir == dir {
		return nil
	}
	startProviderSettingsDone = false
	store, err := openProviderStoreForProcess()
	if err != nil {
		if errors.Is(err, errProviderStoreNewer) {
			return err
		}
		tlog("[state] settings stay on their files, the store is not open: %v\n", err)
		return nil
	}
	if !providerSettingsPlatformSupported() {
		return nil
	}
	// Load error-count recovery before removing the primary file, so a
	// retained backup is not mistaken for a missing primary after import.
	_ = persistentErrorStore()
	startProviderSettingsDone = true
	startProviderSettingsDir = dir
	report := migrateProviderSettings(store)
	complete := true
	for _, outcome := range report.Outcomes {
		if outcome == settingSkipped {
			complete = false
		}
	}
	if complete {
		if err := markProviderStoreStage(store, "settings", "db"); err != nil {
			tlog("[state] settings stage not recorded: %v\n", err)
		}
	}
	for _, setting := range providerSettings {
		if settingArmed(setting.key) || report.Outcomes[setting.key] == settingSkipped {
			continue
		}
		if setting.key == settingErrorCounts {
			if err := armPersistentErrorsOnStore(store); err != nil {
				tlog("[state] error counts stay on their file: %v\n", err)
				IncrPersistentError(settingMigrateSkippedCategory)
			}
			continue
		}
		armSetting(setting.key)
	}
	return nil
}

var (
	startProviderSettingsMu   sync.Mutex
	startProviderSettingsDone bool
	startProviderSettingsDir  string
)

// migrateProviderSettings imports each setting's file. One setting never
// blocks another; the report says what happened to each.
func migrateProviderSettings(store *providerStore) settingsMigrationReport {
	report := settingsMigrationReport{Outcomes: map[string]settingOutcome{}}
	for _, setting := range providerSettings {
		report.Outcomes[setting.key] = migrateProviderSetting(store, setting)
	}
	return report
}

func migrateProviderSetting(store *providerStore, setting providerSetting) settingOutcome {
	path, err := providerStatePath(setting.file)
	if err != nil {
		return settingAbsent
	}
	skip := func(format string, args ...any) settingOutcome {
		reason := fmt.Sprintf(format, args...)
		IncrPersistentError(settingMigrateSkippedCategory)
		critLog("STATE MIGRATE skipped %s (%s): %s; it stays on its file", setting.key, setting.file, reason)
		return settingSkipped
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Nothing to import. The key may still exist from an earlier run.
		return settingAbsent
	}
	if err != nil {
		return skip("stat: %v", err)
	}
	if !info.Mode().IsRegular() {
		return skip("not a regular file (%s)", info.Mode().Type())
	}
	if info.Size() > settingMaxBytes {
		return skip("%d bytes exceeds the %d byte import bound", info.Size(), settingMaxBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return skip("read: %v", err)
	}
	if len(data) == 0 {
		return skip("empty")
	}
	if err := setting.valid(data); err != nil {
		return skip("%v", err)
	}
	if stateMigrateHook != nil {
		stateMigrateHook("before-commit", setting.key)
	}
	outcome, err := store.importSetting(setting.key, data)
	if err != nil {
		return skip("import: %v", err)
	}
	if stateMigrateHook != nil {
		stateMigrateHook("after-commit", setting.key)
	}
	if outcome == settingConflict {
		backup, err := preserveSettingConflict(path, info)
		if err != nil {
			critLog("STATE MIGRATE conflict %s: could not preserve %s: %v; the store wins and the file remains", setting.key, setting.file, err)
			return settingConflict
		}
		IncrPersistentError(settingMigrateConflictCategory)
		critLog("STATE MIGRATE conflict %s: the store wins; the legacy file was preserved as %s", setting.key, backup)
		return settingConflict
	}
	// The primary file is redundant once the database commit succeeds. Keep
	// backups and temporary files: they are not part of this migration.
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		IncrPersistentError(settingMigrateRemoveFailedCategory)
		tlog("[state] imported %s but could not remove %s: %v\n", setting.key, path, err)
		return outcome
	}
	_ = providerStoreSyncDir(filepath.Dir(path))
	return outcome
}

// Preserve up to eight conflicts without ever replacing or deleting a backup.
// A full archive leaves the original in place for the operator to resolve.
// A linked backup found on retry completes an interrupted link/unlink safely.
func preserveSettingConflict(path string, original os.FileInfo) (string, error) {
	for i := 1; i <= 8; i++ {
		backup := fmt.Sprintf("%s.conflict.%d", path, i)
		err := os.Link(path, backup)
		if errors.Is(err, fs.ErrExist) {
			info, statErr := os.Lstat(backup)
			if statErr != nil || !os.SameFile(original, info) {
				continue
			}
		} else if err != nil {
			return "", err
		}
		if err := providerStoreSyncDir(filepath.Dir(path)); err != nil {
			return "", err
		}
		if err := os.Remove(path); err != nil {
			return "", err
		}
		return backup, providerStoreSyncDir(filepath.Dir(path))
	}
	return "", errors.New("conflict archive is full")
}

// importSetting stores data under key unless the key exists. One transaction,
// so the value is stored whole or not at all.
func (s *providerStore) importSetting(key string, data []byte) (settingOutcome, error) {
	if s == nil {
		return "", errProviderStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		if s.released {
			return "", errProviderStoreReleased
		}
		return "", errProviderStoreClosed
	}
	outcome := settingImported
	err := s.write(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(providerStoreBucketSettings))
		if err != nil {
			return err
		}
		if existing := b.Get([]byte(key)); existing != nil {
			if bytes.Equal(existing, data) {
				outcome = settingEqual
			} else {
				outcome = settingConflict
			}
			return nil
		}
		outcome = settingImported
		return b.Put([]byte(key), append([]byte(nil), data...))
	})
	return outcome, err
}

// getSetting returns the stored bytes of a setting. found is false for a key
// never written.
func (s *providerStore) getSetting(key string) ([]byte, bool, error) {
	if s == nil {
		return nil, false, errProviderStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		if s.released {
			return nil, false, errProviderStoreReleased
		}
		return nil, false, errProviderStoreClosed
	}
	var raw []byte
	err := runProviderStoreView(s.db, func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(providerStoreBucketSettings))
		if b == nil {
			return nil
		}
		if val := b.Get([]byte(key)); val != nil {
			raw = append([]byte(nil), val...)
		}
		return nil
	})
	return raw, raw != nil, err
}

// putSetting replaces the stored bytes of a setting in one transaction.
func (s *providerStore) putSetting(key string, data []byte) error {
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
	return s.write(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(providerStoreBucketSettings))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), append([]byte(nil), data...))
	})
}

// readSetting is the armed read. It returns fs.ErrNotExist for a key never
// written, so a caller written against os.ReadFile keeps its absent branch.
func readSetting(key string) ([]byte, error) {
	store, err := openProviderStoreForProcess()
	if err != nil {
		return nil, err
	}
	data, found, err := store.getSetting(key)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

// writeSetting is the armed write.
func writeSetting(key string, data []byte) error {
	store, err := openProviderStoreForProcess()
	if err != nil {
		return err
	}
	return store.putSetting(key, data)
}

// flushSettingsBeforeRelease writes the settings the store holds while this
// process still can. Once the file is released to a hotswap candidate a write
// reports errProviderStoreReleased, and the successor's counters would restart
// from the last periodic flush. What the parent counts during its drain, after
// this point, is not carried over (see the hotswap note in the report).
func flushSettingsBeforeRelease() {
	FlushPersistentErrors()
	flushMetricsCounters()
}
