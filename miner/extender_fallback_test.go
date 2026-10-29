package miner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func fallbackTestSetup(t *testing.T) (*bytes.Buffer, string) {
	t.Helper()
	closeProviderStore()
	t.Cleanup(closeProviderStore)
	t.Setenv("URNETWORK_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	previous := proxyManagerLogWriter
	writer := &bytes.Buffer{}
	proxyManagerLogWriter = writer
	t.Cleanup(func() { proxyManagerLogWriter = previous })
	path, err := extenderFallbackPath()
	if err != nil {
		t.Fatal(err)
	}
	return writer, path
}

func readFallbackTest(t *testing.T, path string) extenderFallbackRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record extenderFallbackRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestExtenderFallbackStartupLocked(t *testing.T) {
	log, path := fallbackTestSetup(t)
	dir, _ := providerStateDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	locked, err := bbolt.Open(filepath.Join(dir, providerStoreFileName), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	gate := newExtenderActivationGate()
	if gate.decideAtStart() {
		t.Fatal("startup should defer until first launch probe")
	}
	if !gate.allowLaunch() {
		t.Fatal("locked store must allow via fallback")
	}
	record := readFallbackTest(t, path)
	if len(record.Launches) != 1 || record.Launches[0].Posts != hostActivationPosts() {
		t.Fatalf("record: %+v", record)
	}
	if !strings.Contains(log.String(), "extender role on: activation budget record unreadable; launch counted against fallback") {
		t.Fatalf("log: %s", log)
	}
	t.Logf("allowed; record=%+v; fallback log present", record)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	if !gate.allowLaunch() {
		t.Fatal("recovered store should allow next launch")
	}
	store := providerStoreInstance()
	if store == nil {
		t.Fatal("recovered store unavailable")
	}
	var actual extenderActivationRecord
	found, err := store.load(providerStoreBucketState, providerStoreKeyExtenderActivation, &actual)
	if err != nil || !found {
		t.Fatalf("real record found=%v err=%v", found, err)
	}
	if got := len(actual.Accounts[extenderActivationUnidentifiedAccount].Attempts); got != hostActivationPosts() {
		t.Fatalf("real posts=%d", got)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("fallback changed on recovery: %v", err)
	}
	t.Log("real store recovered; next launch charged once and fallback untouched")
}

func TestExtenderFallbackCapAndRecovery(t *testing.T) {
	log, path := fallbackTestSetup(t)
	state, calls := stubExtenderMutation(t, extenderActivationState{}, 2, 0)
	now := time.Now()
	for i := 0; i < 3; i++ {
		allow := allowExtenderFallback(now.Add(time.Duration(i)*time.Minute), 2)
		if allow != (i < 2) {
			t.Fatalf("launch %d allowed=%v", i+1, allow)
		}
	}
	record := readFallbackTest(t, path)
	if len(record.Launches) != 2 {
		t.Fatalf("launches=%+v", record.Launches)
	}
	if !strings.Contains(log.String(), "extender role off: fallback activation budget exhausted") {
		t.Fatalf("log: %s", log)
	}
	if *calls != 0 || len(state.Attempts) != 0 {
		t.Fatal("fallback touched real store")
	}
	// Recovery: no fallback charge is adopted or consulted by the real record.
	*calls = 2
	before, _ := os.ReadFile(path)
	if newExtenderActivationGate().allowLaunchForWith(2, extenderTestAccountA, 1, 0) != extenderLaunchAllowed {
		t.Fatal("recovered real record must allow")
	}
	if len(state.Attempts) != 2 {
		t.Fatalf("real posts=%v", state.Attempts)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("real decision changed fallback")
	}
	// The window expires and the fallback can be spent again.
	if !allowExtenderFallback(now.Add(time.Hour+time.Minute), 2) {
		t.Fatal("expired fallback window held")
	}
	if got := len(readFallbackTest(t, path).Launches); got != 1 {
		t.Fatalf("pruned launches=%d", got)
	}
	t.Log("two launches allowed, third refused; recovered store charged exactly two posts; fallback unchanged; expiry pruned")
}

func TestExtenderFallbackUnwritable(t *testing.T) {
	log, path := fallbackTestSetup(t)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	stubExtenderMutation(t, extenderActivationState{}, 100, 0)
	if !newExtenderActivationGate().allowLaunch() {
		t.Fatal("fallback directory must not stop launch")
	}
	if !strings.Contains(log.String(), "extender role on: activation budget record unreadable; fallback count lost:") || !strings.Contains(log.String(), "rename ") {
		t.Fatalf("log: %s", log)
	}
	t.Log("directory prevents fallback write; launch allowed and count-lost logged")
}

func TestExtenderFallbackRealExhaustedUntouched(t *testing.T) {
	log, path := fallbackTestSetup(t)
	store := providerStoreInstance()
	if store == nil {
		t.Fatal("open store")
	}
	now := time.Now().Unix()
	record := extenderActivationRecord{Accounts: map[string]extenderActivationState{extenderActivationUnidentifiedAccount: {Attempts: []int64{now, now, now, now, now}}}}
	if err := store.save(providerStoreBucketState, providerStoreKeyExtenderActivation, record); err != nil {
		t.Fatal(err)
	}
	if newExtenderActivationGate().allowLaunch() {
		t.Fatal("readable exhausted store must refuse")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fallback touched: %v", err)
	}
	if !strings.Contains(log.String(), "activation held:") {
		t.Fatalf("log: %s", log)
	}
	t.Log("readable exhausted record refused; fallback absent")
}

func TestExtenderFallbackReadableCapWriteFailure(t *testing.T) {
	_, path := fallbackTestSetup(t)
	now := time.Now().Unix()
	stubExtenderMutation(t, extenderActivationState{Attempts: []int64{now, now, now, now, now}}, 0, 100)
	if newExtenderActivationGate().allowLaunch() {
		t.Fatal("readable exhausted cap must refuse even if write fails")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fallback touched: %v", err)
	}
}
