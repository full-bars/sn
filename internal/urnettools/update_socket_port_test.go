package urnettools

import (
	"fmt"
	"strings"
	"testing"
)

func TestUpdateSocketVerification(t *testing.T) {
	for _, tc := range []struct {
		name            string
		pid             int
		version         string
		ok, wantSuccess bool
	}{
		{"new matching", 200, "wanted", true, true},
		{"old matching", 100, "wanted", true, false},
		{"new wrong version", 200, "old", true, false},
		{"unverified peer", 200, "wanted", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubVerifyLoop(t)
			p := testProvider(t.TempDir(), 100)
			verifyDiscoverFn = func() []Provider { return []Provider{testProvider(p.StateDir, tc.pid)} }
			verifyRunningImageHandleFn = func(int) (string, error) { return "", fmt.Errorf("denied") }
			verifySocketVersionFn = func(_ Provider, pid int) (string, bool) {
				if pid != tc.pid {
					t.Fatalf("pid=%d", pid)
				}
				return tc.version, tc.ok
			}
			err := verifyRestartLoop(p, testConfig("wanted"), true, "")
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("error=%v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "not rolled back") {
				t.Fatalf("bad observation: %v", err)
			}
		})
	}
}

func TestUpdateUnitTypeUnknown(t *testing.T) {
	setupMigrateMocks(t, migrateTestDeps{})
	p := Provider{Unit: "provider.service"}
	if _, err := migrateUnitToNotify(p); err == nil {
		t.Fatal("empty unit type accepted on migration")
	}
	if _, err := demoteUnitToSimple(p); err == nil {
		t.Fatal("empty unit type accepted on demotion")
	}
}
