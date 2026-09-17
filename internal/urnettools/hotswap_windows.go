//go:build windows

package urnettools

import (
	"errors"
	"syscall"
)

// triggerHotSwap sends {cmd: "hotswap"} over the provider's control socket
// to initiate the in-process handoff. Windows has no SIGUSR2, so the
// control socket is the only trigger path.
func triggerHotSwap(p Provider) error {
	if err := hotSwapPreflight(p); err != nil {
		return err
	}
	return triggerHotSwapViaSocket(p)
}

// pidIsAlive reports whether a process with the given PID is still running.
// Uses OpenProcess with PROCESS_QUERY_LIMITED_INFORMATION | SYNCHRONIZE
// and WaitForSingleObject with a zero timeout to check without blocking.
//
// ERROR_ACCESS_DENIED from OpenProcess indicates the process exists but
// runs under a different user, SYSTEM, or elevated context — we cannot
// query it but it is definitely alive.
func pidIsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const (
		processQueryLimitedInfo = 0x1000
		synchronize             = 0x00100000
		waitTimeout             = 0x00000102
	)
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	procOpen := kernel32.NewProc("OpenProcess")
	procWait := kernel32.NewProc("WaitForSingleObject")
	procClose := kernel32.NewProc("CloseHandle")

	h, _, err := procOpen.Call(
		uintptr(processQueryLimitedInfo|synchronize),
		0, // bInheritHandle = FALSE
		uintptr(pid),
	)
	if h == 0 {
		// OpenProcess failed. ERROR_ACCESS_DENIED (0x5) means the process
		// is alive but we lack permission to query it.
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == syscall.ERROR_ACCESS_DENIED {
			return true
		}
		return false
	}
	defer procClose.Call(h)

	ret, _, _ := procWait.Call(h, 0) // 0 timeout = poll
	return ret == waitTimeout
}
