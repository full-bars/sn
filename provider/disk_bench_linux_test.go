//go:build linux

package provider

import (
	"os"
	"strings"
	"testing"
)

func stateDirEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", dir)
	t.Setenv("URNETWORK_SKIP_AUDIT", "")
	t.Setenv("URNETWORK_RAMLOGS", "")
	t.Setenv("URNETWORK_PROFILE", "")
	return dir
}

func benchArtifacts(dir string) []string {
	var out []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".io-test") || e.Name() == "disk-bench.json" {
			out = append(out, e.Name())
		}
	}
	return out
}

// The benchmark touches the disk, so it must only run when its answer is used.
func TestStartupAuditDoesNotBenchmarkUnlessAsked(t *testing.T) {
	dir := stateDirEnv(t)
	RunStartupAudit(false)
	if got := benchArtifacts(dir); len(got) != 0 {
		t.Fatalf("benchmark ran although nobody asked for it: %v", got)
	}
}

func TestStartupAuditSkipsBenchmarkWhenRamLogsAlreadyOn(t *testing.T) {
	dir := stateDirEnv(t)
	t.Setenv("URNETWORK_RAMLOGS", "1")
	RunStartupAudit(true)
	if got := benchArtifacts(dir); len(got) != 0 {
		t.Fatalf("benchmark ran with RAM logs already on: %v", got)
	}
}

func TestStartupAuditSkipsBenchmarkForLowmemAndEco(t *testing.T) {
	for _, prof := range []string{"lowmem", "eco"} {
		dir := stateDirEnv(t)
		t.Setenv("URNETWORK_PROFILE", prof)
		RunStartupAudit(true)
		if got := benchArtifacts(dir); len(got) != 0 {
			t.Fatalf("%s: benchmark ran: %v", prof, got)
		}
	}
}

func TestStartupAuditHonoursSkipAuditEnv(t *testing.T) {
	dir := stateDirEnv(t)
	t.Setenv("URNETWORK_SKIP_AUDIT", "1")
	RunStartupAudit(true)
	if got := benchArtifacts(dir); len(got) != 0 {
		t.Fatalf("benchmark ran although URNETWORK_SKIP_AUDIT=1: %v", got)
	}
}

func TestStartupAuditBenchmarkWritesOnlyTheCacheAndMetricsShowIt(t *testing.T) {
	dir := stateDirEnv(t)
	t.Setenv("URNETWORK_DISK_BENCH", "1") // run even if the test host is itself a container
	if why := DiskBenchSkipReason(); why != "" {
		t.Skipf("benchmark cannot run on this host right now: %s", why)
	}
	cwd := t.TempDir()
	t.Chdir(cwd)
	RunStartupAudit(true)
	got := benchArtifacts(dir)
	if len(got) != 1 || got[0] != "disk-bench.json" {
		t.Fatalf("after a benchmark only disk-bench.json may remain in the state dir, got %v", got)
	}
	if left, _ := os.ReadDir(cwd); len(left) != 0 {
		t.Fatalf("working dir touched: %v", left)
	}
	var b strings.Builder
	writeDiskBenchMetrics(&b)
	for _, want := range []string{"urnet_disk_write_mbps ", "urnet_disk_slow ", "urnet_disk_bench_age_seconds "} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("metrics missing %q:\n%s", want, b.String())
		}
	}
}

func TestDiskBenchMetricsAbsentUntilABenchmarkRan(t *testing.T) {
	stateDirEnv(t)
	var b strings.Builder
	writeDiskBenchMetrics(&b)
	if b.Len() != 0 {
		t.Fatalf("metrics emitted with no benchmark recorded:\n%s", b.String())
	}
}
