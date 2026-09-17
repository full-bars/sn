# Big Picture Review V2

## Review Summary
Sonnet review of h3-provider codebase after full test parity (818 tests, zero //go:build ignore).
Review date: 2026-09-16. Static analysis only — no test execution.

## CRITICAL
1. **30s exit timeout** (`provide.go:66-96`) — Process exits after 30s with no proxies. This is EXPECTED behavior for a provider with no configured proxies; the comment says "exits after 30s with no proxies = EXPECTED behavior". NOT a bug.

2. **Bandwidth counters at zero** — `WrapDialContextSettings` and `AddSession` not called in production code paths. This may be an upstream issue or may need wiring in the provider main loop.

3. **Node ID shadowing** (`provide.go:304`) — `watcherName, err := os.Hostname()` creates new variable inside if block, shadowing outer. Should use `=` not `:=`.

## HIGH
- Removed proxy health entry race — concurrent goroutine cancellation without waiting
- Hot-swap confirmation requires authentication first
- Seven test files were deleted (not renamed to .bak as stated)
- Proxy-source URLs logged in full (API keys visible)
- SSRF guard gaps (100.64/10 not blocked)

## MEDIUM
- JWT renewal holds global lock through rate limiter + HTTP
- Drain loop has no timeout despite comments
- JWT store flush can resurrect pruned entries
- Test isolation issues with HOME directory
- Flaky test margins (250ms in stagger test)

## LOW
- Unsynchronized reads of metricsServer/globalProxySlowRetryState
- getProxyIndex returns 0 for unknown addresses
- Control socket created before permissions tightened
- Repo root has release tarballs, mainnnet/ typo
