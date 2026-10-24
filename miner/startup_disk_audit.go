package miner

import (
	"fmt"
	"os"
	"sync"
)

// startup_disk_audit.go is the miner's startup entry point for the ported disk
// audit (system_audit_disk_linux.go). It mirrors the relay's RunStartupAudit
// decision for the disk half only: the auto profile asks for the write
// benchmark, every other profile and a process whose logs already live in RAM
// get the free-space check alone, because neither would act on a speed result.
//
// The audit runs in its own goroutine: the work is bounded by time (3 s, plus a
// short grace for a write stuck in the kernel) with a 256 MiB ceiling that only a
// very fast device reaches, and startup must not wait even for that. The result lands in the state dir as disk-bench.json,
// which is what metricsSeamDiskBench reads on the next scrape, so /metrics
// reports the audit without ever triggering it.
//
// It is started once from the provide path (miner/run.go, immediately after the
// providerRunSettings value is built and before settings.run runs the
// providers), which is the point where the state directory is known and startup
// has not yet begun serving.
var startupDiskAuditOnce sync.Once

// startStartupDiskAudit starts the startup disk audit at most once per process,
// without blocking the caller. A second call is a no-op, which is what a
// repeated startup hook (or a hot-restart path that runs it again) needs.
func startStartupDiskAudit() {
	startupDiskAuditOnce.Do(func() {
		dir, err := providerStateDir()
		if err != nil || dir == "" {
			return
		}
		benchmark := startupDiskBenchmarkWanted()
		go func() {
			runStartupDiskAudit(dir, benchmark)
		}()
	})
}

// startupDiskBenchmarkWanted reports whether the write benchmark may run. Only
// the auto profile acts on its result, and a process whose logs are already in
// RAM cannot change its mind: initGlog ran before provide(), so on lowmem, eco
// and URNETWORK_RAMLOGS=1 the write would buy nothing.
func startupDiskBenchmarkWanted() bool {
	return profileBenchmarksDisk(effectiveProfile()) && !ramlogsRequested()
}

// runStartupDiskAudit is the disk half of the relay's RunStartupAudit, split out
// so it can be driven synchronously by tests. An empty dir means no benchmark
// and no probe: the audit never falls back to the working directory.
func runStartupDiskAudit(dir string, benchmarkDisk bool) (slowDisk bool, lowSpace bool) {
	if os.Getenv("URNETWORK_SKIP_AUDIT") == "1" {
		fmt.Printf("[audit] System audit skipped (URNETWORK_SKIP_AUDIT=1)\n")
		return false, false
	}
	if dir == "" {
		return false, false
	}
	if !benchmarkDisk {
		_, lowSpace = CheckDiskSpace(dir)
		return false, lowSpace
	}
	return RunDiskAudit(dir)
}
