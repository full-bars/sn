//go:build windows

package urnettools

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
)

// Known-folder lookups fail with an HRESULT, not a plain Win32 code, and
// errors.Is does not map that to os.ErrNotExist. Restart must still read it
// as "no service installed" instead of a damaged config.
func TestWindowsPathMissing(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"os not exist", &fs.PathError{Op: "open", Path: "x", Err: syscall.ERROR_FILE_NOT_FOUND}, true},
		{"path not found", &fs.PathError{Op: "open", Path: "x", Err: syscall.Errno(3)}, true},
		{"hresult file not found", syscall.Errno(0x80070002), true},
		{"hresult path not found", syscall.Errno(0x80070003), true},
		{"wrapped hresult", fmt.Errorf("load: %w", syscall.Errno(0x80070003)), true},
		{"access denied", syscall.ERROR_ACCESS_DENIED, false},
		{"damaged config", fmt.Errorf("config is damaged"), false},
	}
	for _, c := range cases {
		if got := windowsPathMissing(c.err); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if !windowsPathMissing(os.ErrNotExist) {
		t.Error("os.ErrNotExist must count as missing")
	}
}

// A provider with no pid and no binary cannot belong to the service. The
// restart must say so without reading the machine's service config.
func TestRestartProviderWindowsNoEvidence(t *testing.T) {
	err := restartProviderWindows(Provider{StateDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "could not restart provider") || !strings.Contains(err.Error(), "no process or binary") {
		t.Fatalf("restartProviderWindows(no pid, no binary) = %v, want the no-evidence error", err)
	}
}

// A stopped provider whose binary no saved service runs must fail with the
// restart contract's wording, whatever state the machine's service is in
// (none, unreadable profile folder, or another provider's service).
func TestRestartProviderWindowsUnownedBinary(t *testing.T) {
	err := restartProviderWindows(Provider{Binary: t.TempDir() + `\urnetwork.exe`, StateDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "could not restart provider") {
		t.Fatalf("restartProviderWindows(unowned binary) = %v, want a \"could not restart provider\" error", err)
	}
	t.Logf("error: %v", err)
}
