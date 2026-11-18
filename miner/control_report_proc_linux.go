//go:build linux

package miner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var minerProcResources struct {
	sync.Mutex
	at                                     time.Time
	threads, tasksMax, swapUsed, swapTotal *uint64
}

// collectMinerPlatformResources caches proc and cgroup reads for one second.
// A failed or unavailable reading stays absent on the wire.
func collectMinerPlatformResources(r *snapshotResources) {
	minerProcResources.Lock()
	defer minerProcResources.Unlock()
	now := time.Now()
	if minerProcResources.at.IsZero() || now.Before(minerProcResources.at) || now.Sub(minerProcResources.at) >= time.Second {
		status, _ := os.ReadFile("/proc/self/status")
		minerProcResources.threads = minerProcField(status, "Threads:", 1)
		cgroups, _ := os.ReadFile("/proc/self/cgroup")
		minerProcResources.tasksMax = minerPidsLimit(cgroups, []string{"/sys/fs/cgroup", "/sys/fs/cgroup/pids"})
		mem, _ := os.ReadFile("/proc/meminfo")
		minerProcResources.swapUsed, minerProcResources.swapTotal = minerSwap(mem)
		minerProcResources.at = now
	}
	r.Threads, r.TasksMax = minerProcResources.threads, minerProcResources.tasksMax
	r.SwapUsedBytes, r.SwapTotalBytes = minerProcResources.swapUsed, minerProcResources.swapTotal
}

func minerProcField(data []byte, name string, scale uint64) *uint64 {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != name {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || scale == 0 || v > ^uint64(0)/scale {
			return nil
		}
		v *= scale
		return &v
	}
	return nil
}

func minerSwap(mem []byte) (used, total *uint64) {
	total = minerProcField(mem, "SwapTotal:", 1024)
	free := minerProcField(mem, "SwapFree:", 1024)
	if total == nil || free == nil || *free > *total {
		return nil, nil
	}
	v := *total - *free
	return &v, total
}

// minerPidsLimit finds the tightest finite limit in the process's cgroup and ancestors.
func minerPidsLimit(cgroups []byte, roots []string) *uint64 {
	var limit *uint64
	for _, line := range strings.Split(string(cgroups), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || (parts[1] != "" && !strings.Contains(","+parts[1]+",", ",pids,")) || parts[2] == "" {
			continue
		}
		group := strings.TrimPrefix(filepath.Clean("/"+parts[2]), "/")
		for _, root := range roots {
			root = filepath.Clean(root)
			for dir := filepath.Join(root, group); ; dir = filepath.Dir(dir) {
				if raw, err := os.ReadFile(filepath.Join(dir, "pids.max")); err == nil {
					if v, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err == nil && v > 0 && (limit == nil || v < *limit) {
						value := v
						limit = &value
					}
				}
				if dir == root {
					break
				}
			}
		}
	}
	return limit
}
