package miner

// disk_bench_metrics.go installs the startup disk benchmark seam:
// urnet_disk_write_mbps, urnet_disk_slow and urnet_disk_bench_age_seconds.
//
// The reader is the state dir's disk-bench.json, the file the ported audit
// records and the relay's own audit writes too, so a scrape never triggers a
// measurement: it reports the last one and its age, and stays silent until one
// exists. minerExtraMetrics omits a family whose hook is nil; the hook itself is
// never nil here, and the reading is nil instead.
func init() {
	metricsSeamDiskBench = diskBenchSeamReading
}

// diskBenchSeamReading is the seam payload, or nil when no benchmark result has
// been recorded for the miner's state dir.
func diskBenchSeamReading() *seamDiskBench {
	dir, err := providerStateDir()
	if err != nil || dir == "" {
		return nil
	}
	mbps, slow, age, ok := DiskBenchStatus(dir)
	if !ok {
		return nil
	}
	return &seamDiskBench{
		WriteMBps:  mbps,
		Slow:       slow,
		AgeSeconds: int64(age.Seconds()),
	}
}
