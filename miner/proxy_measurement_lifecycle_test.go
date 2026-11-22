package miner

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// proxy_measurement_lifecycle_test.go covers the run hooks: a start loads the
// durable verdicts and starts the sweep, a clean stop writes what the run holds.
// The sweep's interval is raised for the test so no tick can reach a probe.

func clearGlobalMeasurements(t *testing.T) {
	t.Helper()
	globalProxyUDPCheck.stopMeasurementPersistence()
	globalProxyUDPCheck.mu.Lock()
	globalProxyUDPCheck.entries = map[string]*proxyUDPCheckEntry{}
	globalProxyUDPCheck.measurements = nil
	globalProxyUDPCheck.attempts = nil
	globalProxyUDPCheck.mu.Unlock()
}

func TestControlRunLoadsAndWritesTheMeasurementSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	savedInterval := measurementSweepInterval
	measurementSweepInterval = time.Hour
	defer func() { measurementSweepInterval = savedInterval }()
	resetControlRuntimeForTest()
	clearGlobalMeasurements(t)
	defer func() {
		resetControlRuntimeForTest()
		clearGlobalMeasurements(t)
	}()

	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "udp-measurements.json")
	known := measurementTestSettings("192.0.2.60:1080")
	entry := measurementEntryFor(known.Key(), proxyUDPResult{Status: proxyUDPOK, UDPExit: "198.51.100.10"}, now)
	if err := saveMeasurementFile(path, []measurementFileEntry{entry}); err != nil {
		t.Fatal(err)
	}

	beginControlRun(now)
	restored, ok := globalProxyUDPCheck.cached(known.Key())
	if !ok || restored.Status != proxyUDPOK || restored.UDPExit != "198.51.100.10" {
		t.Fatalf("the run did not load the snapshot: %#v (%t)", restored, ok)
	}

	// a verdict published during the run is written by the stop, not lost
	learned := measurementTestSettings("192.0.2.61:1080")
	globalProxyUDPCheck.mu.Lock()
	globalProxyUDPCheck.persistMeasurementLocked(learned.Key(), proxyUDPResult{Status: proxyUDPNoRelay}, now.Add(time.Minute))
	globalProxyUDPCheck.mu.Unlock()

	finishControlRun("clean")
	entries, err := loadMeasurementFile(path)
	if err != nil {
		t.Fatalf("stop left an unreadable snapshot: %v", err)
	}
	byKey := map[string]measurementFileEntry{}
	for _, saved := range entries {
		byKey[saved.Key] = saved
	}
	if _, ok := byKey[measurementKey(learned.Key())]; !ok {
		t.Fatalf("the stop did not write the verdict learned during the run: %#v", entries)
	}
	if controlRuntime.measurementsStop != nil {
		t.Fatal("the sweep was left running after the stop")
	}
}

func TestControlRunRefusesAnotherVersionsSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	savedInterval := measurementSweepInterval
	measurementSweepInterval = time.Hour
	defer func() { measurementSweepInterval = savedInterval }()
	resetControlRuntimeForTest()
	clearGlobalMeasurements(t)
	defer func() {
		resetControlRuntimeForTest()
		clearGlobalMeasurements(t)
	}()

	path := filepath.Join(dir, "udp-measurements.json")
	if err := os.WriteFile(path, []byte(`{"version":9,"entries":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	beginControlRun(now)
	if _, ok := globalProxyUDPCheck.cached(measurementTestSettings("192.0.2.62:1080").Key()); ok {
		t.Fatal("a snapshot from another version was published")
	}
	globalProxyUDPCheck.mu.Lock()
	published := len(globalProxyUDPCheck.measurements)
	globalProxyUDPCheck.mu.Unlock()
	if published != 0 {
		t.Fatalf("the rejected snapshot published %d records", published)
	}

	// persistence stays on: the run replaces the foreign file with its own view
	globalProxyUDPCheck.mu.Lock()
	globalProxyUDPCheck.persistMeasurementLocked(measurementKeysForTest("192.0.2.63:1080"), proxyUDPResult{Status: proxyUDPOK}, now)
	globalProxyUDPCheck.mu.Unlock()
	finishControlRun("clean")
	entries, err := loadMeasurementFile(path)
	if err != nil {
		t.Fatalf("the run did not replace a foreign snapshot: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the replacement snapshot holds %d entries", len(entries))
	}
}

func measurementKeysForTest(address string) string {
	return measurementTestSettings(address).Key()
}
