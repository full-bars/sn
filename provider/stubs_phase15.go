package provider

// DESIGN ADAPTATION: Stubs for connect symbols removed in h3 (v2026).
// These will be wired up when the full connect-integration layer lands.
// Kept package-local so the ported code compiles without a real h3
// connect backend.

// DegradedProxyEntry, DegradedProxies are defined in proxy_health.go.

// ResizeMessagePoolsPerClass is a no-op stub (h3 does not expose pool resizing).
func ResizeMessagePoolsPerClass(_ int64) {}
