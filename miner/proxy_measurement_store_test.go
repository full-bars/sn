package miner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// proxy_measurement_store_test.go covers the durable snapshot of published UDP
// verdicts: what a restart keeps, what it refuses, and how a cancelled check
// leaves the last verdict alone. Every case drives the store with a fake probe
// and an explicit clock, so none of them sleeps or touches a network.

func measurementTestSettings(address string) *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: address}
}

func TestMeasurementStoreRestoresVerdictAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "udp-measurements.json")
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	settings := measurementTestSettings("192.0.2.10:1080")

	first := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{Status: proxyUDPOK, TCPExit: "198.51.100.1", UDPExit: "198.51.100.1"}
	})
	first.enableMeasurementPersistence(path, now)
	first.Check(context.Background(), settings, now)
	first.flushMeasurements()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("snapshot is empty after a published check")
	}

	restarted := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		t.Fatal("a restored verdict was checked again")
		return proxyUDPResult{}
	})
	restarted.enableMeasurementPersistence(path, now.Add(time.Minute))
	result, ok := restarted.cached(settings.Key())
	if !ok {
		t.Fatal("the verdict did not survive the restart")
	}
	if result.Status != proxyUDPOK || result.TCPExit != "198.51.100.1" || result.UDPExit != "198.51.100.1" {
		t.Fatalf("restored result = %#v", result)
	}
	// the snapshot holds the hash of the identity, never the address
	if bytes.Contains(b, []byte("192.0.2.10")) {
		t.Fatalf("snapshot holds the raw address: %s", b)
	}
}

func TestMeasurementStoreKeepsExclusionAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "udp-measurements.json")
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	settings := measurementTestSettings("192.0.2.11:1080")

	first := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{Status: proxyUDPExitMismatch, TCPExit: "198.51.100.2", UDPExit: "198.51.100.3"}
	})
	first.enableMeasurementPersistence(path, now)
	first.Check(context.Background(), settings, now)
	first.flushMeasurements()

	restarted := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		t.Fatal("a restored exclusion was checked again")
		return proxyUDPResult{}
	})
	restarted.enableMeasurementPersistence(path, now.Add(time.Minute))
	result, ok := restarted.cached(settings.Key())
	if !ok {
		t.Fatal("the exclusion did not survive the restart")
	}
	if proxyUDPAllowsH3(result.Status) {
		t.Fatalf("a restored exit mismatch was admitted: %#v", result)
	}
}

func TestMeasurementStoreIgnoresOtherVersions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "udp-measurements.json")
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(path, []byte(`{"version":2,"entries":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{Status: proxyUDPOK}
	})
	store.enableMeasurementPersistence(path, now)
	if _, ok := store.cached(measurementTestSettings("192.0.2.12:1080").Key()); ok {
		t.Fatal("a file from another version was trusted")
	}
	// the next published result still reaches disk, at this version
	store.Check(context.Background(), measurementTestSettings("192.0.2.12:1080"), now)
	store.flushMeasurements()
	entries, err := loadMeasurementFile(path)
	if err != nil {
		t.Fatalf("rewritten snapshot rejected: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("rewritten snapshot holds %d entries", len(entries))
	}
}

func TestMeasurementStoreTreatsFutureStampAsNoEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "udp-measurements.json")
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	settings := measurementTestSettings("192.0.2.13:1080")
	entry := measurementEntryFor(settings.Key(), proxyUDPResult{Status: proxyUDPOK, UDPExit: "198.51.100.4"}, now.Add(time.Hour))
	if err := saveMeasurementFile(path, []measurementFileEntry{entry}); err != nil {
		t.Fatal(err)
	}
	store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{}
	})
	store.enableMeasurementPersistence(path, now)
	if _, ok := store.cached(settings.Key()); ok {
		t.Fatal("a stamp in the future was trusted")
	}
}

func TestMeasurementStorePrunesStaleAndBoundsEntries(t *testing.T) {
	saved := measurementStoreMaxEntries
	measurementStoreMaxEntries = 3
	defer func() { measurementStoreMaxEntries = saved }()

	path := filepath.Join(t.TempDir(), "udp-measurements.json")
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	var entries []measurementFileEntry
	for i := 0; i < 5; i++ {
		settings := measurementTestSettings("192.0.2." + string(rune('a'+i)) + ":1080")
		entries = append(entries, measurementEntryFor(settings.Key(), proxyUDPResult{Status: proxyUDPOK}, now.Add(-time.Duration(i)*time.Minute)))
	}
	stale := measurementEntryFor(measurementTestSettings("192.0.2.99:1080").Key(), proxyUDPResult{Status: proxyUDPOK}, now.Add(-3*proxyUDPResultTTL))
	entries = append(entries, stale)
	if err := saveMeasurementFile(path, entries); err != nil {
		t.Fatal(err)
	}

	store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{}
	})
	store.enableMeasurementPersistence(path, now)
	store.mu.Lock()
	count := len(store.measurements)
	_, keptStale := store.measurements[stale.Key]
	store.mu.Unlock()
	if count != measurementStoreMaxEntries {
		t.Fatalf("restored %d records, want %d", count, measurementStoreMaxEntries)
	}
	if keptStale {
		t.Fatal("a record older than its evidence was kept")
	}
	// the newest record survived the bound
	if _, ok := store.cached(measurementTestSettings("192.0.2.a:1080").Key()); !ok {
		t.Fatal("the newest record was dropped by the bound")
	}
}

func TestMeasurementStoreKeepsLastVerdictWhenCheckCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "udp-measurements.json")
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	settings := measurementTestSettings("192.0.2.14:1080")

	store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{Status: proxyUDPOK, UDPExit: "198.51.100.5"}
	})
	store.enableMeasurementPersistence(path, now)
	store.Check(context.Background(), settings, now)
	store.flushMeasurements()

	// a sweep dispatches and then the process stops waiting on it
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := store.Check(ctx, settings, now.Add(proxyUDPResultTTL))
	if result.Status != proxyUDPUnknown {
		t.Fatalf("a cancelled check published %#v", result)
	}
	store.mu.Lock()
	attempt := store.attemptAtLocked(settings.Key())
	store.mu.Unlock()
	if attempt.IsZero() {
		t.Fatal("a cancelled check lost its dispatch record")
	}
	store.flushMeasurements()
	entries, err := loadMeasurementFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Status != proxyUDPOK || !entries[0].At.Equal(now) {
		t.Fatalf("the cancelled check replaced the last verdict: %#v", entries)
	}
}

func TestMeasurementStoreDoesNotEraseNewerEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "udp-measurements.json")
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	shared := measurementTestSettings("192.0.2.16:1080")

	store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{Status: proxyUDPOK, UDPExit: "198.51.100.7"}
	})
	store.enableMeasurementPersistence(path, now)
	store.Check(context.Background(), shared, now)
	store.flushMeasurements()

	// a successor publishes a newer verdict for the same identity, and one for
	// an identity this store has not seen
	newer := measurementEntryFor(shared.Key(), proxyUDPResult{Status: proxyUDPNoRelay}, now.Add(5*time.Minute))
	other := measurementEntryFor(measurementTestSettings("192.0.2.17:1080").Key(), proxyUDPResult{Status: proxyUDPOK}, now)
	if err := saveMeasurementFile(path, []measurementFileEntry{newer, other}); err != nil {
		t.Fatal(err)
	}

	// the older writer publishes an unrelated result afterwards
	store.Check(context.Background(), measurementTestSettings("192.0.2.18:1080"), now)
	store.flushMeasurements()

	entries, err := loadMeasurementFile(path)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]measurementFileEntry{}
	for _, entry := range entries {
		byKey[entry.Key] = entry
	}
	if got := byKey[newer.Key]; got.Status != proxyUDPNoRelay || !got.At.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("a write overrode newer evidence: %#v", got)
	}
	if _, ok := byKey[other.Key]; !ok {
		t.Fatal("a write dropped evidence it did not produce")
	}
}

func TestMeasurementStoreBoundsTheEntryTable(t *testing.T) {
	saved := measurementStoreMaxEntries
	measurementStoreMaxEntries = 2
	defer func() { measurementStoreMaxEntries = saved }()

	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{Status: proxyUDPOK}
	})
	running := &proxyUDPCheckEntry{done: make(chan struct{})}
	store.mu.Lock()
	store.entries["oldest"] = &proxyUDPCheckEntry{at: now.Add(-3 * time.Minute)}
	store.entries["older"] = &proxyUDPCheckEntry{at: now.Add(-2 * time.Minute)}
	store.entries["newest"] = &proxyUDPCheckEntry{at: now.Add(-time.Minute)}
	store.entries["running"] = running
	store.prune(now)
	_, keptOldest := store.entries["oldest"]
	_, keptNewest := store.entries["newest"]
	_, keptRunning := store.entries["running"]
	count := len(store.entries)
	store.mu.Unlock()
	if count != measurementStoreMaxEntries {
		t.Fatalf("table holds %d entries, want %d", count, measurementStoreMaxEntries)
	}
	if keptOldest {
		t.Fatal("the oldest entry survived the count bound")
	}
	if !keptNewest {
		t.Fatal("the newest entry was evicted")
	}
	if !keptRunning {
		t.Fatal("a check in flight was evicted")
	}
	close(running.done)
}

func TestMeasurementStoreRetriesAFailedWrite(t *testing.T) {
	dir := t.TempDir()
	// a directory in the file's place makes the rename fail
	path := filepath.Join(dir, "udp-measurements.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{Status: proxyUDPOK, UDPExit: "198.51.100.6"}
	})
	store.enableMeasurementPersistence(path, now)
	store.Check(context.Background(), measurementTestSettings("192.0.2.15:1080"), now)
	store.flushMeasurements()
	store.mu.Lock()
	dirty := store.measurementDirty
	kept := len(store.measurements)
	store.mu.Unlock()
	if !dirty {
		t.Fatal("a failed write left the snapshot clean")
	}
	if kept != 1 {
		t.Fatalf("a failed write dropped the record: %d kept", kept)
	}
}
