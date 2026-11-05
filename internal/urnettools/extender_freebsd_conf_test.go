package urnettools

import (
	"io/fs"
	"os"
	"strings"
	"testing"
)

// sysctl.conf that does not exist yet is created; one that cannot be read is
// never overwritten.
func TestExtenderFreeBSDMissingSysctlConfIsCreated(t *testing.T) {
	var written string
	sp := &SystemPlatformConfigurer{PlatformName: "freebsd",
		ReadFile: func(string) ([]byte, error) {
			return nil, &fs.PathError{Op: "open", Path: "/etc/sysctl.conf", Err: fs.ErrNotExist}
		},
		WriteFile: func(_ string, d []byte, _ os.FileMode) error { written = string(d); return nil },
		ExecCmd:   func(string, ...string) ([]byte, error) { return []byte("0\n"), nil }}
	if ok, _, err := sp.applyFreeBSD("alice"); err != nil || !ok || !strings.Contains(written, "net.inet.ip.portrange.reservedhigh=0") {
		t.Fatalf("ok=%v err=%v written=%q", ok, err, written)
	}
	sp.ReadFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
	written = ""
	if _, _, err := sp.applyFreeBSD("alice"); err == nil || written != "" {
		t.Fatalf("unreadable sysctl.conf: err=%v written=%q", err, written)
	}
}
