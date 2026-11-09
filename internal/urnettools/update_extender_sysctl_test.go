package urnettools

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnprivilegedPortRouteActive(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform string
		content  string
		readErr  error
		want     bool
	}{
		{name: "linux 443 active", platform: "linux", content: "443\n", want: true},
		{name: "linux 443 with spaces", platform: "linux", content: "  443  \n", want: true},
		{name: "linux 0 active", platform: "linux", content: "0\n", want: true},
		{name: "linux 80 active", platform: "linux", content: "80\n", want: true},
		{name: "linux 444 inactive", platform: "linux", content: "444\n"},
		{name: "linux 1024 inactive", platform: "linux", content: "1024\n"},
		{name: "linux negative inactive", platform: "linux", content: "-1\n"},
		{name: "linux non-numeric inactive", platform: "linux", content: "invalid\n"},
		{name: "linux empty inactive", platform: "linux", content: ""},
		{name: "linux unreadable inactive", platform: "linux", readErr: os.ErrNotExist},
		{name: "darwin ignored", platform: "darwin", content: "443\n"},
		{name: "windows ignored", platform: "windows", content: "443\n"},
		{name: "freebsd ignored", platform: "freebsd", content: "443\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := &SystemPlatformConfigurer{PlatformName: tc.platform, ReadFile: procPortStartReader(tc.content, tc.readErr)}
			if got := sp.UnprivilegedPortRouteActive(); got != tc.want {
				t.Fatalf("UnprivilegedPortRouteActive(%q) = %v, want %v", tc.platform, got, tc.want)
			}
		})
	}
}

// With the sysctl route active the updater skips the capability check, the
// restore and every warning, for updates and rollbacks alike.
func TestInstallProviderBinarySysctlRouteActive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		hadCap  bool
	}{
		{name: "443 without previous cap", content: "443\n"},
		{name: "443 with previous cap", content: "443\n", hadCap: true},
		{name: "0 without previous cap", content: "0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "new"), filepath.Join(dir, "provider")
			if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}
			commands := 0
			cfg := NewSystemPlatformConfigurer("linux")
			cfg.ReadFile = procPortStartReader(tc.content, nil)
			cfg.ExecCmd = func(name string, args ...string) ([]byte, error) {
				commands++
				if tc.hadCap {
					return []byte(dst + " cap_net_bind_service=ep"), nil
				}
				return nil, nil
			}
			var warnings bytes.Buffer
			// A blocked bounding set must not warn either: nothing needs it.
			status := filepath.Join(dir, "status")
			if err := os.WriteFile(status, []byte("CapBnd:\t0000000000000000\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := installProviderBinaryWithConfigAndStatus(src, dst, "", "linux", cfg, &warnings, status); err != nil {
				t.Fatalf("install failed: %v", err)
			}
			if commands != 0 {
				t.Fatalf("expected no getcap or setcap with the sysctl route active, ran %d commands", commands)
			}
			if got := warnings.String(); got != "" {
				t.Fatalf("expected no warnings with the sysctl route active, got %q", got)
			}
			if data, err := os.ReadFile(dst); err != nil || string(data) != "new" {
				t.Fatalf("binary not updated: %q %v", data, err)
			}
		})
	}
}

// Without the route (threshold above 443 or unreadable) the capability flow is
// unchanged: a missing capability still warns and a present one is restored.
func TestInstallProviderBinarySysctlRouteInactive(t *testing.T) {
	for _, tc := range []struct {
		name        string
		content     string
		readErr     error
		hadCap      bool
		wantSets    int
		wantWarning bool
	}{
		{name: "1024 without previous cap warns", content: "1024\n", wantWarning: true},
		{name: "1024 with previous cap restores", content: "1024\n", hadCap: true, wantSets: 1},
		{name: "unreadable without previous cap warns", readErr: os.ErrNotExist, wantWarning: true},
		{name: "unreadable with previous cap restores", readErr: os.ErrNotExist, hadCap: true, wantSets: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "new"), filepath.Join(dir, "provider")
			if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}
			sets, capPresent := 0, false
			cfg := NewSystemPlatformConfigurer("linux")
			cfg.ReadFile = procPortStartReader(tc.content, tc.readErr)
			cfg.ExecCmd = func(name string, args ...string) ([]byte, error) {
				data, err := os.ReadFile(dst)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) == "old" {
					if tc.hadCap {
						return []byte(dst + " cap_net_bind_service=ep"), nil
					}
					return nil, nil
				}
				switch name {
				case "getcap":
					if capPresent {
						return []byte(dst + " cap_net_bind_service=ep"), nil
					}
					return nil, nil
				case "setcap":
					sets++
					capPresent = true
					return nil, nil
				}
				t.Fatalf("unexpected command %s", name)
				return nil, nil
			}
			var warnings bytes.Buffer
			if err := installProviderBinaryWithConfigAndStatus(src, dst, "", "linux", cfg, &warnings, filepath.Join(dir, "missing-status")); err != nil {
				t.Fatalf("install failed: %v", err)
			}
			if sets != tc.wantSets {
				t.Fatalf("setcap ran %d times, want %d", sets, tc.wantSets)
			}
			if tc.wantWarning != strings.Contains(warnings.String(), "WARNING:") {
				t.Fatalf("warning=%q, wantWarning=%v", warnings.String(), tc.wantWarning)
			}
		})
	}
}

// A configurer that cannot report the route keeps the old behaviour.
func TestInstallProviderBinaryConfigurerWithoutRouteReport(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "new"), filepath.Join(dir, "provider")
	if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	if err := installProviderBinaryWithConfigAndStatus(src, dst, "", "linux", &mockPlatformConfigurer{platform: "linux"}, &warnings, filepath.Join(dir, "missing-status")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warnings.String(), "WARNING:") {
		t.Fatalf("a configurer without a route report must still warn about a missing capability: %q", warnings.String())
	}
}
