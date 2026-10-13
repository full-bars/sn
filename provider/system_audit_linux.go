//go:build linux

package provider

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Golden Values for a high-performance provider.
// Based on the user's fleet (Arch/Debian) optimization profiles.
const (
	GoldenUlimit           = 1048576
	GoldenConntrackMax     = 1048576 // Scaled dynamically, but this is the "Warning" floor
	GoldenConntrackTimeout = 5400    // 90 minutes
)

// RunSystemAudit checks the host's system limits and logs warnings if they
// are suboptimal for a high-volume provider. This is a passive check that
// does not require root. Returns (slowDisk, lowSpace). stateDir is where the
// optional disk benchmark may write; empty means the benchmark never runs.
func RunSystemAudit(stateDir string, skipDiskTest bool) (slowDisk bool, lowSpace bool) {
	if runtime.GOOS != "linux" {
		return false, false
	}

	// 1. Check File Descriptors (Ulimit)
	var rLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit); err == nil {
		if rLimit.Cur < 65535 {
			fmt.Printf("[audit] File Descriptors: %d (Suboptimal! Target: %d+)\n", rLimit.Cur, GoldenUlimit)
		}
	}

	// 2. Check Conntrack Max
	if max, err := readSysctlInt("/proc/sys/net/netfilter/nf_conntrack_max"); err == nil {
		// Based on fleet observation, even 1GB boxes (texas, losangeles) run 2M entries.
		// We'll set a high target of 2M for all systems.
		target := int64(2097152)

		if int64(max) < target {
			fmt.Printf("[audit] Conntrack Max: %d (Suboptimal! Target: %d)\n", max, target)
		}
	} else if _, err := os.Stat("/proc/sys/net/netfilter"); os.IsNotExist(err) {
		fmt.Printf("[audit] Warning: Conntrack kernel module not found/loaded.\n")
	}

	// 3. Check TCP Established Timeout
	if timeout, err := readSysctlInt("/proc/sys/net/netfilter/nf_conntrack_tcp_timeout_established"); err == nil {
		if timeout > 14400 { // > 4 hours
			fmt.Printf("[audit] Conntrack Timeout: %ds (Suboptimal! Target: %ds)\n", timeout, GoldenConntrackTimeout)
		}
	}

	// 4. Recommendation Hint
	// We check if we are likely in Docker or a binary to tailor the hint
	isDocker := false
	if _, err := os.Stat("/.dockerenv"); err == nil {
		isDocker = true
	}

	suboptimal := false
	if rLimit.Cur < 65535 {
		suboptimal = true
	}
	if max, err := readSysctlInt("/proc/sys/net/netfilter/nf_conntrack_max"); err == nil && int64(max) < 1048576 {
		suboptimal = true
	}
	if timeout, err := readSysctlInt("/proc/sys/net/netfilter/nf_conntrack_tcp_timeout_established"); err == nil && timeout > 14400 {
		suboptimal = true
	}

	if suboptimal {
		if isDocker {
			fmt.Printf("[audit] Hint: Container is isolated from host network optimizations.\n")
			fmt.Printf("[audit] Hint: Add the optimized sysctls from the README to your Docker command to fix.\n")
		} else {
			fmt.Printf("[audit] Hint: System is not optimized for high volume. Run 'urnet-tools optimize' to fix (it will prompt for sudo).\n")
		}
	}

	// 5. Check Disk I/O (Free Space & Latency)
	if stateDir == "" {
		return false, false
	}
	if skipDiskTest {
		_, lowSpace = CheckDiskSpace(stateDir)
		return false, lowSpace
	}

	return RunDiskAudit(stateDir)
}

func readSysctlInt(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}
