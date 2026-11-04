package miner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	"github.com/urnetwork/connect"
)

// proofStoreNarration swaps the import's narration writer for a buffer.
func proofStoreNarration(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := legacyImportLogWriter
	legacyImportLogWriter = &buf
	t.Cleanup(func() { legacyImportLogWriter = old })
	return &buf
}

// interruptedStart is a start that proves every slot and dies before the
// credentials commit: the marker is durable, nothing else is.
func (self *legacyImportFixture) interruptedStart() {
	self.t.Helper()
	self.run(legacyImportHooks{afterMarkerWrite: func() error { return errors.New("synthetic: killed before commit") }})
	for _, settings := range self.proxySettings {
		if _, err := os.Stat(self.target(settings)); !errors.Is(err, os.ErrNotExist) {
			self.t.Fatalf("an interrupted start committed %s", settings.Key())
		}
	}
}

func (self *legacyImportFixture) proofFile() legacyImportProofFile {
	self.t.Helper()
	var file legacyImportProofFile
	if err := json.Unmarshal(self.read(legacyImportProofStoreName), &file); err != nil {
		self.t.Fatal(err)
	}
	return file
}

func (self *legacyImportFixture) resetProverCounts() {
	self.prover.mu.Lock()
	defer self.prover.mu.Unlock()
	self.prover.publicCalls = 0
	self.prover.refreshCall = 0
}

func (self *legacyImportFixture) proverCounts() (int, int) {
	self.prover.mu.Lock()
	defer self.prover.mu.Unlock()
	return self.prover.publicCalls, self.prover.refreshCall
}

// A start that finds a usable proof for every slot spends no live call at all
// and still brings every slot to a committed credential.
func TestProofStoreWarmStartPerformsZeroLiveProofs(t *testing.T) {
	narration := proofStoreNarration(t)
	fixture := newLegacyImportFixture(t, true, 4)
	fixture.interruptedStart()
	if public, _ := fixture.proverCounts(); public != 4 {
		t.Fatalf("the cold start made %d live proofs, want 4", public)
	}
	if got := len(fixture.proofFile().Slots); got != 4 {
		t.Fatalf("the cold start saved %d proofs, want 4", got)
	}

	fixture.resetProverCounts()
	narration.Reset()
	result := fixture.run(legacyImportHooks{})
	if public, refresh := fixture.proverCounts(); public != 0 || refresh != 0 {
		t.Fatalf("the warm start made live proofs: key reads=%d refreshes=%d, want 0 and 0", public, refresh)
	}
	if len(result.mintSlots) != 0 {
		t.Fatalf("the warm start minted: %v", result.mintSlots)
	}
	store, err := readLegacyStoreRaw(fixture)
	if err != nil {
		t.Fatal(err)
	}
	journal := fixture.journal()
	for _, settings := range fixture.proxySettings {
		raw, err := os.ReadFile(fixture.target(settings))
		if err != nil || string(raw) != store[settings.Key()].ByClientJWT {
			t.Fatalf("warm start did not commit %s: %v", settings.Key(), err)
		}
		if slot := journal.Slots[settings.Key()]; slot.State != "imported" || slot.Proof != legacyImportStoredProof {
			t.Fatalf("journal %s = %+v, want imported on a stored proof", settings.Key(), slot)
		}
	}
	if !strings.Contains(narration.String(), "summary: imported=4 fallback=0 skipped=0 left=0 elapsed=") ||
		!strings.Contains(narration.String(), "result=complete stored=4") {
		t.Fatalf("summary does not report the stored adoption:\n%s", narration.String())
	}
}

// The platform rejecting an adopted identity on its first use withdraws the
// proof; the next start proves that proxy live, and only that proxy.
func TestProofStoreRejectedOnUseFallsBackToLiveProof(t *testing.T) {
	fixture := newLegacyImportFixture(t, true, 3)
	fixture.interruptedStart()
	fixture.run(legacyImportHooks{})
	rejected := fixture.proxySettings[1]
	untouched := map[string][]byte{}
	for _, settings := range fixture.proxySettings {
		if settings != rejected {
			untouched[settings.Key()] = mustReadFile(t, fixture.target(settings))
		}
	}

	// What the registration path does when the platform refuses the client: the
	// credential goes away, and the run reports the rejection.
	if err := os.Remove(fixture.target(rejected)); err != nil {
		t.Fatal(err)
	}
	if err := noteLegacyImportProofRejected(fixture.dir, filepath.Base(fixture.target(rejected)), "client_revoked"); err != nil {
		t.Fatal(err)
	}
	record := fixture.proofFile().Slots[filepath.Base(fixture.target(rejected))]
	if record.ProvenAt != 0 || record.Rejections != 1 || record.LastRejection != "client_revoked" || record.LastRejectionAt == 0 {
		t.Fatalf("the rejection was not recorded: %+v", record)
	}

	fixture.resetProverCounts()
	fixture.run(legacyImportHooks{})
	if public, refresh := fixture.proverCounts(); public != 1 || refresh != 0 {
		t.Fatalf("key reads=%d refreshes=%d, want a live proof for the rejected proxy only", public, refresh)
	}
	if _, err := os.Stat(fixture.target(rejected)); err != nil {
		t.Fatalf("the rejected proxy is not proven after the live proof: %v", err)
	}
	if slot := fixture.journal().Slots[rejected.Key()]; slot.State != "imported" || slot.Proof != "server_key" {
		t.Fatalf("journal after the live proof = %+v", slot)
	}
	for key, want := range untouched {
		for _, settings := range fixture.proxySettings {
			if settings.Key() == key && !bytes.Equal(mustReadFile(t, fixture.target(settings)), want) {
				t.Fatalf("%s was touched by another proxy's fallback", key)
			}
		}
	}
	if fresh := fixture.proofFile().Slots[filepath.Base(fixture.target(rejected))]; fresh.ProvenAt == 0 || fresh.Rejections != 0 {
		t.Fatalf("the live proof was not stored: %+v", fresh)
	}
}

// A withdrawn proof never makes a dead proxy healthy: when the live proof also
// fails, the slot does not commit.
func TestProofStoreRejectedProofNeverMarksDeadProxyProven(t *testing.T) {
	fixture := newLegacyImportFixture(t, true, 2)
	fixture.interruptedStart()
	fixture.run(legacyImportHooks{})
	dead := fixture.proxySettings[0]
	if err := os.Remove(fixture.target(dead)); err != nil {
		t.Fatal(err)
	}
	if err := noteLegacyImportProofRejected(fixture.dir, filepath.Base(fixture.target(dead)), "client_revoked"); err != nil {
		t.Fatal(err)
	}
	fixture.prover.serviceKey[fixture.proxyClientIDs[dead.Key()]] = []byte("another-key-not-ours-0123456789a")

	result := fixture.run(legacyImportHooks{})
	if _, err := os.Stat(fixture.target(dead)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rejected proof committed a credential for a dead proxy: %v", err)
	}
	if !result.mintSlots[dead.Key()] {
		t.Fatalf("the failed live proof did not take the documented fallback")
	}
	if fixture.proofFile().Slots[filepath.Base(fixture.target(dead))].ProvenAt != 0 {
		t.Fatalf("the rejected proof became usable again")
	}
}

// An absent, corrupt, older-format or tampered store is today's behaviour: the
// proofs run live and the summary line has its old shape.
func TestProofStoreCorruptOrAbsentBehavesLikeToday(t *testing.T) {
	corruptions := map[string]func(t *testing.T, fixture *legacyImportFixture){
		"absent": func(t *testing.T, fixture *legacyImportFixture) {
			if err := os.Remove(filepath.Join(fixture.dir, legacyImportProofStoreName)); err != nil {
				t.Fatal(err)
			}
		},
		"garbage": func(t *testing.T, fixture *legacyImportFixture) {
			fixture.write(legacyImportProofStoreName, []byte("{not json"))
		},
		"truncated": func(t *testing.T, fixture *legacyImportFixture) {
			raw := fixture.read(legacyImportProofStoreName)
			fixture.write(legacyImportProofStoreName, raw[:len(raw)/2])
		},
		"older_format": func(t *testing.T, fixture *legacyImportFixture) {
			fixture.write(legacyImportProofStoreName, []byte(`{"schema":"urnetwork-legacy-import-proofs-v0","slots":{}}`))
		},
		"no_schema": func(t *testing.T, fixture *legacyImportFixture) {
			fixture.write(legacyImportProofStoreName, []byte(`[]`))
		},
	}
	for name, corrupt := range corruptions {
		t.Run(name, func(t *testing.T) {
			narration := proofStoreNarration(t)
			fixture := newLegacyImportFixture(t, true, 3)
			fixture.interruptedStart()
			corrupt(t, fixture)
			fixture.resetProverCounts()
			narration.Reset()
			fixture.run(legacyImportHooks{})
			if public, _ := fixture.proverCounts(); public != 3 {
				t.Fatalf("%s: %d live proofs, want all 3 proven live", name, public)
			}
			if strings.Contains(narration.String(), "stored=") {
				t.Fatalf("%s: summary changed shape:\n%s", name, narration.String())
			}
			if !strings.Contains(narration.String(), "summary: imported=3 fallback=0 skipped=0 left=0 elapsed=") {
				t.Fatalf("%s: summary is not the old line:\n%s", name, narration.String())
			}
			for _, slot := range fixture.journal().Slots {
				if slot.Proof == legacyImportStoredProof {
					t.Fatalf("%s: a slot was adopted from a store that cannot be trusted", name)
				}
			}
			// The store is whole again afterwards: a bad file is replaced, not kept.
			var file legacyImportProofFile
			if err := json.Unmarshal(fixture.read(legacyImportProofStoreName), &file); err != nil || file.Schema != legacyImportProofStoreSchema || len(file.Slots) != 3 {
				t.Fatalf("%s: store not rewritten whole: %v %+v", name, err, file)
			}
		})
	}
}

// A record that is not exactly a proof of the slot as it is now is never used:
// each case leaves one slot to prove live.
func TestProofStoreMalformedOrStaleRecordIsNeverTrusted(t *testing.T) {
	other := connect.NewId().String()
	mutations := map[string]func(r *legacyImportProofRecord){
		"stale": func(r *legacyImportProofRecord) {
			r.ProvenAt = time.Now().Add(-legacyImportProofMaxAge - time.Hour).Unix()
		},
		"future":          func(r *legacyImportProofRecord) { r.ProvenAt = time.Now().Add(time.Hour).Unix() },
		"other_client":    func(r *legacyImportProofRecord) { r.ClientID = other },
		"other_network":   func(r *legacyImportProofRecord) { r.NetworkID = other },
		"other_key":       func(r *legacyImportProofRecord) { r.KeyFP = legacyImportFingerprint([]byte("another key")) },
		"other_token":     func(r *legacyImportProofRecord) { r.TokenFP = legacyImportFingerprint([]byte("another token")) },
		"short_key_print": func(r *legacyImportProofRecord) { r.KeyFP = "abcd" },
		"unknown_proof":   func(r *legacyImportProofRecord) { r.Proof = "trust_me" },
		"negative_time":   func(r *legacyImportProofRecord) { r.ProvenAt = -5 },
		"rejected":        func(r *legacyImportProofRecord) { r.ProvenAt = 0; r.Rejections = 1 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			fixture := newLegacyImportFixture(t, true, 2)
			fixture.interruptedStart()
			file := fixture.proofFile()
			key := filepath.Base(fixture.target(fixture.proxySettings[0]))
			record := file.Slots[key]
			mutate(&record)
			file.Slots[key] = record
			raw, err := json.Marshal(file)
			if err != nil {
				t.Fatal(err)
			}
			fixture.write(legacyImportProofStoreName, raw)

			fixture.resetProverCounts()
			fixture.run(legacyImportHooks{})
			if public, _ := fixture.proverCounts(); public != 1 {
				t.Fatalf("%s: %d live proofs, want exactly the tampered slot proven live", name, public)
			}
			journal := fixture.journal()
			if journal.Slots[fixture.proxySettings[0].Key()].Proof == legacyImportStoredProof {
				t.Fatalf("%s: the tampered proof was trusted", name)
			}
			if journal.Slots[fixture.proxySettings[1].Key()].Proof != legacyImportStoredProof {
				t.Fatalf("%s: the intact proof was not used", name)
			}
		})
	}
}

// A slot whose proof stays unavailable while the API answers other slots keeps
// its attempt count across process restarts and reaches the terminal fallback
// instead of being retried forever. Each start queues a companion that answers,
// which is the evidence that the API is up.
func TestProofDeferralSurvivesRestartAndReachesTerminal(t *testing.T) {
	oldBackoff := legacyImportTransientBackoff
	legacyImportTransientBackoff = 0
	defer func() { legacyImportTransientBackoff = oldBackoff }()

	fixture := newLegacyImportFixture(t, true, 3)
	stuck, companions := fixture.proxySettings[0], fixture.proxySettings[1:]
	store, err := readLegacyStoreRaw(fixture)
	if err != nil {
		t.Fatal(err)
	}
	// The stuck slot's key read is inconclusive and its refresh times out.
	delete(fixture.prover.serviceKey, fixture.proxyClientIDs[stuck.Key()])
	fixture.prover.refreshErr[store[stuck.Key()].ByClientJWT] = &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ETIMEDOUT}

	for start := 1; start <= legacyImportDeferralStarts; start++ {
		// A fresh process each time: nothing but the files carries over. The
		// companions are queued again so the start has an answered proof.
		journal := fixture.journal()
		for _, settings := range companions {
			delete(journal.Slots, settings.Key())
			_ = os.Remove(fixture.target(settings))
		}
		// An adoption from the store is no evidence the API answers, so the
		// companions must prove live this start.
		_ = os.Remove(filepath.Join(fixture.dir, legacyImportProofStoreName))
		if err := writeLegacyImportJournal(fixture.dir, journal); err != nil {
			t.Fatal(err)
		}

		result := fixture.run(legacyImportHooks{})
		recorded := fixture.journal().Slots[stuck.Key()]
		if start < legacyImportDeferralStarts {
			if len(result.mintSlots) != 0 {
				t.Fatalf("start %d reached the terminal state early", start)
			}
			if recorded.State != "defer" || recorded.Deferrals != start {
				t.Fatalf("start %d journal = %+v, want defer with %d attempts that survived the restart", start, recorded, start)
			}
			continue
		}
		if !result.mintSlots[stuck.Key()] || recorded.State != "fallback" || recorded.Reason != "proof_unavailable_bound" {
			t.Fatalf("start %d journal = %+v mint=%v, want the terminal fallback", start, recorded, result.mintSlots)
		}
	}

	// Terminal means terminal: the next start does not prove the slot again.
	fixture.resetProverCounts()
	later := fixture.run(legacyImportHooks{})
	if !later.mintSlots[stuck.Key()] {
		t.Fatalf("the terminal state did not hold")
	}
	if _, refresh := fixture.proverCounts(); refresh != 0 {
		t.Fatalf("a terminal slot was proven again (%d refreshes)", refresh)
	}
}

// With no answered proof in the start, a lone timeout is indistinguishable from
// an outage and keeps deferring without counting.
func TestProofDeferralWithoutAnswerNeverCounts(t *testing.T) {
	oldBackoff := legacyImportTransientBackoff
	legacyImportTransientBackoff = 0
	defer func() { legacyImportTransientBackoff = oldBackoff }()

	fixture := newLegacyImportFixture(t, true, 1)
	stuck := fixture.proxySettings[0]
	store, err := readLegacyStoreRaw(fixture)
	if err != nil {
		t.Fatal(err)
	}
	delete(fixture.prover.serviceKey, fixture.proxyClientIDs[stuck.Key()])
	fixture.prover.refreshErr[store[stuck.Key()].ByClientJWT] = &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ETIMEDOUT}
	for start := 1; start <= legacyImportDeferralStarts+1; start++ {
		result := fixture.run(legacyImportHooks{})
		if len(result.mintSlots) != 0 {
			t.Fatalf("start %d minted with no evidence the API answers", start)
		}
		if recorded := fixture.journal().Slots[stuck.Key()]; recorded.State != "defer" || recorded.Deferrals != 0 {
			t.Fatalf("start %d journal = %+v", start, recorded)
		}
	}
}

// The grading gate keeps its contract: it holds while the pool is coming up,
// and a pool that comes up from disk (every worker connected at once) opens it
// on the running count, not on a fallback.
func TestGradingGateHoldsThenOpensFromWarmStart(t *testing.T) {
	const desired = 1500
	var running atomic.Int64
	gate := newGradingGate(desired)
	now := time.Unix(1_000_000, 0)
	gate.now = func() time.Time { return now }
	gate.runningCount = func() int { return int(running.Load()) }
	gate.noteWorkerLaunches()

	running.Store(258) // the cold start's measured count at 1m37s
	now = now.Add(97 * time.Second)
	if gate.observe() || gate.isOpen() {
		t.Fatal("the gate opened on a pool that is not up")
	}
	if gate.wait(canceledContext()) {
		t.Fatal("a held gate released a waiter")
	}

	running.Store(desired * 90 / 100) // warm: the pool is up from disk
	now = now.Add(time.Second)
	if !gate.observe() || !gate.isOpen() {
		t.Fatal("the gate did not open from a warm pool")
	}
	if !strings.HasPrefix(gate.openReason, gradingGateReasonRunning) {
		t.Fatalf("opened for %q, want the running count", gate.openReason)
	}
	if !gate.wait(canceledContext()) {
		t.Fatal("an open gate held a waiter")
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// Concurrent writers must never overwrite a recorded rejection with an older
// dirty proof: a recorded rejection is sticky and authoritative against any
// stale in-memory record.
func TestProofStoreRejectionRaceConcurrentFlush(t *testing.T) {
	dir := t.TempDir()
	store1 := loadLegacyImportProofStore(dir)
	claim := legacyImportProofClaim{
		ClientID:  connect.NewId().String(),
		NetworkID: connect.NewId().String(),
		KeyFP:     legacyImportFingerprint([]byte("test-key-material-0123456789012345")),
		TokenFP:   legacyImportFingerprint([]byte("test-token-jwt-material-0123456789")),
	}
	key := "proxy-slot-1.jwt"
	store1.record(key, claim, "server_key")
	if err := store1.flush(); err != nil {
		t.Fatal(err)
	}

	// Stale in-memory copy: store2 loads the store while key has a valid proof.
	store2 := loadLegacyImportProofStore(dir)
	if !store2.usable(key, claim) {
		t.Fatal("store2 should see usable proof initially")
	}
	// store2 has an older dirty record in memory.
	store2.dirty[key] = true

	// Concurrent writer notes a rejection on disk.
	if err := noteLegacyImportProofRejected(dir, key, "client_revoked"); err != nil {
		t.Fatal(err)
	}

	// Concurrently flush store2 with its stale dirty proof.
	if err := store2.flush(); err != nil {
		t.Fatal(err)
	}

	// On disk and in memory, the rejection must not have been overwritten!
	diskStore := loadLegacyImportProofStore(dir)
	if diskStore.usable(key, claim) {
		t.Fatalf("FAIL: stale flush revived rejected proof: usable=true, proof=%+v", diskStore.slots[key])
	}
	if !diskStore.rejected(key) {
		t.Fatalf("FAIL: rejection on disk was overwritten: %+v", diskStore.slots[key])
	}
	if store2.usable(key, claim) {
		t.Fatalf("FAIL: in-memory store still reports usable after flush: %+v", store2.slots[key])
	}
}

// Concurrently writing rejections while background flushers run with stale records
// must never resurrect a rejected proof.
func TestProofStoreRejectionRaceConcurrentGoroutines(t *testing.T) {
	dir := t.TempDir()
	claim := legacyImportProofClaim{
		ClientID:  connect.NewId().String(),
		NetworkID: connect.NewId().String(),
		KeyFP:     legacyImportFingerprint([]byte("test-key-material-0123456789012345")),
		TokenFP:   legacyImportFingerprint([]byte("test-token-jwt-material-0123456789")),
	}
	key := "concurrent-slot.jwt"
	initial := loadLegacyImportProofStore(dir)
	initial.record(key, claim, "server_key")
	if err := initial.flush(); err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup

	// Goroutines repeatedly flushing stale in-memory store
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				stale := loadLegacyImportProofStore(dir)
				stale.mu.Lock()
				stale.dirty[key] = true
				stale.mu.Unlock()
				_ = stale.flush()
			}
		}()
	}

	// Reject the proof on disk while flushes are running.
	if err := noteLegacyImportProofRejected(dir, key, "client_revoked"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	// Verify the rejection remained sticky on disk and was not overwritten.
	finalStore := loadLegacyImportProofStore(dir)
	if finalStore.usable(key, claim) {
		t.Fatalf("FAIL: concurrent flush revived rejected proof: usable=true, proof=%+v", finalStore.slots[key])
	}
	if !finalStore.rejected(key) {
		t.Fatalf("FAIL: rejection was lost under concurrent writers: %+v", finalStore.slots[key])
	}
}

// An adopted proof means the credential was valid at some point, not that the
// proxy works. The import phase dials the API directly without proxy traversal,
// so a slot with an unreachable proxy address is adopted on credential validity
// alone without testing proxy reachability.
func TestProofAdoptionCredentialValidityNotProxyReachability(t *testing.T) {
	unreachable := &connect.ProxySettings{
		Network: "tcp",
		Address: "192.0.2.1:1",
		Auth:    &proxy.Auth{User: "fakeuser1", Password: "fakepass"},
	}
	fixture := newLegacyImportFixtureWithSettings(t, []*connect.ProxySettings{unreachable})
	fixture.interruptedStart()

	file := fixture.proofFile()
	if !file.CredentialValidityOnly {
		t.Fatalf("FAIL: proof file did not specify that proofs establish credential validity only, not proxy reachability")
	}
	record := file.Slots[filepath.Base(fixture.target(unreachable))]
	if record.ProvesProxyReachability() {
		t.Fatalf("FAIL: proof record claimed proxy reachability when proxy was never traversed")
	}

	// Warm start adopts the slot from disk without live proof and without testing the proxy.
	fixture.resetProverCounts()
	result := fixture.run(legacyImportHooks{})
	if public, refresh := fixture.proverCounts(); public != 0 || refresh != 0 {
		t.Fatalf("warm start dialed live proofs: public=%d refresh=%d", public, refresh)
	}
	if slot := fixture.journal().Slots[unreachable.Key()]; slot.State != "imported" || slot.Proof != legacyImportStoredProof {
		t.Fatalf("unreachable proxy slot was not adopted on credential proof: %+v", slot)
	}
	if len(result.mintSlots) != 0 {
		t.Fatalf("unreachable proxy unexpectedly minted: %v", result.mintSlots)
	}
}

// A rejection observed at USE time (e.g. AuthLogout) must be recorded in the
// proof store immediately, not ignored until restart.
func TestProofStoreUseTimeRejectionRecorded(t *testing.T) {
	dir := t.TempDir()
	store := loadLegacyImportProofStore(dir)
	claim := legacyImportProofClaim{
		ClientID:  connect.NewId().String(),
		NetworkID: connect.NewId().String(),
		KeyFP:     legacyImportFingerprint([]byte("test-key-material-0123456789012345")),
		TokenFP:   legacyImportFingerprint([]byte("test-token-jwt-material-0123456789")),
	}
	key := "use-time-rejected.jwt"
	store.record(key, claim, "server_key")
	if err := store.flush(); err != nil {
		t.Fatal(err)
	}

	clientPath := filepath.Join(dir, key)
	if err := os.WriteFile(clientPath, []byte("dummy-token"), 0600); err != nil {
		t.Fatal(err)
	}

	callbacks := &providerAuthenticationCallbacks{
		clientJwtPath: clientPath,
		cancel:        func() {},
	}
	callbacks.AuthLogout()

	reloaded := loadLegacyImportProofStore(dir)
	if !reloaded.rejected(key) {
		t.Fatalf("FAIL: use-time rejection via AuthLogout was not recorded in proof store: %+v", reloaded.slots[key])
	}
}

// A transient failure (such as proof_unavailable_bound from an outage or 429 storm)
// must not retire a good identity permanently: after a cooldown, the slot must have
// an automatic way back and be re-admitted for proof.
func TestProofTransientFallbackReAdmittedAfterCooldown(t *testing.T) {
	oldCooldown := legacyImportTransientFallbackCooldown
	legacyImportTransientFallbackCooldown = 0
	defer func() { legacyImportTransientFallbackCooldown = oldCooldown }()

	oldBackoff := legacyImportTransientBackoff
	legacyImportTransientBackoff = 0
	defer func() { legacyImportTransientBackoff = oldBackoff }()

	fixture := newLegacyImportFixture(t, true, 2)
	stuck := fixture.proxySettings[0]
	companion := fixture.proxySettings[1]

	store, err := readLegacyStoreRaw(fixture)
	if err != nil {
		t.Fatal(err)
	}

	// Stuck slot gets a transient timeout; companion succeeds so API answered.
	delete(fixture.prover.serviceKey, fixture.proxyClientIDs[stuck.Key()])
	fixture.prover.refreshErr[store[stuck.Key()].ByClientJWT] = &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ETIMEDOUT}

	// Run until stuck slot reaches proof_unavailable_bound.
	for start := 1; start <= legacyImportDeferralStarts; start++ {
		journal := fixture.journal()
		delete(journal.Slots, companion.Key())
		_ = os.Remove(fixture.target(companion))
		_ = os.Remove(filepath.Join(fixture.dir, legacyImportProofStoreName))
		if err := writeLegacyImportJournal(fixture.dir, journal); err != nil {
			t.Fatal(err)
		}
		_ = fixture.run(legacyImportHooks{})
	}

	recorded := fixture.journal().Slots[stuck.Key()]
	if recorded.State != "fallback" || recorded.Reason != "proof_unavailable_bound" {
		t.Fatalf("expected proof_unavailable_bound, got %+v", recorded)
	}

	// Now the transient outage heals: refresh succeeds.
	delete(fixture.prover.refreshErr, store[stuck.Key()].ByClientJWT)
	fixture.resetProverCounts()

	// On the next start (after cooldown), the slot must be re-admitted and proven!
	laterResult := fixture.run(legacyImportHooks{})
	if _, refresh := fixture.proverCounts(); refresh == 0 {
		t.Fatalf("FAIL: transient failure was permanently discarded; want slot re-admitted and proven after cooldown")
	}
	afterSlot := fixture.journal().Slots[stuck.Key()]
	if afterSlot.State != "imported" {
		t.Fatalf("FAIL: slot was not imported after healing: %+v (mintSlots=%v)", afterSlot, laterResult.mintSlots)
	}
}

// Entries for proxies that no longer exist and expired records must be pruned,
// and the store file must stay bounded.
func TestProofStorePruningAndBound(t *testing.T) {
	dir := t.TempDir()
	store := loadLegacyImportProofStore(dir)
	claim := legacyImportProofClaim{
		ClientID:  connect.NewId().String(),
		NetworkID: connect.NewId().String(),
		KeyFP:     legacyImportFingerprint([]byte("test-key-material-0123456789012345")),
		TokenFP:   legacyImportFingerprint([]byte("test-token-jwt-material-0123456789")),
	}

	activeKey := "active-proxy.jwt"
	removedKey := "removed-proxy.jwt"
	expiredKey := "expired-proxy.jwt"

	store.record(activeKey, claim, "server_key")
	store.record(removedKey, claim, "server_key")
	store.record(expiredKey, claim, "server_key")

	// Make expiredKey older than max age.
	store.mu.Lock()
	rec := store.slots[expiredKey]
	rec.ProvenAt = store.now().Add(-legacyImportProofMaxAge - 24*time.Hour).Unix()
	store.slots[expiredKey] = rec
	store.mu.Unlock()

	// Active proxies only include activeKey.
	store.setActiveKeys(map[string]bool{activeKey: true})

	if err := store.flush(); err != nil {
		t.Fatal(err)
	}

	reloaded := loadLegacyImportProofStore(dir)
	if _, ok := reloaded.slots[removedKey]; ok {
		t.Fatalf("FAIL: proof store retained entries for removed proxies; want pruned: %+v", reloaded.slots[removedKey])
	}
	if _, ok := reloaded.slots[expiredKey]; ok {
		t.Fatalf("FAIL: proof store retained expired entries; want pruned: %+v", reloaded.slots[expiredKey])
	}
	if _, ok := reloaded.slots[activeKey]; !ok {
		t.Fatalf("FAIL: active proxy entry was unexpectedly pruned")
	}
}

// When entries exceed the maximum allowed capacity, the oldest records are pruned
// so the store file stays strictly bounded.
func TestProofStoreEntryBound(t *testing.T) {
	dir := t.TempDir()
	store := loadLegacyImportProofStore(dir)
	claim := legacyImportProofClaim{
		ClientID:  connect.NewId().String(),
		NetworkID: connect.NewId().String(),
		KeyFP:     legacyImportFingerprint([]byte("test-key-material-0123456789012345")),
		TokenFP:   legacyImportFingerprint([]byte("test-token-jwt-material-0123456789")),
	}

	const total = legacyImportProofStoreMaxEntries + 50
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("proxy-%d.jwt", i)
		store.record(key, claim, "server_key")
	}

	if err := store.flush(); err != nil {
		t.Fatal(err)
	}

	reloaded := loadLegacyImportProofStore(dir)
	if len(reloaded.slots) > legacyImportProofStoreMaxEntries {
		t.Fatalf("FAIL: proof store entry count %d exceeds max %d", len(reloaded.slots), legacyImportProofStoreMaxEntries)
	}
}







