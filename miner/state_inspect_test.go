package miner

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

const (
	inspectSecretJWT  = "eyJhbGciOiJIUzI1NiJ9.SECRETPAYLOAD.SECRETSIG"
	inspectSecretPass = "pa55w0rd-do-not-print"
	inspectSecretSeed = "seed words abandon ability able"
)

func inspectStore(t *testing.T) *providerStore {
	t.Helper()
	s, err := openProviderStore(filepath.Join(t.TempDir(), providerStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := checkProviderStoreSchema(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func inspectPutRaw(t *testing.T, s *providerStore, bucket, key string, raw []byte) {
	t.Helper()
	if err := s.write(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), raw)
	}); err != nil {
		t.Fatal(err)
	}
}

func inspectFamilyRecord(t *testing.T, s *providerStore, mode migrationMode, op *migrationOperation, record *migrationRecord) {
	t.Helper()
	if err := migrationUpdate(s, func(tx *bbolt.Tx) error {
		if err := writeMigrationFamily(tx, stateIdentityFamily, migrationFamily{Version: 1, Mode: mode, Operation: op}); err != nil {
			return err
		}
		if record != nil {
			return writeMigrationRecord(tx, stateIdentityFamily, *record)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func fileSideOf(data string) func() migrationSide {
	return func() migrationSide {
		return migrationSide{Present: true, Record: migrationRecord{Format: "f", Bytes: []byte(data)}}
	}
}

func identityFamilyOf(t *testing.T, r stateVerifyReport) stateFamilyReport {
	t.Helper()
	for _, f := range r.Families {
		if f.Name == stateIdentityFamily {
			return f
		}
	}
	t.Fatalf("no identity family in %+v", r)
	return stateFamilyReport{}
}

func TestVerifyZeroValueIsNotClean(t *testing.T) {
	if r := newStateVerifyReport(); r.Status == stateClean || r.Store == stateClean {
		t.Fatalf("zero report reads clean: %+v", r)
	}
	if r := newStateIdentitiesReport(); r.Status == stateClean {
		t.Fatalf("zero identities report reads clean: %+v", r)
	}
	if stateSeverity("") == 0 || stateSeverity("garbage") == 0 {
		t.Fatal("an unknown status must not rank as clean")
	}
}

func TestVerifyStatusesAreDistinctAndNeverClean(t *testing.T) {
	newer := inspectStore(t)
	inspectPutRaw(t, newer, providerStoreBucketMeta, providerStoreKeySchema, []byte(`{"version":99,"min_reader":99}`))
	unreadableStamp := inspectStore(t)
	inspectPutRaw(t, unreadableStamp, providerStoreBucketMeta, providerStoreKeySchema, []byte(`not json`))
	unstamped := inspectStore(t)
	if err := unstamped.write(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(providerStoreBucketMeta)).Delete([]byte(providerStoreKeySchema))
	}); err != nil {
		t.Fatal(err)
	}
	badRecord := inspectStore(t)
	inspectPutRaw(t, badRecord, providerMigrationRecordsBucket, "identity", []byte(`{broken`))
	badFamily := inspectStore(t)
	inspectPutRaw(t, badFamily, providerMigrationBucket, stateIdentityFamily, []byte(`{broken`))
	fresh := inspectStore(t)
	closed := inspectStore(t)
	closed.Close()

	cases := []struct {
		name  string
		store *providerStore
		err   error
		want  stateStatus
	}{
		{"no store and no reason", nil, nil, stateUnavailable},
		{"open failed", nil, errors.New("open /secret/path/provider_state.db: boom"), stateUnavailable},
		{"locked", nil, fmt.Errorf("open: %w", bbolt.ErrTimeout), stateLocked},
		{"corrupt open", nil, fmt.Errorf("%w: page", errProviderStoreCorrupt), stateCorrupt},
		{"released", nil, errProviderStoreReleased, stateReleased},
		{"newer open", nil, &providerStoreNewerError{minReader: 9}, stateNewer},
		{"newer stamp", newer, nil, stateNewer},
		{"unreadable stamp", unreadableStamp, nil, stateNewer},
		{"unstamped", unstamped, nil, stateUnchecked},
		{"undecodable record", badRecord, nil, stateCorrupt},
		{"undecodable family", badFamily, nil, stateCorrupt},
		{"fresh store, nothing migrated", fresh, nil, stateUnchecked},
		{"closed handle", closed, nil, stateUnavailable},
	}
	seen := map[stateStatus]bool{}
	for _, c := range cases {
		r := buildStateVerifyReport(c.store, c.err, fileSideOf("x"))
		if r.Status != c.want {
			t.Errorf("%s: status %q, want %q (%+v)", c.name, r.Status, c.want, r)
		}
		if r.Status == stateClean {
			t.Errorf("%s: read clean", c.name)
		}
		seen[r.Status] = true
	}
	for _, want := range []stateStatus{stateUnavailable, stateLocked, stateCorrupt, stateReleased, stateNewer, stateUnchecked} {
		if !seen[want] {
			t.Errorf("no case produced its own status %q", want)
		}
	}
}

func TestVerifyFamilyReconciliation(t *testing.T) {
	rec := migrationRecord{Format: "f", Bytes: []byte("same")}
	committed := &migrationOperation{Generation: 3, State: migrationCommitted}
	pending := &migrationOperation{Generation: 4, State: migrationPending}
	cases := []struct {
		name   string
		mode   migrationMode
		op     *migrationOperation
		record *migrationRecord
		file   string
		want   stateStatus
		seal   string
	}{
		{"uninitialized is unchecked", migrationUninitialized, nil, nil, "same", stateUnchecked, "none"},
		{"equal is the only clean", migrationVerified, committed, &rec, "same", stateClean, "committed"},
		{"conflict", migrationVerified, committed, &rec, "other", stateConflict, "committed"},
		{"only file", migrationShadow, committed, nil, "same", stateDiverged, "committed"},
		{"pending seal", migrationShadow, pending, &rec, "same", statePending, "pending"},
	}
	for _, c := range cases {
		s := inspectStore(t)
		inspectFamilyRecord(t, s, c.mode, c.op, c.record)
		r := buildStateVerifyReport(s, nil, fileSideOf(c.file))
		f := identityFamilyOf(t, r)
		if f.Status != c.want || r.Status != c.want || f.Seal != c.seal {
			t.Errorf("%s: family %+v report %q, want %q seal %q", c.name, f, r.Status, c.want, c.seal)
		}
	}
	// A file that cannot be read is not "only in the store".
	s := inspectStore(t)
	inspectFamilyRecord(t, s, migrationVerified, committed, &rec)
	r := buildStateVerifyReport(s, nil, func() migrationSide { return migrationSide{Err: errors.New("x")} })
	if r.Status != stateUnreadable {
		t.Errorf("unreadable file: %q", r.Status)
	}
}

func inspectFixture(t *testing.T) *providerStore {
	s := inspectStore(t)
	rec := migrationRecord{Format: "f", Bytes: []byte(inspectSecretJWT)}
	inspectFamilyRecord(t, s, migrationShadow, &migrationOperation{Generation: 1, State: migrationCommitted}, &rec)
	events := []any{
		providerIdentityEvent{Slot: "user:" + inspectSecretPass + "@10.0.0.1:1080", Outcome: "minted_new"},
		providerIdentityEvent{Slot: "user:" + inspectSecretPass + "@10.0.0.1:1080", Outcome: "adopted", Reason: inspectSecretSeed},
		providerIdentityEvent{Slot: "direct", Outcome: "revoked"},
	}
	if err := s.appendBoundedMany(providerStoreBucketIdentityEvents, events, 100); err != nil {
		t.Fatal(err)
	}
	// One undecodable event, appended raw.
	if err := s.write(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(providerStoreBucketIdentityEvents))
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, 9999)
		return b.Put(key, []byte("{broken "+inspectSecretJWT))
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerifyAndIdentitiesDoNotWrite(t *testing.T) {
	s := inspectFixture(t)
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	writes, commits := providerStoreWrites.Load(), providerStoreCommits.Load()
	for i := 0; i < 3; i++ {
		buildStateVerifyReport(s, nil, fileSideOf("other"))
		buildStateIdentitiesReport(s, nil)
	}
	after, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("inspection changed the store file")
	}
	if providerStoreWrites.Load() != writes || providerStoreCommits.Load() != commits {
		t.Fatal("inspection began a write transaction")
	}
}

func TestIdentitiesOutputHasNoSecrets(t *testing.T) {
	s := inspectFixture(t)
	r := buildStateIdentitiesReport(s, nil)
	v := buildStateVerifyReport(s, nil, fileSideOf(inspectSecretJWT))
	bad := buildStateVerifyReport(nil, fmt.Errorf("open %s: %w", s.path, bbolt.ErrTimeout), nil)
	for name, report := range map[string]any{"identities": r, "verify": v, "failed-open": bad} {
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		out := string(raw)
		for _, secret := range []string{inspectSecretJWT, "SECRETPAYLOAD", inspectSecretPass, inspectSecretSeed, "10.0.0.1", filepath.Dir(s.path), providerStoreFileName, "user:"} {
			if strings.Contains(out, secret) {
				t.Errorf("%s output contains %q: %s", name, secret, out)
			}
		}
	}
	if len(r.Slots) != 2 || r.Events != 3 || r.Undecodable != 1 {
		t.Fatalf("counts wrong: %+v", r)
	}
	if r.Status != stateCorrupt {
		t.Fatalf("an undecodable event must not read clean: %q", r.Status)
	}
	for _, slot := range r.Slots {
		if !strings.HasPrefix(string(slot.Slot), "slot-") || len(slot.Slot) != len("slot-")+8 {
			t.Errorf("slot not redacted: %q", slot.Slot)
		}
		if slot.Events == 2 && slot.Outcome != "adopted" {
			t.Errorf("latest outcome not reported: %+v", slot)
		}
	}
	if redactSlot("a") != redactSlot("a") || redactSlot("a") == redactSlot("b") {
		t.Fatal("slot labels must be stable and distinct")
	}
}

func setOwnerStoreForTest(t *testing.T, s *providerStore, released bool, failed error) {
	t.Helper()
	globalProviderStoreMu.Lock()
	oldS, oldR, oldE := globalProviderStore, globalProviderStoreReleased, globalProviderStoreFailedErr
	globalProviderStore, globalProviderStoreReleased, globalProviderStoreFailedErr = s, released, failed
	globalProviderStoreMu.Unlock()
	t.Cleanup(func() {
		globalProviderStoreMu.Lock()
		globalProviderStore, globalProviderStoreReleased, globalProviderStoreFailedErr = oldS, oldR, oldE
		globalProviderStoreMu.Unlock()
	})
}

func TestInspectLiveOwnerServesStateRequests(t *testing.T) {
	s := inspectFixture(t)
	setOwnerStoreForTest(t, s, false, nil)
	old := stateIdentityFileSide
	stateIdentityFileSide = fileSideOf("different")
	t.Cleanup(func() { stateIdentityFileSide = old })

	resp := handleControlRequest(nil, controlRequest{Cmd: "state", Action: "verify"})
	if !resp.OK {
		t.Fatalf("verify: %s", resp.Error)
	}
	var v stateVerifyReport
	if err := json.Unmarshal([]byte(resp.Value), &v); err != nil {
		t.Fatal(err)
	}
	if v.Status != stateConflict || !v.ReadOnly || identityFamilyOf(t, v).Status != stateConflict {
		t.Fatalf("unexpected verify reply: %+v", v)
	}
	resp = handleControlRequest(nil, controlRequest{Cmd: "state", Action: "identities"})
	var id stateIdentitiesReport
	if !resp.OK || json.Unmarshal([]byte(resp.Value), &id) != nil || len(id.Slots) != 2 {
		t.Fatalf("identities reply: ok=%v err=%s %s", resp.OK, resp.Error, resp.Value)
	}
	if resp := handleControlRequest(nil, controlRequest{Cmd: "state", Action: "repair"}); resp.OK {
		t.Fatal("an unknown action must be refused")
	}

	// Released and failed-open owners say so.
	setOwnerStoreForTest(t, nil, true, nil)
	resp = handleControlRequest(nil, controlRequest{Cmd: "state", Action: "verify"})
	_ = json.Unmarshal([]byte(resp.Value), &v)
	if v.Status != stateReleased {
		t.Fatalf("released: %q", v.Status)
	}
	setOwnerStoreForTest(t, nil, false, fmt.Errorf("x: %w", bbolt.ErrTimeout))
	resp = handleControlRequest(nil, controlRequest{Cmd: "state", Action: "verify"})
	_ = json.Unmarshal([]byte(resp.Value), &v)
	if v.Status != stateLocked {
		t.Fatalf("locked: %q", v.Status)
	}
}

func TestInspectionNeverOpensTheStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	setOwnerStoreForTest(t, nil, false, nil)
	resp := handleControlRequest(nil, controlRequest{Cmd: "state", Action: "verify"})
	var v stateVerifyReport
	if !resp.OK || json.Unmarshal([]byte(resp.Value), &v) != nil || v.Status != stateUnavailable {
		t.Fatalf("closed owner: %+v %s", v, resp.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, providerStoreFileName)); !os.IsNotExist(err) {
		t.Fatalf("an inspection created the store (stat err %v)", err)
	}
}

// A stored outcome is not trusted text: only the four recorded outcomes are
// ever shown, anything else is reported as unknown and counted undecodable.
func TestIdentitiesOutcomeIsWhitelisted(t *testing.T) {
	s := inspectStore(t)
	const leak = "eyJSECRET.jwt"
	events := []any{
		providerIdentityEvent{Slot: "direct", Outcome: leak},
		providerIdentityEvent{Slot: "other", Outcome: "adopted"},
	}
	if err := s.appendBoundedMany(providerStoreBucketIdentityEvents, events, 100); err != nil {
		t.Fatal(err)
	}
	r := buildStateIdentitiesReport(s, nil)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), leak) || strings.Contains(string(raw), "SECRET") {
		t.Fatalf("stored outcome echoed: %s", raw)
	}
	if r.Status == stateClean || r.Undecodable != 1 {
		t.Fatalf("unknown outcome must count undecodable and not read clean: %+v", r)
	}
	found := false
	for _, slot := range r.Slots {
		if slot.Outcome == stateOutcomeUnknown {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown outcome not reported as %q: %+v", stateOutcomeUnknown, r.Slots)
	}
}

// The identity file is read with no read transaction open and the store mutex
// free, so a slow or large file cannot stall other store users or a writer.
func TestVerifyReadsFileSideOutsideTransaction(t *testing.T) {
	s := inspectStore(t)
	rec := migrationRecord{Format: "f", Bytes: []byte("x")}
	inspectFamilyRecord(t, s, migrationShadow, &migrationOperation{Generation: 1, State: migrationCommitted}, &rec)
	calls := 0
	probe := func() migrationSide {
		calls++
		if !s.mu.TryLock() {
			t.Error("file side read while the store mutex is held (inside the read transaction)")
		} else {
			s.mu.Unlock()
		}
		return migrationSide{Present: true, Record: rec}
	}
	r := buildStateVerifyReport(s, nil, probe)
	if calls == 0 {
		t.Fatal("file side never read")
	}
	if identityFamilyOf(t, r).Status != stateClean {
		t.Fatalf("matching sides should read clean: %+v", r)
	}
}

// Offline inspection: the provider is stopped, so the file is opened read only
// with a short timeout. It never creates, stamps, recovers or quarantines.
func TestOfflineVerifyLeavesFileUnchanged(t *testing.T) {
	s := inspectFixture(t)
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	infoBefore, _ := os.Stat(path)
	entriesBefore, _ := os.ReadDir(filepath.Dir(path))
	v, err := stateInspectOffline("verify", path, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	i, err := stateInspectOffline("identities", path, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var vr stateVerifyReport
	if json.Unmarshal([]byte(v), &vr) != nil || vr.Store != stateClean || vr.Schema != "supported" || vr.Records == 0 && len(vr.Families) == 0 {
		t.Fatalf("offline verify did not read the store: %s", v)
	}
	if !strings.Contains(i, `"slot-`) || strings.Contains(i, inspectSecretPass) {
		t.Fatalf("offline identities wrong: %s", i)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	infoAfter, _ := os.Stat(path)
	entriesAfter, _ := os.ReadDir(filepath.Dir(path))
	if !bytes.Equal(before, after) || !infoBefore.ModTime().Equal(infoAfter.ModTime()) || len(entriesBefore) != len(entriesAfter) {
		t.Fatal("offline inspection changed the file or the directory")
	}
}

func TestOfflineVerifyRefusesLockedFile(t *testing.T) {
	s := inspectStore(t) // the owner: holds the exclusive file lock
	start := time.Now()
	out, err := stateInspectOffline("verify", s.path, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var r stateVerifyReport
	if json.Unmarshal([]byte(out), &r) != nil || r.Status != stateLocked || r.Store != stateLocked {
		t.Fatalf("locked file must report locked, got %s", out)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("offline open was not bounded")
	}
	if strings.Contains(out, s.path) {
		t.Fatalf("report leaks the path: %s", out)
	}
}

func TestOfflineVerifyMissingAndEmptyFileAreNotCreatedOrStamped(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "sub", providerStoreFileName)
	out, err := stateInspectOffline("verify", missing, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var r stateVerifyReport
	if json.Unmarshal([]byte(out), &r) != nil || r.Status != stateUnavailable {
		t.Fatalf("missing file must be unavailable: %s", out)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatal("offline inspection created a directory")
	}
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = stateInspectOffline("verify", empty, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal([]byte(out), &r) != nil || r.Status == stateClean {
		t.Fatalf("empty file must not read clean: %s", out)
	}
	if info, _ := os.Stat(empty); info.Size() != 0 {
		t.Fatal("offline inspection initialised an empty file")
	}
}
