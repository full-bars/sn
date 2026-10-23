package miner

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The startup disk audit's only outward surface in the miner is the metrics
// seam plus the cache file it records. These tests drive that contract without
// executing a benchmark: the cache fixture is written by hand, so a passing
// run proves the seam reads what the audit recorded rather than what this
// test process happened to measure.

// diskBenchCacheFixture is the on-disk shape the audit writes. It is spelled
// out here instead of reusing the reader's struct so the test cannot pass by
// agreeing with an implementation change to the format: a renamed or
// retyped field would leave the seam with nothing to report.
type diskBenchCacheFixture struct {
	At     time.Time `json:"at"`
	Device uint64    `json:"device"`
	Result struct {
		MBps     float64 `json:"mbps"`
		Bytes    int64   `json:"bytes"`
		Slow     bool    `json:"slow"`
		TimedOut bool    `json:"timed_out,omitempty"`
		Err      string  `json:"err,omitempty"`
	} `json:"result"`
}

func writeDiskBenchCacheFixture(t *testing.T, dir string, mbps float64, slow bool, at time.Time) {
	t.Helper()
	var c diskBenchCacheFixture
	c.At = at
	c.Device = 2049
	c.Result.MBps = mbps
	c.Result.Bytes = 4 << 20
	c.Result.Slow = slow
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal cache fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk-bench.json"), b, 0600); err != nil {
		t.Fatalf("write cache fixture: %v", err)
	}
}

// TestStartupDiskBenchmarkSeamIsInstalled is the port's red test: before the
// audit lands the miner leaves metricsSeamDiskBench nil, so the scrape omits
// urnet_disk_write_mbps, urnet_disk_slow and urnet_disk_bench_age_seconds.
func TestStartupDiskBenchmarkSeamIsInstalled(t *testing.T) {
	if metricsSeamDiskBench == nil {
		t.Fatal("metricsSeamDiskBench is nil: the startup disk audit is not installed in the miner")
	}
}

func TestStartupDiskBenchmarkSeamReportsTheRecordedResult(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	writeDiskBenchCacheFixture(t, dir, 3.2, true, time.Now().Add(-90*time.Second))
	if metricsSeamDiskBench == nil {
		t.Fatal("metricsSeamDiskBench is nil: the startup disk audit is not installed in the miner")
	}

	d := metricsSeamDiskBench()
	if d == nil {
		t.Fatal("seam returned nil although a benchmark result is recorded under the state dir")
	}
	if !d.Slow {
		t.Error("seam lost the slow verdict of the recorded result")
	}
	if math.Abs(d.WriteMBps-3.2) > 0.05 {
		t.Errorf("WriteMBps = %.2f, want 3.2", d.WriteMBps)
	}
	if d.AgeSeconds < 85 || d.AgeSeconds > 100 {
		t.Errorf("AgeSeconds = %d, want the age of a result recorded 90 s ago", d.AgeSeconds)
	}
}

func TestStartupDiskBenchmarkSeamIsSilentWithoutAResult(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	if metricsSeamDiskBench == nil {
		t.Fatal("metricsSeamDiskBench is nil: the startup disk audit is not installed in the miner")
	}
	if d := metricsSeamDiskBench(); d != nil {
		t.Fatalf("seam reported a reading with no benchmark recorded: %+v", d)
	}
}

// The three families must reach the scrape through the existing seam writer;
// a seam that reports a reading is useless if minerExtraMetrics never calls it.
func TestScrapeEmitsTheDiskBenchmarkFamilies(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	writeDiskBenchCacheFixture(t, dir, 12.5, false, time.Now())
	withMetricsState(t)

	body := minerExtraMetrics()
	for _, want := range []string{
		"urnet_disk_write_mbps",
		"urnet_disk_slow",
		"urnet_disk_bench_age_seconds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing %s", want)
		}
	}
	for _, err := range Lint(body) {
		t.Error(err)
	}
}
