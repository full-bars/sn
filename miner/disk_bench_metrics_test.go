package miner

import "testing"

// TestStartupDiskBenchmarkSeamIsInstalled is the port's red test: before the
// audit lands the miner leaves metricsSeamDiskBench nil, so the scrape omits
// urnet_disk_write_mbps, urnet_disk_slow and urnet_disk_bench_age_seconds.
//
// The hook is installed on every platform (a nil reading on the ones without a
// Linux statfs), so this assertion is portable; what the reading contains is
// covered by the Linux tests beside the audit.
func TestStartupDiskBenchmarkSeamIsInstalled(t *testing.T) {
	if metricsSeamDiskBench == nil {
		t.Fatal("metricsSeamDiskBench is nil: the startup disk audit is not installed in the miner")
	}
}
