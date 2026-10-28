package miner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sdk"
)

// A mutation that fails must leave the stored record exactly as it was: the
// read and the write share one transaction, so there is no half-applied state
// for the next reader to find.
func TestProviderStoreUpdateAbortsAndLeavesTheRecord(t *testing.T) {
	testExtenderState(t)
	type record struct {
		Count int `json:"count"`
	}
	if err := providerStoreInstance().save(providerStoreBucketState, "probe", record{Count: 1}); err != nil {
		t.Fatalf("save: %s", err)
	}

	wantErr := errors.New("mutate refused")
	err := providerStoreInstance().updateJSON(providerStoreBucketState, "probe", &record{}, func(found bool) error {
		if !found {
			t.Fatal("update did not see the stored record")
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("update error = %v, want the mutation's own error", err)
	}

	var got record
	if _, err := providerStoreInstance().load(providerStoreBucketState, "probe", &got); err != nil {
		t.Fatalf("load after an aborted update: %s", err)
	}
	if got.Count != 1 {
		t.Fatalf("count = %d after an aborted update, want the stored 1", got.Count)
	}

	if err := providerStoreInstance().updateJSON(providerStoreBucketState, "probe", &got, func(found bool) error {
		got.Count++
		return nil
	}); err != nil {
		t.Fatalf("committed update: %s", err)
	}
	var after record
	if _, err := providerStoreInstance().load(providerStoreBucketState, "probe", &after); err != nil {
		t.Fatalf("load after a committed update: %s", err)
	}
	if after.Count != 2 {
		t.Fatalf("count = %d after a committed update, want 2", after.Count)
	}
}

// A file bbolt can never load is moved aside once and the run starts from a
// fresh store, instead of warning about the same bytes on every open for the
// life of the process.
func TestProviderStoreQuarantinesACorruptFile(t *testing.T) {
	testExtenderState(t)
	path, err := providerStatePath(providerStoreFileName)
	if err != nil {
		t.Fatalf("store path: %s", err)
	}
	if err := os.WriteFile(path, []byte("this is not a bbolt database"), 0o600); err != nil {
		t.Fatalf("write the corrupt file: %s", err)
	}

	store, err := openProviderStoreForProcess()
	if err != nil {
		t.Fatalf("a corrupt file must be quarantined, not reported as unavailable: %s", err)
	}
	if store == nil {
		t.Fatal("no store after quarantining the corrupt file")
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read the state dir: %s", err)
	}
	var quarantined []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), providerStoreFileName+".corrupt.") {
			quarantined = append(quarantined, entry.Name())
		}
	}
	if len(quarantined) != 1 {
		t.Fatalf("quarantined files = %v, want exactly one %s.corrupt.<ts>", quarantined, providerStoreFileName)
	}

	// The fresh store is usable, which is the point of moving the old one.
	if err := store.save(providerStoreBucketState, "probe", map[string]int{"n": 1}); err != nil {
		t.Fatalf("save into the fresh store: %s", err)
	}
}

// A refusal the listener sees must be charged to the account the sign-in token
// names, not to the key the listener happened to be built with. Adoption later
// would move an unidentified charge onto the account, but a success arriving
// first clears the placeholder and leaves the real account's hold standing.
func TestExtenderActivationListenerChargesTheTokenAccount(t *testing.T) {
	testExtenderState(t)
	writeAccountToken(t, extenderTestAccountA)
	now := time.Now()

	listener := newProviderExtenderStatusListener(nil, 0, extenderActivationUnidentifiedAccount)
	listener.ExtenderProvideStatusChanged(&sdk.ExtenderProvideStatus{
		Enabled:               true,
		Listening:             true,
		LastActivationError:   "refused",
		LastActivationRefused: true,
		LastActivationTime:    now.Unix(),
	})

	record, err := readExtenderActivationRecord(extenderTestAccountA)
	if err != nil {
		t.Fatalf("read the budget record: %s", err)
	}
	state, ok := record.Accounts[extenderActivationAccountKey(extenderTestAccountA)]
	if !ok || state.BackoffUntilUnix == 0 {
		t.Fatalf("the refusal landed on %v, want it charged to the token's account %q",
			record.Accounts, extenderTestAccountA)
	}
	if _, adopted := record.Accounts[extenderActivationUnidentifiedAccount]; adopted {
		t.Fatalf("the refusal is still parked on the unidentified key: %+v", record.Accounts)
	}
}

// A store that cannot be used holds the extender role rather than spending
// posts nothing will remember: fail closed, not open.
func TestExtenderActivationDeniesWhenTheStoreIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write the blocking file: %s", err)
	}
	t.Setenv("URNETWORK_STATE_DIR", blocker)
	closeProviderStore()
	t.Cleanup(closeProviderStore)

	if allow, wait, _ := extenderActivationAllow(time.Now(), postsPerDualStackPass, extenderTestAccountA); allow {
		t.Fatalf("the role was allowed with no usable store (wait=%s), want it held", wait)
	}
}
