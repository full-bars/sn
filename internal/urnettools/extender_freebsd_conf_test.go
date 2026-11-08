package urnettools

import (
	"testing"
)

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

// A commented-out line is not trusted as persisted.
func TestFreeBSDCommentedSysctlIsNotPersisted(t *testing.T) {
	const conf = "# net.inet.ip.portrange.reservedhigh=0\n"
	sp := &SystemPlatformConfigurer{
		ReadFile: func(string) ([]byte, error) { return []byte(conf), nil },
		ExecCmd:  func(string, ...string) ([]byte, error) { return []byte("0\n"), nil },
	}
	if ok, _, err := sp.checkFreeBSD("alice"); err != nil || ok {
		t.Fatalf("a commented line read as persisted: ok=%v err=%v", ok, err)
	}
}
