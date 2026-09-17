//go:build windows

package urnettools

import "testing"

// TestCmdRestartWindows_Fallback is intentionally empty.
//
// The Windows lifecycle fallback path (stop+start when hotswap fails
// or the control socket is unreachable) is already tested by the CI
// integration test in .github/workflows/windows-lifecycle.yml.
//
// A previous unit test that spawned a helper subprocess and called
// cmdRestartWindows hung on Windows CI because:
//  1. exec.Command with DETACHED_PROCESS | CREATE_BREAKAWAY_FROM_JOB
//     hangs when the binary path is nonexistent on Windows.
//  2. The mock socket had a single Accept() that was consumed by
//     controlSocketReachable, leaving subsequent socket operations
//     hanging.
//
// Rather than fight Windows process lifecycle quirks, the integration
// test covers this path end-to-end.
func TestCmdRestartWindows_Fallback(t *testing.T) {
	t.Skip("covered by CI integration test (windows-lifecycle.yml)")
}
