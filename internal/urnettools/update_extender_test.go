package urnettools

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateExtenderCapabilityAfterSwap(t *testing.T) {
	for _, tc := range []struct {
		name        string
		present     bool
		applyFails  bool
		wantSetcap  int
		wantWarning bool
	}{
		{name: "missing", wantWarning: true},
		{name: "present", present: true, wantSetcap: 1},
		{name: "apply failure", present: true, applyFails: true, wantSetcap: 1, wantWarning: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "new"), filepath.Join(dir, "provider binary")
			if err := os.WriteFile(src, []byte("new"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, []byte("old"), 0755); err != nil {
				t.Fatal(err)
			}
			cfg := NewSystemPlatformConfigurer("linux")
			present, sets, checks := false, 0, 0
			cfg.ExecCmd = func(name string, args ...string) ([]byte, error) {
				data, err := os.ReadFile(dst)
				if err != nil || (string(data) != "new" && string(data) != "old") {
					t.Fatalf("%s unexpected binary: %q %v", name, data, err)
				}
				if string(data) == "old" && name == "getcap" {
					checks++
					if tc.present {
						return []byte(dst + " cap_net_bind_service=ep"), nil
					}
					return nil, nil
				}
				switch name {
				case "getcap":
					checks++
					if !(len(args) == 1 && args[0] == dst || len(args) == 2 && args[0] == "-n" && args[1] == dst) {
						t.Fatalf("getcap args: %q", args)
					}
					if present {
						return []byte(dst + " cap_net_bind_service=ep"), nil
					}
					return nil, nil
				case "setcap":
					sets++
					if len(args) != 2 || args[0] != "cap_net_bind_service=+ep" || args[1] != dst {
						t.Fatalf("setcap args: %q", args)
					}
					if tc.applyFails {
						return nil, fmt.Errorf("permission denied")
					}
					present = true
					return nil, nil
				default:
					t.Fatalf("unexpected command %s", name)
					return nil, nil
				}
			}
			var warnings bytes.Buffer
			if err := installProviderBinaryWithConfigAndStatus(src, dst, "", "linux", cfg, &warnings, filepath.Join(dir, "missing-status")); err != nil {
				t.Fatalf("update failed: %v", err)
			}
			if checks == 0 || sets != tc.wantSetcap {
				t.Fatalf("getcap=%d setcap=%d", checks, sets)
			}
			if tc.wantSetcap == 1 && !tc.applyFails && checks != 4 {
				t.Fatalf("successful apply not verified: %d checks", checks)
			}
			if got := warnings.String(); tc.wantWarning != (strings.Contains(got, "WARNING:") && strings.Contains(got, "sudo setcap cap_net_bind_service=+ep '"+dst+"'")) {
				t.Fatalf("warning=%q", got)
			}
		})
	}
}

func TestUpdateExtenderCapabilityNonLinux(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "new"), filepath.Join(dir, "provider")
	if err := os.WriteFile(src, []byte("new"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := NewSystemPlatformConfigurer("darwin")
	cfg.ExecCmd = func(string, ...string) ([]byte, error) { t.Fatal("non-Linux hook ran a command"); return nil, nil }
	if err := installProviderBinaryWithConfig(src, dst, "", "darwin", cfg, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}
