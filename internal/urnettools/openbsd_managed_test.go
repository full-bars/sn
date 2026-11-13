package urnettools

import (
	"errors"
	"testing"
)

func TestOpenBSDManagedPID(t *testing.T) {
	for _, tc := range []struct {
		name, runfile, output string
		lookupErr             error
		want                  int
	}{
		{"unique", "daemon_rtable=4\npexp=/usr/local/bin/urnetwork provide -x\n", "123\n", nil, 123},
		{"absent", "daemon_rtable=4\npexp=provider\n", "", nil, 0},
		{"multiple", "daemon_rtable=4\npexp=provider\n", "123\n456\n", nil, -1},
		{"missing expression", "daemon_rtable=4\n", "123", nil, -1},
		{"missing table", "pexp=provider\n", "123", nil, -1},
		{"invalid table", "daemon_rtable=-1\npexp=provider\n", "123", nil, -1},
		{"duplicate expression", "daemon_rtable=0\npexp=a\npexp=b\n", "123", nil, -1},
		{"bad pid", "daemon_rtable=0\npexp=a\n", "no", nil, -1},
		{"lookup failure", "daemon_rtable=0\npexp=a\n", "123", errors.New("failed"), -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := openbsdManagedPID(tc.runfile, func(table, pattern string) ([]byte, error) {
				if tc.name == "unique" && (table != "4" || pattern != "/usr/local/bin/urnetwork provide -x") {
					t.Fatalf("lookup got %q %q", table, pattern)
				}
				return []byte(tc.output), tc.lookupErr
			})
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestOpenBSDCleanupNeedsStop(t *testing.T) {
	for _, tc := range []struct {
		name              string
		selected, current int
		stop, fail        bool
	}{
		{"already stopped", 123, 0, false, false},
		{"stopped discovery", 0, 0, false, false},
		{"same process", 123, 123, true, false},
		{"other process", 123, 456, false, true},
		{"unexpected process", 0, 456, false, true},
		{"ambiguous", 123, -1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop, err := openbsdCleanupNeedsStop(Provider{PID: tc.selected}, tc.current)
			if stop != tc.stop || (err != nil) != tc.fail {
				t.Fatalf("stop=%v err=%v", stop, err)
			}
		})
	}
}
