package miner

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// settingsHarness points the process at a fresh state directory with nothing
// armed, so each test starts from the files and from no open store.
type settingsHarness struct {
	t   *testing.T
	dir string
}

func newSettingsHarness(t *testing.T) *settingsHarness {
	t.Helper()
	dir := withTempHome(t)
	closeProviderStore()
	resetProviderSettings()
	resetPersistentErrorStore(t)
	t.Cleanup(func() {
		closeProviderStore()
		resetProviderSettings()
		stateMigrateHook = nil
	})
	return &settingsHarness{t: t, dir: dir}
}

func (h *settingsHarness) path(name string) string { return filepath.Join(h.dir, name) }

// restart is what a new process sees: no open handle, nothing armed, the
// once-per-process latch clear, the error counts loaded again from wherever
// they live.
func (h *settingsHarness) restart() {
	h.t.Helper()
	closeProviderStore()
	resetProviderSettings()
	persistentErrStore = nil
	persistentErrStoreOnce = sync.Once{}
}

func (h *settingsHarness) writeErrorCounts(counts map[string]uint64) []byte {
	h.t.Helper()
	if err := atomicWriteJSON(h.path("error_counts.json"), counts); err != nil {
		h.t.Fatal(err)
	}
	return h.read("error_counts.json")
}

func (h *settingsHarness) writeMetrics() []byte {
	h.t.Helper()
	file := minerMetricsCountersFile{
		Version: metricsCounterStoreVersion,
		SavedAt: time.Unix(1_700_000_000, 0).UTC(),
		PID:     7,
		Proxies: map[int]minerMetricsProxyCounters{0: {TotalRx: 100, TotalTx: 200, LastSeen: 1_700_000_000}},
		Control: minerMetricsControlCounters{Acked: 3},
	}
	blob, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.path(metricsCounterStoreName), blob, 0o600); err != nil {
		h.t.Fatal(err)
	}
	return blob
}

func (h *settingsHarness) writeGrade() []byte {
	h.t.Helper()
	blob, err := json.Marshal(directGrade{Graded: true, LastGraded: time.Unix(1_700_000_000, 0).UTC()})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.path("direct_grade.json"), blob, 0o600); err != nil {
		h.t.Fatal(err)
	}
	return blob
}

func (h *settingsHarness) read(name string) []byte {
	h.t.Helper()
	data, err := os.ReadFile(h.path(name))
	if err != nil {
		h.t.Fatal(err)
	}
	return data
}

func (h *settingsHarness) exists(name string) bool {
	_, err := os.Lstat(h.path(name))
	return err == nil
}

func (h *settingsHarness) stored(key string) ([]byte, bool) {
	h.t.Helper()
	store := providerStoreInstance()
	if store == nil {
		h.t.Fatal("the provider store did not open")
	}
	data, found, err := store.getSetting(key)
	if err != nil {
		h.t.Fatal(err)
	}
	return data, found
}

func (h *settingsHarness) stamp() providerStoreSchema {
	h.t.Helper()
	var stamp providerStoreSchema
	found, err := providerStoreInstance().load(providerStoreBucketMeta, providerStoreKeySchema, &stamp)
	if err != nil || !found {
		h.t.Fatalf("schema stamp: found=%v err=%v", found, err)
	}
	return stamp
}

func (h *settingsHarness) errCount(category string) uint64 {
	return persistentErrorStore().Snapshot()[category]
}

// Stage 2, empty directory: the store opens and is stamped, no setting key is
// written, and no file is created beside the database.
func TestStateMigrationOnAnEmptyDirectory(t *testing.T) {
	h := newSettingsHarness(t)
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	stamp := h.stamp()
	if stamp.Version != providerStoreSchemaVersion || stamp.MinReader != providerStoreSchemaVersion {
		t.Fatalf("stamp = %+v", stamp)
	}
	if stamp.Stages["settings"] != "db" {
		t.Fatalf("settings stage not recorded: %+v", stamp)
	}
	for _, setting := range providerSettings {
		if _, found := h.stored(setting.key); found {
			t.Errorf("%s was written on an empty directory", setting.key)
		}
		if !settingArmed(setting.key) {
			t.Errorf("%s is not served from the store", setting.key)
		}
	}
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != providerStoreFileName {
			t.Errorf("unexpected file %s on an empty directory", entry.Name())
		}
	}
}

// Stage 2, populated directory: every setting is stored as the exact bytes the
// file held, the primary files are gone, and the readers see
// the same values from the store.
func TestStateMigrationOnAPopulatedDirectory(t *testing.T) {
	h := newSettingsHarness(t)
	errorsFile := h.writeErrorCounts(map[string]uint64{"auth_refused": 4})
	// An earlier backup is outside this migration and must survive.
	if err := os.WriteFile(h.path("error_counts.json.bak"), errorsFile, 0o600); err != nil {
		t.Fatal(err)
	}
	metrics := h.writeMetrics()
	grade := h.writeGrade()

	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string][]byte{
		settingErrorCounts:    errorsFile,
		settingMetricsCounter: metrics,
		settingDirectGrade:    grade,
	} {
		got, found := h.stored(key)
		if !found || !bytes.Equal(got, want) {
			t.Errorf("%s stored %q, want the file's bytes %q", key, got, want)
		}
	}
	for _, name := range []string{"error_counts.json", metricsCounterStoreName, "direct_grade.json"} {
		if h.exists(name) {
			t.Errorf("%s survived the migration", name)
		}
	}
	if h.exists("error_counts.json.bak") {
		// the recovery copy is stale once the store owns the setting: restoring
		// it on the next start is the conflict that never settles
		t.Fatal("the stale recovery backup survived the migration")
	}
	if got := persistentErrorStore().Snapshot()["auth_refused"]; got != 4 {
		t.Errorf("error counts through the store = %d, want 4", got)
	}
	if g, ok := readDirectGrade(); !ok || !g.Graded {
		t.Errorf("direct grade through the store = %+v ok=%v", g, ok)
	}
	store := newMetricsCounterStore(h.path(metricsCounterStoreName))
	file, err := store.load()
	if err != nil || file.Control.Acked != 3 || file.Proxies[0].TotalRx != 100 {
		t.Errorf("counters through the store = %+v err=%v", file, err)
	}
}

// A second run over a migrated directory changes nothing.
func TestStateMigrationIsIdempotent(t *testing.T) {
	h := newSettingsHarness(t)
	h.writeMetrics()
	h.writeGrade()
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	first, _ := h.stored(settingMetricsCounter)
	h.restart()
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	second, found := h.stored(settingMetricsCounter)
	if !found || !bytes.Equal(first, second) {
		t.Fatalf("a second run changed the stored counters")
	}
	report := migrateProviderSettings(providerStoreInstance())
	for key, outcome := range report.Outcomes {
		if outcome != settingAbsent {
			t.Errorf("%s: a repeat run reported %s, want absent", key, outcome)
		}
	}
}

// Crash mid-migration: the run ends at every point a kill could land (before a
// record's commit, after it and before the file is removed), across every
// record. The next start finishes: each record is whole in the store with the
// original bytes, no file is left, and nothing is lost in between.
func TestStateMigrationResumesAfterAKillAtEveryPoint(t *testing.T) {
	type killed struct{}
	for _, key := range []string{settingErrorCounts, settingMetricsCounter, settingDirectGrade} {
		for _, point := range []string{"before-commit", "after-commit", "before-remove"} {
			t.Run(point+"/"+key, func(t *testing.T) {
				h := newSettingsHarness(t)
				want := map[string][]byte{
					settingErrorCounts:    h.writeErrorCounts(map[string]uint64{"auth_refused": 9}),
					settingMetricsCounter: h.writeMetrics(),
					settingDirectGrade:    h.writeGrade(),
				}
				stateMigrateHook = func(p, k string) {
					if p == point && k == key {
						panic(killed{})
					}
				}
				func() {
					defer func() {
						if r := recover(); r != nil {
							if _, ok := r.(killed); !ok {
								panic(r)
							}
						}
					}()
					_ = startProviderSettings()
					t.Fatal("the hook did not fire")
				}()

				// What the killed process left behind: every record is either
				// whole in the store or absent from it, and the file is still
				// there unless the store already holds its bytes.
				store := providerStoreInstance()
				for k, bytesWant := range want {
					data, found, err := store.getSetting(k)
					if err != nil {
						t.Fatal(err)
					}
					if found && !bytes.Equal(data, bytesWant) {
						t.Fatalf("%s is half-written: %q", k, data)
					}
					if !found && !h.exists(providerSettingFileFor(t, k)) {
						t.Fatalf("%s is in neither the store nor a file", k)
					}
				}

				stateMigrateHook = nil
				h.restart()
				if err := startProviderSettings(); err != nil {
					t.Fatal(err)
				}
				for k, bytesWant := range want {
					data, found := h.stored(k)
					if !found || !bytes.Equal(data, bytesWant) {
						t.Errorf("%s after the restart = %q found=%v", k, data, found)
					}
					if h.exists(providerSettingFileFor(t, k)) {
						t.Errorf("%s: the file survived the restart", k)
					}
				}
			})
		}
	}
}

func providerSettingFileFor(t *testing.T, key string) string {
	t.Helper()
	for _, setting := range providerSettings {
		if setting.key == key {
			return setting.file
		}
	}
	t.Fatalf("no setting %s", key)
	return ""
}

// A file that does not validate is skipped and counted, left untouched, and
// does not stop the others. Its setting keeps running from the file.
func TestStateMigrationSkipsAndCountsACorruptFile(t *testing.T) {
	h := newSettingsHarness(t)
	garbage := []byte("{not json at all")
	if err := os.WriteFile(h.path(metricsCounterStoreName), garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	grade := h.writeGrade()
	// Counted in memory only: the persistent error store has no file yet.
	before := h.errCount(settingMigrateSkippedCategory)

	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	if got := h.read(metricsCounterStoreName); !bytes.Equal(got, garbage) {
		t.Fatalf("the corrupt file was changed: %q", got)
	}
	if _, found := h.stored(settingMetricsCounter); found {
		t.Fatal("a corrupt file reached the store")
	}
	if settingArmed(settingMetricsCounter) {
		t.Fatal("the skipped setting is served from the store")
	}
	if got := h.errCount(settingMigrateSkippedCategory); got != before+1 {
		t.Fatalf("state_migrate_skipped = %d, want %d", got, before+1)
	}
	if data, found := h.stored(settingDirectGrade); !found || !bytes.Equal(data, grade) {
		t.Fatal("a corrupt neighbour blocked the direct grade")
	}
	if !settingArmed(settingDirectGrade) {
		t.Fatal("the good setting is not served from the store")
	}
}

// A real process dies after one committed record and the next process resumes.
func TestStateMigrationKillProcess(t *testing.T) {
	if os.Getenv("STATE_MIGRATION_KILL_CHILD") == "1" {
		stateMigrateHook = func(point, key string) {
			if point == "after-commit" && key == settingMetricsCounter {
				p, err := os.FindProcess(os.Getpid())
				if err != nil {
					os.Exit(91)
				}
				_ = p.Kill()
				os.Exit(92)
			}
		}
		_ = startProviderSettings()
		os.Exit(93)
	}
	h := newSettingsHarness(t)
	want := map[string][]byte{settingErrorCounts: h.writeErrorCounts(map[string]uint64{"a": 3}), settingMetricsCounter: h.writeMetrics(), settingDirectGrade: h.writeGrade()}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStateMigrationKillProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "STATE_MIGRATION_KILL_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child did not die: %s", out)
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != -1 {
		t.Fatalf("child was not killed: %v: %s", err, out)
	}
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	for key, expected := range want {
		got, found := h.stored(key)
		if !found || !bytes.Equal(got, expected) {
			t.Errorf("%s after kill: found=%v got=%q", key, found, got)
		}
		if h.exists(providerSettingFileFor(t, key)) {
			t.Errorf("%s file survived restart", key)
		}
	}
}

// The other ways a file is not this build's to move.
func TestStateMigrationSkipsWhatItCannotTrust(t *testing.T) {
	newer := fmt.Sprintf(`{"version":%d,"proxies":{}}`, metricsCounterStoreVersion+1)
	cases := map[string]func(h *settingsHarness){
		"symlink": func(h *settingsHarness) {
			target := h.path("elsewhere")
			if err := os.WriteFile(target, []byte(`{"graded":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, h.path("direct_grade.json")); err != nil {
				t.Skip("symlinks unavailable:", err)
			}
		},
		"empty": func(h *settingsHarness) {
			if err := os.WriteFile(h.path("direct_grade.json"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(h *settingsHarness) {
			if err := os.Mkdir(h.path("direct_grade.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"newer version": func(h *settingsHarness) {
			if err := os.WriteFile(h.path("direct_grade.json"), []byte(`{"graded":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(h.path(metricsCounterStoreName), []byte(newer), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newSettingsHarness(t)
			setup(h)
			if err := startProviderSettings(); err != nil {
				t.Fatal(err)
			}
			if name == "newer version" {
				if _, found := h.stored(settingMetricsCounter); found || settingArmed(settingMetricsCounter) {
					t.Fatal("a newer build's counters were moved")
				}
				if got := h.read(metricsCounterStoreName); string(got) != newer {
					t.Fatalf("a newer build's counters were rewritten: %q", got)
				}
				return
			}
			if _, found := h.stored(settingDirectGrade); found || settingArmed(settingDirectGrade) {
				t.Fatalf("%s reached the store", name)
			}
			if _, err := os.Lstat(h.path("direct_grade.json")); err != nil {
				t.Fatalf("%s: the entry was removed: %v", name, err)
			}
		})
	}
}

// Conflicts settle on the first start without losing either version.
func TestStateMigrationConflictSettlesAcrossStarts(t *testing.T) {
	h := newSettingsHarness(t)
	store := providerStoreInstance()
	if err := store.putSetting(settingDirectGrade, []byte(`{"graded":true,"score":90}`)); err != nil {
		t.Fatal(err)
	}
	other := []byte(`{"graded":true,"score":10}`)
	if err := os.WriteFile(h.path("direct_grade.json"), other, 0600); err != nil {
		t.Fatal(err)
	}
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	if h.exists("direct_grade.json") {
		t.Fatal("conflicting file still at legacy path")
	}
	matches, err := filepath.Glob(h.path("direct_grade.json.conflict.*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("conflict copies: %v, %v", matches, err)
	}
	got, err := os.ReadFile(matches[0])
	if err != nil || !bytes.Equal(got, other) {
		t.Fatalf("conflict data not preserved: %v", err)
	}
	count := h.errCount(settingMigrateConflictCategory)
	if count != 1 {
		t.Fatalf("conflict count = %d, want 1", count)
	}
	FlushPersistentErrors()
	h.restart()
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	if got := h.errCount(settingMigrateConflictCategory); got != count {
		t.Fatalf("conflict count after restart = %d, want %d", got, count)
	}
}

// The production shape: a setting writer leaves <file>.bak beside <file>. The
// import removes the primary, so the backup has to go with it — otherwise the
// recovery loader restores stale data over the store on the next start and the
// conflict count climbs forever without a real conflict.
func TestStateMigrationRemovesTheRecoveryBackupWithThePrimary(t *testing.T) {
	h := newSettingsHarness(t)
	value := []byte(`{"graded":true,"score":42}`)
	if err := os.WriteFile(h.path("direct_grade.json"), value, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.path("direct_grade.json.bak"), value, 0600); err != nil {
		t.Fatal(err)
	}
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	if h.exists("direct_grade.json.bak") {
		t.Fatal("recovery backup left behind after a clean import")
	}
	FlushPersistentErrors()
	h.restart()
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	if got := h.errCount(settingMigrateConflictCategory); got != 0 {
		t.Fatalf("conflict after a clean import = %d, want 0", got)
	}
}

// The recovery backup is removed before the primary, so a kill between the
// two removals leaves the primary, which the next start finds equal to the
// store and removes. Removed the other way round, the same kill leaves a lone
// backup that the error-count loader restores and the migration counts as a
// conflict that never happened.
func TestStateMigrationRemovesTheRecoveryBackupBeforeThePrimary(t *testing.T) {
	type killed struct{}
	h := newSettingsHarness(t)
	h.writeErrorCounts(map[string]uint64{"auth_refused": 3})
	h.writeErrorCounts(map[string]uint64{"auth_refused": 4})
	if !h.exists("error_counts.json.bak") {
		t.Fatal("the setting writer left no recovery backup to test with")
	}
	stateMigrateHook = func(point, key string) {
		if point != "before-remove" || key != settingErrorCounts {
			return
		}
		if h.exists("error_counts.json.bak") {
			t.Error("the recovery backup was still there when the primary was removed")
		}
		if !h.exists("error_counts.json") {
			t.Error("the primary was gone before its recovery backup")
		}
		panic(killed{})
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(killed); !ok {
					panic(r)
				}
			}
		}()
		_ = startProviderSettings()
		t.Fatal("the hook did not fire")
	}()
	stateMigrateHook = nil
	h.restart()
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	if got := h.errCount(settingMigrateConflictCategory); got != 0 {
		t.Fatalf("conflicts after a kill between the removals = %d, want 0", got)
	}
	if h.exists("error_counts.json") || h.exists("error_counts.json.bak") {
		t.Fatal("the restart left the legacy file or its backup behind")
	}
	if got := persistentErrorStore().Snapshot()["auth_refused"]; got != 4 {
		t.Fatalf("error counts after the restart = %d, want 4", got)
	}
}

// A file beside a stored value it does not equal: the store wins, the file is
// left alone and counted. An equal file is just removed.
func TestStateMigrationConflictArchiveFullPreservesFiles(t *testing.T) {
	h := newSettingsHarness(t)
	store := providerStoreInstance()
	if err := store.putSetting(settingDirectGrade, []byte(`{"graded":true,"score":90}`)); err != nil {
		t.Fatal(err)
	}
	other := []byte(`{"graded":true,"score":10}`)
	if err := os.WriteFile(h.path("direct_grade.json"), other, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 8; i++ {
		if err := os.WriteFile(h.path(fmt.Sprintf("direct_grade.json.conflict.%d", i)), []byte("retained"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if got := migrateProviderSetting(store, providerSettings[2]); got != settingConflict {
			t.Fatalf("outcome %s", got)
		}
	}
	if got := h.read("direct_grade.json"); !bytes.Equal(got, other) {
		t.Fatal("original changed")
	}
	if got := h.errCount(settingMigrateConflictCategory); got != 0 {
		t.Fatalf("unresolved conflict counted %d times", got)
	}
	for i := 1; i <= 8; i++ {
		if got := h.read(fmt.Sprintf("direct_grade.json.conflict.%d", i)); string(got) != "retained" {
			t.Fatal("backup changed")
		}
	}
}

func TestStateMigrationBothFileAndKeyExist(t *testing.T) {
	h := newSettingsHarness(t)
	store := providerStoreInstance()
	stored := []byte(`{"graded":true,"score":90}`)
	if err := store.putSetting(settingDirectGrade, stored); err != nil {
		t.Fatal(err)
	}

	// Equal: nothing to decide, the file is redundant.
	if err := os.WriteFile(h.path("direct_grade.json"), stored, 0o600); err != nil {
		t.Fatal(err)
	}
	if outcome := migrateProviderSetting(store, providerSettings[2]); outcome != settingEqual {
		t.Fatalf("equal file reported %s", outcome)
	}
	if h.exists("direct_grade.json") {
		t.Fatal("an equal file was kept")
	}

	// Different: no choice by mtime. The store keeps its value.
	other := []byte(`{"graded":true,"score":10}`)
	if err := os.WriteFile(h.path("direct_grade.json"), other, 0o600); err != nil {
		t.Fatal(err)
	}
	before := h.errCount(settingMigrateConflictCategory)
	if outcome := migrateProviderSetting(store, providerSettings[2]); outcome != settingConflict {
		t.Fatalf("differing file reported %s", outcome)
	}
	if h.exists("direct_grade.json") {
		t.Fatal("a conflicting file remained at its legacy path")
	}
	if got := h.read("direct_grade.json.conflict.1"); !bytes.Equal(got, other) {
		t.Fatal("a conflicting file was not preserved")
	}
	if data, _ := h.stored(settingDirectGrade); !bytes.Equal(data, stored) {
		t.Fatal("a conflicting file overwrote the store")
	}
	if got := h.errCount(settingMigrateConflictCategory); got != before+1 {
		t.Fatalf("state_migrate_conflict = %d, want %d", got, before+1)
	}
}

// The store takes the error counts over without losing either side.
func TestStateErrorCountsContinueAcrossTheHandover(t *testing.T) {
	t.Run("history in the store, nothing in a file", func(t *testing.T) {
		h := newSettingsHarness(t)
		blob, err := encodeChecksummedJSON(map[string]uint64{"a": 5})
		if err != nil {
			t.Fatal(err)
		}
		if err := providerStoreInstance().putSetting(settingErrorCounts, blob); err != nil {
			t.Fatal(err)
		}
		IncrPersistentError("a")
		IncrPersistentError("a")
		if err := startProviderSettings(); err != nil {
			t.Fatal(err)
		}
		if got := persistentErrorStore().Snapshot()["a"]; got != 7 {
			t.Fatalf("a = %d, want 7 (stored 5 plus 2 counted)", got)
		}
		persistentErrorStore().Flush()
		h.restart()
		if err := startProviderSettings(); err != nil {
			t.Fatal(err)
		}
		if got := persistentErrorStore().Snapshot()["a"]; got != 7 {
			t.Fatalf("a after a restart = %d, want 7", got)
		}
	})
	t.Run("history in the file", func(t *testing.T) {
		h := newSettingsHarness(t)
		h.writeErrorCounts(map[string]uint64{"a": 5})
		IncrPersistentError("a") // loads the file lazily, then counts once
		if err := startProviderSettings(); err != nil {
			t.Fatal(err)
		}
		if got := persistentErrorStore().Snapshot()["a"]; got != 6 {
			t.Fatalf("a = %d, want 6; the imported 5 must not be counted twice", got)
		}
	})
}

func TestStateErrorCountsRecoverBackupWhenMainFileIsAbsent(t *testing.T) {
	h := newSettingsHarness(t)
	blob, err := encodeChecksummedJSON(map[string]uint64{"a": 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.path("error_counts.json.bak"), blob, 0600); err != nil {
		t.Fatal(err)
	}
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	if got := persistentErrorStore().Snapshot()["a"]; got != 5 {
		t.Fatalf("recovered count = %d, want 5", got)
	}
}

// A store write that fails counts its own failure through the same error
// counters the flush holds. The flush must not hold their lock across the
// write, or the failure count deadlocks the flush.
func TestStateErrorCountFlushDoesNotDeadlockOnAFailedStoreWrite(t *testing.T) {
	h := newSettingsHarness(t)
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	IncrPersistentError("a")
	// A handle whose database is closed under it fails every transaction with
	// an error that is not the release error, which s.write counts.
	if err := providerStoreInstance().db.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		persistentErrorStore().Flush()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the error-count flush deadlocked on its own failure count")
	}
	if got := h.errCount(providerStoreWriteFailedCategory); got == 0 {
		t.Fatal("the failed write was not counted")
	}
	// The counts it could not write are still pending, not dropped.
	persistentErrorStore().mu.Lock()
	dirty := persistentErrorStore().dirty
	persistentErrorStore().mu.Unlock()
	if !dirty {
		t.Fatal("a failed write cleared the pending counts")
	}
}

// The metrics counters are written to the store, not to a file, once it holds
// them, and a second store restores the same totals from it.
func TestStateMetricsCountersLiveInTheStore(t *testing.T) {
	h := newSettingsHarness(t)
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	path := h.path(metricsCounterStoreName)
	writer := newMetricsCounterStore(path)
	writer.snapshot = func() minerMetricsCounterSnapshot {
		return minerMetricsCounterSnapshot{
			Proxies: map[int]minerMetricsProxyCounters{0: {TotalRx: 11, TotalTx: 22}},
			Control: minerMetricsControlCounters{Acked: 5},
		}
	}
	if err := writer.flush(); err != nil {
		t.Fatal(err)
	}
	if h.exists(metricsCounterStoreName) {
		t.Fatal("the counters were written to a file as well")
	}
	reader := newMetricsCounterStore(path)
	var restored minerMetricsControlCounters
	reader.addControl = func(c minerMetricsControlCounters) { restored = c }
	if err := reader.restore(); err != nil {
		t.Fatal(err)
	}
	if restored.Acked != 5 {
		t.Fatalf("restored control counters = %+v", restored)
	}
	if seed, ok := reader.proxySeed(0); !ok || seed.TotalRx != 11 {
		t.Fatalf("restored proxy seed = %+v ok=%v", seed, ok)
	}
}

// Before the parent hands the file over it writes the latest values; after
// that its writes report the loss and never fall back to creating a file.
func TestStateSettingsAreFlushedBeforeTheHotswapRelease(t *testing.T) {
	h := newSettingsHarness(t)
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	IncrPersistentError("late")
	flushSettingsBeforeRelease()
	releaseProviderStore()
	IncrPersistentError("later")
	persistentErrorStore().Flush()
	if h.exists("error_counts.json") {
		t.Fatal("a released parent fell back to writing a file")
	}
	if err := writeSetting(settingDirectGrade, []byte(`{}`)); !errors.Is(err, errProviderStoreReleased) {
		t.Fatalf("write after release = %v, want errProviderStoreReleased", err)
	}
	if err := restoreProviderStoreAfterAbortedHandoff(); err != nil {
		t.Fatal(err)
	}
	raw, found := h.stored(settingErrorCounts)
	var counts map[string]uint64
	if !found || !decodeChecksummedJSON(raw, &counts) {
		t.Fatalf("the store holds no verifiable counts: found=%v", found)
	}
	if counts["late"] != 1 {
		t.Fatalf("the pre-release flush did not land: %+v", counts)
	}
	if counts["later"] != 0 {
		t.Fatalf("a write after the release landed: %+v", counts)
	}
}

// The incident bundle carries the live direct grade although its file is gone.
func TestStateIncidentCaptureReadsTheDirectGradeThroughTheStore(t *testing.T) {
	h := newSettingsHarness(t)
	grade := h.writeGrade()
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	if h.exists("direct_grade.json") {
		t.Fatal("the file survived the migration")
	}
	b, err := newIncidentBundle(t.TempDir(), time.Now(), incidentTriggerPanic, incidentBudget)
	if err != nil {
		t.Fatal(err)
	}
	incidentCopyState(b)
	got, err := os.ReadFile(filepath.Join(b.dir, "state", "direct_grade.json"))
	if err != nil {
		t.Fatalf("the bundle has no direct_grade.json: %v", err)
	}
	if !strings.Contains(string(got), `"graded":true`) {
		t.Fatalf("bundle copy = %q, want the stored grade %q", got, grade)
	}
	inventory, err := os.ReadFile(filepath.Join(b.dir, "state", "inventory.txt"))
	if err != nil || !strings.Contains(string(inventory), "direct_grade.json held in provider_state.db") {
		t.Fatalf("the inventory does not say where the grade is: %v\n%s", err, inventory)
	}
}

// Windows keeps the settings on their files. The gate is a variable so the
// decision is tested here on any platform.
func TestStateSettingsStayOnTheirFilesWhereTheMoveIsGatedOff(t *testing.T) {
	h := newSettingsHarness(t)
	prev := providerSettingsPlatformSupported
	providerSettingsPlatformSupported = func() bool { return false }
	t.Cleanup(func() { providerSettingsPlatformSupported = prev })
	grade := h.writeGrade()
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	if got := h.read("direct_grade.json"); !bytes.Equal(got, grade) {
		t.Fatal("a gated platform touched the file")
	}
	if settingArmed(settingDirectGrade) {
		t.Fatal("a gated platform serves a setting from the store")
	}
	if stamp := h.stamp(); stamp.Version != providerStoreSchemaVersion {
		t.Fatalf("the guard did not stamp the store on a gated platform: %+v", stamp)
	}
}

// The documented bbolt fact the design rests on: a read-only open cannot sit
// beside the provider's read-write handle. It waits out its timeout and fails,
// which is why urnet-tools never opens the file.
func TestStateStoreSecondOpenerTimesOutWhileTheProviderHoldsIt(t *testing.T) {
	h := newSettingsHarness(t)
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	db, err := bbolt.Open(h.path(providerStoreFileName), 0o600, &bbolt.Options{ReadOnly: true, Timeout: 300 * time.Millisecond})
	if err == nil {
		_ = db.Close()
		t.Fatal("a read-only open succeeded beside the provider's read-write handle")
	}
	if !errors.Is(err, bbolt.ErrTimeout) {
		t.Fatalf("second open failed with %v, want bbolt.ErrTimeout", err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatalf("the open gave up after %s instead of waiting out its timeout", time.Since(start))
	}
	// And the provider is unharmed by the attempt.
	if err := providerStoreInstance().putSetting(settingDirectGrade, []byte(`{}`)); err != nil {
		t.Fatalf("the provider's handle broke: %v", err)
	}
}

// A reader that goes through the provider's handle while a writer commits sees
// only whole values.
func TestStateStoreConcurrentReaderSeesOnlyWholeRecords(t *testing.T) {
	newSettingsHarness(t)
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	store := providerStoreInstance()
	value := func(i int) []byte {
		return []byte(fmt.Sprintf(`{"graded":true,"failed":[%q]}`, strings.Repeat("x", 4096+i)))
	}
	if err := store.putSetting(settingDirectGrade, value(0)); err != nil {
		t.Fatal(err)
	}
	const writes = 200
	valid := map[[32]byte]bool{}
	for i := 0; i <= writes; i++ {
		valid[sha256.Sum256(value(i))] = true
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= writes; i++ {
			if err := store.putSetting(settingDirectGrade, value(i)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < writes; i++ {
		data, found, err := store.getSetting(settingDirectGrade)
		if err != nil || !found {
			t.Fatalf("read %d: found=%v err=%v", i, found, err)
		}
		if !valid[sha256.Sum256(data)] {
			t.Fatalf("read %d returned a value no writer wrote (%d bytes)", i, len(data))
		}
	}
	wg.Wait()
}

// rawStore opens the database file directly, as a process that had no guard
// would, to write what a later build would have written.
func writeStampDirect(t *testing.T, path string, stamp string) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(providerStoreBucketMeta))
		if err != nil {
			return err
		}
		return b.Put([]byte(providerStoreKeySchema), []byte(stamp))
	}); err != nil {
		t.Fatal(err)
	}
}

func fileDigest(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

// RELEASE GATE. A store stamped by a newer build is refused, untouched: no
// setting is migrated or written, nothing is armed, the extender budget (which
// reads the same store) fails closed rather than writing, and the run ends
// with the startup-refused exit code so a supervisor retries.
func TestStateGuardRefusesAStoreStampedNewerThanTheBinary(t *testing.T) {
	h := newSettingsHarness(t)
	dbPath := h.path(providerStoreFileName)
	writeStampDirect(t, dbPath, `{"version":2,"min_reader":2,"stages":{"identity":"dual"}}`)
	grade := h.writeGrade()
	digest := fileDigest(t, dbPath)

	err := startProviderSettings()
	if err == nil {
		t.Fatal("a store stamped min_reader 2 was accepted by a build that reads up to 1")
	}
	if !errors.Is(err, errProviderStoreNewer) {
		t.Fatalf("refusal is not errProviderStoreNewer: %v", err)
	}
	wantText := fmt.Sprintf("open %s", dbPath)
	_ = wantText
	if !strings.Contains(err.Error(), "was written by a newer build (schema min_reader 2, this build reads up to 1); refusing to start slots or mint new identities") {
		t.Fatalf("refusal text = %q", err.Error())
	}
	t.Logf("refusal text: %s", err.Error())

	// The run turns it into the startup-refused exit.
	code, ok := providerRunExitCode(startupRefusal(t.Context(), err))
	if !ok || code != providerStartupRefusedExitCode {
		t.Fatalf("exit code = %d ok=%v, want %d", code, ok, providerStartupRefusedExitCode)
	}

	// Nothing was written to the store, and no consumer can use it.
	if got := fileDigest(t, dbPath); got != digest {
		t.Fatal("the refused store was modified")
	}
	if got := h.read("direct_grade.json"); !bytes.Equal(got, grade) {
		t.Fatal("the refusal removed a settings file")
	}
	for _, setting := range providerSettings {
		if settingArmed(setting.key) {
			t.Errorf("%s armed on a refused store", setting.key)
		}
	}
	if store := providerStoreInstance(); store != nil {
		t.Fatal("a refused store was handed to a consumer")
	}
	if err := mutateExtenderActivationRecord(extenderTestAccountA, func(*extenderActivationRecord) error { return nil }); err == nil {
		t.Fatal("the extender budget wrote to a refused store")
	}
	if got := fileDigest(t, dbPath); got != digest {
		t.Fatal("a consumer modified the refused store")
	}
	if got := h.errCount(providerStoreSchemaNewerCategory); got == 0 {
		t.Fatal("the refusal is not counted")
	}
}

// A stamp the build cannot read is treated as newer, not as absent.
func TestStateGuardRefusesAnUnreadableStamp(t *testing.T) {
	for name, stamp := range map[string]string{
		"not json":        `not json`,
		"no version":      `{}`,
		"zero version":    `{"version":0,"min_reader":0}`,
		"string version":  `{"version":"2"}`,
		"newer no reader": `{"version":2}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newSettingsHarness(t)
			writeStampDirect(t, h.path(providerStoreFileName), stamp)
			if err := startProviderSettings(); !errors.Is(err, errProviderStoreNewer) {
				t.Fatalf("stamp %q: err = %v, want a newer-store refusal", stamp, err)
			}
		})
	}
}

// A store stamped by a later build that kept min_reader at or below this one's
// is readable. It is not refused and its stamp is not downgraded or stripped.
func TestStateGuardAcceptsALaterStampThatStaysReadable(t *testing.T) {
	h := newSettingsHarness(t)
	writeStampDirect(t, h.path(providerStoreFileName), `{"version":3,"min_reader":1,"future":{"k":1}}`)
	if err := startProviderSettings(); err != nil {
		t.Fatalf("a readable later stamp was refused: %v", err)
	}
	var object map[string]json.RawMessage
	store := providerStoreInstance()
	store.mu.Lock()
	err := store.db.View(func(tx *bbolt.Tx) error {
		return json.Unmarshal(tx.Bucket([]byte(providerStoreBucketMeta)).Get([]byte(providerStoreKeySchema)), &object)
	})
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if string(object["version"]) != "3" || string(object["future"]) != `{"k":1}` {
		t.Fatalf("the stamp lost fields: %v", object)
	}
	if !strings.Contains(string(object["stages"]), `"settings":"db"`) {
		t.Fatalf("the settings stage is not recorded: %v", object)
	}
}

// A store written before the guard existed carries no stamp. It is stamped and
// its extender budget is untouched.
func TestStateGuardStampsAnUnstampedStoreWithoutDisturbingItsRecords(t *testing.T) {
	h := newSettingsHarness(t)
	store := providerStoreInstance()
	record := extenderActivationRecord{QuarantineUntilUnix: 12345}
	if err := store.save(providerStoreBucketState, providerStoreKeyExtenderActivation, record); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		return tx.DeleteBucket([]byte(providerStoreBucketMeta))
	}); err != nil {
		t.Fatal(err)
	}
	h.restart()
	if err := startProviderSettings(); err != nil {
		t.Fatal(err)
	}
	var got extenderActivationRecord
	found, err := providerStoreInstance().load(providerStoreBucketState, providerStoreKeyExtenderActivation, &got)
	if err != nil || !found || got.QuarantineUntilUnix != 12345 {
		t.Fatalf("the extender record changed: found=%v err=%v %+v", found, err, got)
	}
	if h.stamp().Version != providerStoreSchemaVersion {
		t.Fatal("the store was not stamped")
	}
}
