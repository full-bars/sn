package urnettools

import (
	"fmt"
	"strings"
	"testing"
)

func TestApplyLinuxRejectsBlockedBoundingSet(t *testing.T) {
	for _, tc := range []struct {
		name, mask string
		allowed    bool
	}{
		{"blocked", "00000000a80421fb", false},
		{"allowed", "00000000a80425fb", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &SystemPlatformConfigurer{PlatformName: "linux"}
			cfg.ReadFile = func(string) ([]byte, error) { return []byte("CapBnd:\t" + tc.mask + "\n"), nil }
			var set bool
			cfg.ExecCmd = func(name string, args ...string) ([]byte, error) {
				switch name {
				case "setcap":
					set = true
					return nil, nil
				case "getcap":
					return []byte("/binary cap_net_bind_service=ep"), nil
				default:
					return nil, fmt.Errorf("unexpected command %s", name)
				}
			}
			_, _, err := cfg.applyLinux("/binary")
			if set != tc.allowed {
				t.Fatalf("setcap called=%v want %v", set, tc.allowed)
			}
			if tc.allowed && err != nil {
				t.Fatal(err)
			}
			if !tc.allowed && (err == nil || !strings.Contains(err.Error(), "bounding set")) {
				t.Fatalf("missing bounding-set error: %v", err)
			}
		})
	}
}
