package provider

import "testing"

// The sensor's pool count must exclude the native direct transport: it is one
// fixed goroutine, not a pool member, and counting it made a direct-only node
// divide by 1 and judge its ~1,000-goroutine background as an emergency.
func TestRunningProxyCountForPressureExcludesDirect(t *testing.T) {
	ResetProxyHealthForTesting()
	t.Cleanup(ResetProxyHealthForTesting)

	if got := runningProxyCountForPressure(); got != 0 {
		t.Fatalf("empty registry: got %d, want 0", got)
	}
	// The direct transport is registered at index 0 under the key "direct"
	// exactly as provider startup does.
	RegisterProxy(0, "direct", "direct")
	RegisterProxy(1, "10.0.0.1:1080", "10.0.0.1:1080")
	RegisterProxy(2, "10.0.0.2:1080", "10.0.0.2:1080")
	if got := runningProxyCountForPressure(); got != 2 {
		t.Fatalf("direct + 2 proxies: got %d, want 2 (direct excluded)", got)
	}
	// A direct-only node must read ZERO running proxies so the sensor falls
	// back to the absolute ramp instead of dividing by one.
	UnregisterProxy(1)
	UnregisterProxy(2)
	if got := runningProxyCountForPressure(); got != 0 {
		t.Fatalf("direct-only node: got %d, want 0 (else per-proxy division by 1)", got)
	}
	UnregisterProxy(0)
	if got := runningProxyCountForPressure(); got != 0 {
		t.Fatalf("nothing registered: got %d, want 0", got)
	}
}
