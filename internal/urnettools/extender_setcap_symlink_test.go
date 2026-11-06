package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setcap follows symlinks and runs as root, so a link in a user-writable
// directory must never receive the capability.
func TestApplyLinuxRefusesANonRegularBinary(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "urnetwork-real")
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "urnetwork")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}

	for name, tc := range map[string]struct {
		binary  string
		wantErr string
	}{
		"symlink":   {link, "not a regular file"},
		"directory": {dir, "not a regular file"},
		"missing":   {filepath.Join(dir, "gone"), "cannot apply"},
	} {
		t.Run(name, func(t *testing.T) {
			sp := &SystemPlatformConfigurer{
				PlatformName: "linux",
				Lstat:        os.Lstat,
				ExecCmd: func(n string, args ...string) ([]byte, error) {
					t.Fatalf("%s ran %v on %s", n, args, tc.binary)
					return nil, nil
				},
			}
			_, _, err := sp.applyLinux(tc.binary)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestApplyLinuxSetsCapabilityOnARegularFile(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "urnetwork")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls []string
	sp := &SystemPlatformConfigurer{
		PlatformName: "linux",
		Lstat:        os.Lstat,
		ExecCmd: func(n string, args ...string) ([]byte, error) {
			calls = append(calls, n)
			return []byte(bin + " cap_net_bind_service=ep"), nil
		},
	}
	ok, _, err := sp.applyLinux(bin)
	if err != nil || !ok || len(calls) != 2 || calls[0] != "setcap" {
		t.Fatalf("ok=%v err=%v calls=%v", ok, err, calls)
	}
}
