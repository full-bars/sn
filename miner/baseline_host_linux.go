//go:build linux

package miner

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/urfoundation/sn/internal/connectx"
)

// baseline_host_linux.go is the Linux half of the host readers: PSI, available
// memory and load. The non-Linux stub returns errNoHostReadings so a sample on
// another platform omits these fields instead of writing zeros.

func readPSI(resource string) (avg60 float64, err error) {
	b, err := os.ReadFile("/proc/pressure/" + resource)
	if err != nil {
		return 0, err
	}
	_, avg60, err = parsePSISome(string(b))
	return avg60, err
}

// parsePSISome extracts avg10 and avg60 from the "some" line of the PSI file.
func parsePSISome(content string) (avg10, avg60 float64, err error) {
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, field := range strings.Fields(line)[1:] {
			k, v, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch k {
			case "avg10":
				avg10, err = strconv.ParseFloat(v, 64)
			case "avg60":
				avg60, err = strconv.ParseFloat(v, 64)
			}
			if err != nil {
				return 0, 0, err
			}
		}
		return avg10, avg60, nil
	}
	return 0, 0, fmt.Errorf("no 'some' line in PSI content")
}

func readMemAvailableMiB() int64 {
	v, ok := connectx.HostMemoryAvailableBytes()
	if !ok {
		return -1
	}
	return v / 1024 / 1024
}

// readCgroupAvailableMiB returns the free headroom within the active cgroup
// memory limit in MiB, or -1 if no cgroup limit is set. This is necessary
// inside Docker containers, where /proc/meminfo MemAvailable reflects host RAM,
// not the container limit.
func readCgroupAvailableMiB() int64 {
	const oneTiB = int64(1) << 40

	// cgroup v2: the process's own cgroup and its ancestors (systemd MemoryMax=
	// and MemoryHigh= live there, not at the mount root, which only a
	// container's own cgroup makes meaningful).
	if room, ok := connectx.CgroupMemoryHeadroomBytes(); ok && room < oneTiB {
		return room / 1024 / 1024
	}

	// cgroup v1
	limitData, limitErr := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")
	usageData, usageErr := os.ReadFile("/sys/fs/cgroup/memory/memory.usage_in_bytes")
	if limitErr == nil && usageErr == nil {
		limit, err1 := strconv.ParseInt(strings.TrimSpace(string(limitData)), 10, 64)
		usage, err2 := strconv.ParseInt(strings.TrimSpace(string(usageData)), 10, 64)
		if err1 == nil && err2 == nil && limit > 0 && limit < oneTiB {
			if avail := (limit - usage) / 1024 / 1024; avail >= 0 {
				return avail
			}
			return 0
		}
	}

	return -1
}

// getSystemLoad reads /proc/loadavg and returns the 1-minute and 5-minute load
// averages.
func getSystemLoad() (load1, load5 float64, err error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(string(data))
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("unexpected /proc/loadavg format: %q", string(data))
	}
	load1, err = strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, 0, err
	}
	load5, err = strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0, 0, err
	}
	return load1, load5, nil
}
