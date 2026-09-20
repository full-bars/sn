//go:build !linux

package urnettools

// processNotifySocket cannot inspect another process's environment portably;
// it reports unknown and the caller falls back to the unit Type= check alone.
func processNotifySocket(pid int) (has, known bool) { return false, false }
