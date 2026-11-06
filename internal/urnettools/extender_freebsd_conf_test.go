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

// A commented-out line persists nothing, and the last assignment wins.
func TestSysctlConfSetsReservedHighZero(t *testing.T) {
	const name = "net.inet.ip.portrange.reservedhigh"
	for _, tc := range []struct {
		name, conf string
		want       bool
	}{
		{"live line", name + "=0\n", true},
		{"spaces around equals", name + " = 0\n", true},
		{"trailing comment", name + "=0 # extender\n", true},
		{"commented out", "# " + name + "=0\n", false},
		{"commented out no space", "#" + name + "=0\n", false},
		{"other value", name + "=1023\n", false},
		{"last assignment wins, off", name + "=0\n" + name + "=1023\n", false},
		{"last assignment wins, on", name + "=1023\n" + name + "=0\n", true},
		{"prefix of another name", name + "x=0\n", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sysctlConfSetsReservedHighZero(tc.conf); got != tc.want {
				t.Fatalf("got %v, want %v for %q", got, tc.want, tc.conf)
			}
		})
	}
}

// The check and the apply agree: a commented-out line is rewritten as a live
// one, not trusted as already persisted.
func TestFreeBSDCommentedSysctlIsNotPersisted(t *testing.T) {
	const conf = "# net.inet.ip.portrange.reservedhigh=0\n"
	var written string
	sp := &SystemPlatformConfigurer{
		ReadFile: func(string) ([]byte, error) { return []byte(conf), nil },
		ExecCmd:  func(string, ...string) ([]byte, error) { return []byte("0\n"), nil },
		WriteFile: func(_ string, data []byte, _ os.FileMode) error {
			written = string(data)
			return nil
		},
	}
	if ok, _, err := sp.checkFreeBSD("alice"); err != nil || ok {
		t.Fatalf("a commented line read as persisted: ok=%v err=%v", ok, err)
	}
	if _, _, err := sp.applyFreeBSD("alice"); err != nil {
		t.Fatal(err)
	}
	if !sysctlConfSetsReservedHighZero(written) {
		t.Fatalf("apply left no live assignment: %q", written)
	}
}
