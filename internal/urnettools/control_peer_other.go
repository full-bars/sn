//go:build !linux

package urnettools

import (
	"fmt"
	"net"
)

func socketPeerPID(conn net.Conn) (int, error) {
	return 0, fmt.Errorf("socket peer credentials unavailable")
}
