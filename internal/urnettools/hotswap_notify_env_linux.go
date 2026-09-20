//go:build linux

package urnettools

import (
	"bytes"
	"os"
	"strconv"
)

// processNotifySocket reports whether the process was started with a systemd
// notify socket (NOTIFY_SOCKET in its environment). known is false when the
// environment cannot be read (another user's process without root, a process
// that just exited), in which case nothing is concluded.
func processNotifySocket(pid int) (has, known bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return false, false
	}
	for _, kv := range bytes.Split(b, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte("NOTIFY_SOCKET=")); ok && len(v) > 0 {
			return true, true
		}
	}
	return false, true
}
