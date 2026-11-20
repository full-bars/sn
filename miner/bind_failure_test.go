package miner

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

// bind_failure_test.go covers ClassifyBindError and the per-port listen-error
// bookkeeping in the miner package. The classifier is the smallest local
// equivalent of the one the connect fork does not yet provide.

func TestClassifyBindError(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"nil":                {nil, ""},
		"address in use":     {&net.OpError{Op: "listen", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}, BindFailureAddressInUse},
		"wrapped in use":     {fmt.Errorf("udp 53: %w", syscall.EADDRINUSE), BindFailureAddressInUse},
		"permission":         {&net.OpError{Op: "listen", Err: os.NewSyscallError("bind", syscall.EACCES)}, BindFailurePermissionDenied},
		"operation not perm": {fmt.Errorf("udp 53: %w", syscall.EPERM), BindFailurePermissionDenied},
		"os permission":      {fmt.Errorf("udp 53: %w", os.ErrPermission), BindFailurePermissionDenied},
		"other errno":        {syscall.ENETDOWN, BindFailureUnknown},
		"text only":          {errors.New("address already in use"), BindFailureUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ClassifyBindError(tc.err); got != tc.want {
				t.Errorf("ClassifyBindError = %q, want %q", got, tc.want)
			}
		})
	}
}

// A real second bind of one udp port is classified as an address in use.
func TestClassifyBindErrorRealCollision(t *testing.T) {
	first, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback udp: %v", err)
	}
	defer first.Close()
	_, err = net.ListenPacket("udp", first.LocalAddr().String())
	if err == nil {
		t.Skip("platform allows a second bind of the same udp port")
	}
	if got := ClassifyBindError(err); got != BindFailureAddressInUse {
		t.Fatalf("ClassifyBindError(%v) = %q, want %q", err, got, BindFailureAddressInUse)
	}
}
