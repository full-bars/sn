//go:build linux

package miner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// thrash_sensors_linux.go is the Linux half of the swap-thrash sensing: PSI
// memory (this cgroup's own memory.pressure, else /proc/pressure/memory),
// swap activity (this cgroup's memory.stat pswpin/pswpout when the kernel has
// them, else host /proc/vmstat), this cgroup's swap level, and the host swap
// and RAM levels from /proc/meminfo.
//
// Each source is best-effort and reports its own availability: a missing
// source is "unavailable", never a zero.

// swapThrashCgroupDir resolves this process's own cgroup v2 directory from
// the "0::" line of /proc/self/cgroup, rooted at /sys/fs/cgroup. PSI, the
// swap counters and the swap level are read from the OWN cgroup: they
// describe exactly this process's subtree. Empty when unavailable.
func swapThrashCgroupDir() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		rel := strings.TrimSpace(strings.TrimPrefix(line, "0::"))
		if rel == "" {
			rel = "/"
		}
		return filepath.Join("/sys/fs/cgroup", rel)
	}
	return ""
}

// readSwapThrash assembles one reading. Unit-level sources are preferred
// (they describe this process); host sources are the fallback and are what
// makes detection work on kernels without unit swap counters.
func readSwapThrash() swapThrashRead {
	var rd swapThrashRead
	unitDir := swapThrashCgroupDir()

	// PSI memory: prefer the unit's memory.pressure PER LINE, fall back to
	// /proc/pressure/memory per line. A unit file lacking only "full" must not
	// suppress a readable host "full" (severe detection depends on full).
	if unitDir != "" {
		if b, err := os.ReadFile(filepath.Join(unitDir, "memory.pressure")); err == nil {
			some, full, someOK, fullOK := parseSwapThrashPSI(string(b))
			if someOK {
				rd.psiSomeTotal, rd.psiSomeOK, rd.psiSomeUnit = some, true, true
			}
			if fullOK {
				rd.psiFullTotal, rd.psiFullOK, rd.psiUnit = full, true, true
			}
		}
	}
	if !rd.psiSomeOK || !rd.psiFullOK {
		if b, err := os.ReadFile("/proc/pressure/memory"); err == nil {
			some, full, someOK, fullOK := parseSwapThrashPSI(string(b))
			if someOK && !rd.psiSomeOK {
				rd.psiSomeTotal, rd.psiSomeOK = some, true
			}
			if fullOK && !rd.psiFullOK {
				rd.psiFullTotal, rd.psiFullOK = full, true
			}
		}
	}

	// Unit memory.stat: swap counters (kernel-dependent), refaults, direct
	// reclaim. One read serves all three.
	var memstat string
	var haveMemstat bool
	if unitDir != "" {
		if b, err := os.ReadFile(filepath.Join(unitDir, "memory.stat")); err == nil {
			memstat, haveMemstat = string(b), true
		}
	}

	// Swap activity: unit counters when the kernel has them, else host
	// /proc/vmstat. Never a fake zero.
	if haveMemstat {
		in, inOK := parseSwapThrashCounter(memstat, "pswpin")
		out, outOK := parseSwapThrashCounter(memstat, "pswpout")
		if inOK && outOK {
			rd.swapIn, rd.swapOut, rd.swapOK, rd.swapUnit = in, out, true, true
		}
	}
	var vmstat string
	var haveVmstat bool
	if !rd.swapOK {
		if b, err := os.ReadFile("/proc/vmstat"); err == nil {
			vmstat, haveVmstat = string(b), true
			if in, out, ok := parseSwapThrashVMStatSwap(vmstat); ok {
				rd.swapIn, rd.swapOut, rd.swapOK = in, out, true
			}
		}
	}

	// Refaults: our own anonymous pages read back in, direct thrash evidence.
	if haveMemstat {
		if v, ok := parseSwapThrashCounter(memstat, "workingset_refault_anon"); ok {
			rd.refault, rd.refaultOK = v, true
		}
	}

	// Direct reclaim: unit pgscan_direct, else host vmstat.
	if haveMemstat {
		if v, ok := parseSwapThrashCounter(memstat, "pgscan_direct"); ok {
			rd.pgscan, rd.pgscanOK, rd.pgscanUnit = v, true, true
		}
	}
	if !rd.pgscanOK {
		if !haveVmstat {
			if b, err := os.ReadFile("/proc/vmstat"); err == nil {
				vmstat, haveVmstat = string(b), true
			}
		}
		if haveVmstat {
			if v, ok := parseSwapThrashCounter(vmstat, "pgscan_direct"); ok {
				rd.pgscan, rd.pgscanOK = v, true
			}
		}
	}

	// Unit swap LEVEL: is this process the swapped-out one?
	if unitDir != "" {
		if b, err := os.ReadFile(filepath.Join(unitDir, "memory.swap.current")); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && n >= 0 {
				rd.unitSwapMiB, rd.unitSwapOK = n>>20, true
			}
		}
	}

	// Host swap and RAM levels, one meminfo read.
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		meminfo := string(b)
		if total, free, ok := parseSwapThrashMeminfoSwap(meminfo); ok {
			rd.hostSwapTotalMiB, rd.hostSwapOK = total, true
			rd.hostSwapUsedMiB = total - free
		}
		if v, ok := parseSwapThrashMeminfoValue(meminfo, "MemAvailable"); ok {
			rd.ramAvailMiB, rd.ramAvailOK = v, true
		}
		if v, ok := parseSwapThrashMeminfoValue(meminfo, "MemTotal"); ok {
			rd.ramTotalMiB, rd.ramTotalOK = v, true
		}
	}

	rd.heapFrac, rd.heapUsedMiB, rd.heapLimitMiB, rd.heapOK = swapThrashHeapReading()
	return rd
}
