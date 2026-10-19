package provider

import (
	"fmt"
	"strings"
)

// writeDiskBenchMetrics exposes the last disk write benchmark the startup audit
// recorded (auto profile only; absent until one has run). Slow disks are why
// RAM logs exist, so the number belongs next to the other health metrics.
func writeDiskBenchMetrics(b *strings.Builder) {
	dir, ok := proxyHealthDir()
	if !ok {
		return
	}
	mbps, slow, age, have := DiskBenchStatus(dir)
	if !have {
		return
	}
	s := 0
	if slow {
		s = 1
	}
	fmt.Fprintf(b, "# HELP urnet_disk_write_mbps Synchronous write speed measured by the startup disk benchmark, MB/s.\n# TYPE urnet_disk_write_mbps gauge\nurnet_disk_write_mbps %.1f\n", mbps)
	fmt.Fprintf(b, "# HELP urnet_disk_slow 1 when the last disk benchmark judged the disk slow (under 50 MB/s) or it hung.\n# TYPE urnet_disk_slow gauge\nurnet_disk_slow %d\n", s)
	fmt.Fprintf(b, "# HELP urnet_disk_bench_age_seconds Age of the last disk benchmark result.\n# TYPE urnet_disk_bench_age_seconds gauge\nurnet_disk_bench_age_seconds %d\n", int64(age.Seconds()))
}
