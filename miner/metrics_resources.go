package miner

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"time"
)

// metrics_resources.go ports the v33 provider's process-resource and
// observation-only resource gauges (provider/node_snapshot_resources.go,
// provider/resource_signals.go) onto the miner:
//
//   - collectMetricsResources reads the same figures the snapshot's resources
//     block uses, for urnet_mem_limit_bytes, urnet_rss_bytes, urnet_open_fds
//     and urnet_fd_limit;
//   - readBaselineGC/readBaselineNet gather the GC and kernel socket-table
//     readings behind urnet_gc_* and urnet_conntrack_used_ratio /
//     urnet_tcp_time_wait / urnet_tcp_orphans. Every reader is best-effort:
//     one that fails omits its field rather than reporting a zero, so a
//     missing series means unknown.

// collectMetricsResources reads the process resource figures for the gauges.
// Fields the platform cannot supply stay zero/absent.
func collectMetricsResources() snapshotResources {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	res := snapshotResources{
		HeapInuseBytes: ms.HeapInuse,
		Goroutines:     runtime.NumGoroutine(),
	}
	// A negative input reads the limit without changing it. MaxInt64 is the
	// runtime's "no limit".
	if lim := debug.SetMemoryLimit(-1); lim != math.MaxInt64 && lim > 0 {
		v := uint64(lim)
		res.MemLimitBytes = &v
	}
	if rss, ok := readProcRSS(); ok {
		res.RSSBytes = &rss
	}
	if open, limit, ok := controlReportOpenFDs(); ok {
		res.OpenFDs = &open
		res.FDLimit = &limit
	}
	return res
}

// baselineGC is the GC governor's visible state. GOGC is the value in force,
// Tightening whether the governor holds it below the baseline (the miner has
// no GC governor yet, so it reads false), and CPUFrac the share of CPU spent
// in GC over the last internals window.
type baselineGC struct {
	GOGC       int64
	Tightening bool
	CPUFrac    *float64
}

// baselineNet is the kernel's socket-table pressure. ConntrackUsedFrac is
// nf_conntrack_count over nf_conntrack_max; TimeWait and Orphans come from the
// TCP line of /proc/net/sockstat.
type baselineNet struct {
	ConntrackUsedFrac *float64
	TimeWait          *int64
	Orphans           *int64
}

// parseSockstat reads the TCP line of /proc/net/sockstat:
//
//	TCP: inuse 29 orphan 3 tw 112 alloc 40 mem 7
//
// Each counter reports its own presence: a line carrying only one of them
// must not turn the other into a zero reading.
func parseSockstat(content string) (timeWait, orphans int64, haveTW, haveOrphan bool) {
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "TCP:") {
			continue
		}
		fields := strings.Fields(line)
		for i := 1; i+1 < len(fields); i++ {
			switch fields[i] {
			case "tw":
				if v, err := strconv.ParseInt(fields[i+1], 10, 64); err == nil {
					timeWait, haveTW = v, true
				}
			case "orphan":
				if v, err := strconv.ParseInt(fields[i+1], 10, 64); err == nil {
					orphans, haveOrphan = v, true
				}
			}
		}
		return timeWait, orphans, haveTW, haveOrphan
	}
	return 0, 0, false, false
}

// conntrackUsedFrac is count/max, unknown when max is not positive. A real
// count of zero is data.
func conntrackUsedFrac(count, max int64) (float64, bool) {
	if max <= 0 || count < 0 {
		return 0, false
	}
	return float64(count) / float64(max), true
}

func readProcInt(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// readBaselineNet gathers the socket-table readings, or nil when none of them
// could be read (non-Linux, or a kernel without conntrack loaded).
func readBaselineNet() *baselineNet {
	var n baselineNet
	any := false
	count, okCount := readProcInt("/proc/sys/net/netfilter/nf_conntrack_count")
	max, okMax := readProcInt("/proc/sys/net/netfilter/nf_conntrack_max")
	if okCount && okMax {
		if f, ok := conntrackUsedFrac(count, max); ok {
			n.ConntrackUsedFrac, any = &f, true
		}
	}
	if b, err := os.ReadFile("/proc/net/sockstat"); err == nil {
		tw, orphans, haveTW, haveOrphan := parseSockstat(string(b))
		if haveTW {
			n.TimeWait, any = &tw, true
		}
		if haveOrphan {
			n.Orphans, any = &orphans, true
		}
	}
	if !any {
		return nil
	}
	return &n
}

// readBaselineGC gathers the GC state. GOGC comes from the non-mutating
// runtime metric; the CPU share is omitted until the internals collector has
// completed a window.
func readBaselineGC(now time.Time) *baselineGC {
	gogc, ok := readGOGCPercent()
	if !ok {
		return nil
	}
	g := &baselineGC{GOGC: int64(gogc)}
	if in := minerNodeInternals.get(now); in != nil && in.IntervalSeconds > 0 {
		f := in.GCCPUFraction
		g.CPUFrac = &f
	}
	return g
}

// readGOGCPercent reads the GOGC value in force from runtime/metrics, the
// same non-mutating read v33's governor uses.
func readGOGCPercent() (int64, bool) {
	samples := []metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(samples)
	if samples[0].Value.Kind() == metrics.KindUint64 {
		return int64(samples[0].Value.Uint64()), true
	}
	return 0, false
}

// writeResourceSignalGauges emits the Prometheus series for the resource
// readings above. A reading that is unknown is left out rather than exported
// as 0, so a missing series means unknown.
func writeResourceSignalGauges(b *strings.Builder) {
	gc := readBaselineGC(time.Now())
	net := readBaselineNet()
	if gc != nil {
		fmt.Fprintf(b, "# HELP urnet_gc_gogc GOGC value currently in force.\n")
		fmt.Fprintf(b, "# TYPE urnet_gc_gogc gauge\n")
		fmt.Fprintf(b, "urnet_gc_gogc %d\n", gc.GOGC)
		tight := 0
		if gc.Tightening {
			tight = 1
		}
		fmt.Fprintf(b, "# HELP urnet_gc_tightening 1 while the GC governor holds GOGC below its baseline.\n")
		fmt.Fprintf(b, "# TYPE urnet_gc_tightening gauge\n")
		fmt.Fprintf(b, "urnet_gc_tightening %d\n", tight)
		if gc.CPUFrac != nil {
			fmt.Fprintf(b, "# HELP urnet_gc_cpu_fraction Share of CPU spent in GC over the last window.\n")
			fmt.Fprintf(b, "# TYPE urnet_gc_cpu_fraction gauge\n")
			fmt.Fprintf(b, "urnet_gc_cpu_fraction %g\n", *gc.CPUFrac)
		}
	}
	if net != nil {
		if net.ConntrackUsedFrac != nil {
			fmt.Fprintf(b, "# HELP urnet_conntrack_used_ratio nf_conntrack_count over nf_conntrack_max (Linux). Overflow drops packets silently.\n")
			fmt.Fprintf(b, "# TYPE urnet_conntrack_used_ratio gauge\n")
			fmt.Fprintf(b, "urnet_conntrack_used_ratio %g\n", *net.ConntrackUsedFrac)
		}
		if net.TimeWait != nil {
			fmt.Fprintf(b, "# HELP urnet_tcp_time_wait TCP sockets in TIME_WAIT (Linux, /proc/net/sockstat).\n")
			fmt.Fprintf(b, "# TYPE urnet_tcp_time_wait gauge\n")
			fmt.Fprintf(b, "urnet_tcp_time_wait %d\n", *net.TimeWait)
		}
		if net.Orphans != nil {
			fmt.Fprintf(b, "# HELP urnet_tcp_orphans Orphaned TCP sockets (Linux, /proc/net/sockstat).\n")
			fmt.Fprintf(b, "# TYPE urnet_tcp_orphans gauge\n")
			fmt.Fprintf(b, "urnet_tcp_orphans %d\n", *net.Orphans)
		}
	}
}
