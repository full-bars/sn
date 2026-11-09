package urnettools

import (
	"fmt"
	"net"
	"path/filepath"
	"time"
)

func providerVersionFromSocketForPID(p Provider, wantPID int) (string, bool) {
	if p.StateDir == "" || wantPID <= 0 {
		return "", false
	}
	resp, err := sendSocketRequestChecked(filepath.Join(p.StateDir, "provider.sock"), controlRequest{Cmd: "version"}, 5*time.Second, func(conn net.Conn) error {
		pid, err := socketPeerPID(conn)
		if err != nil {
			return err
		}
		if pid != wantPID {
			return fmt.Errorf("control socket peer pid %d, expected %d", pid, wantPID)
		}
		return nil
	})
	return resp.BuildVersion, err == nil && resp.OK && resp.BuildVersion != ""
}
