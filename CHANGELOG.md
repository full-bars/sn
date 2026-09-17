# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Docker multi-arch build+push CI (ghcr.io + DockerHub) on tag and main pushes
- Pelican panel egg (`pelican/egg-urnetwork-h3.json`)
- CI lint: pelican egg validation, shellcheck, docker update-source guard

## [v2026.9.17-1789639761-meso] — 2026-09-17

### Added
- UDP/QUIC bandwidth tracking via `H3PacketConnFactory` in platform transport
- TCP bandwidth tracking via `WrapConnectSettings` preserving proxy routing
- Generation-guarded `UnregisterProxySafe` in all production goroutines
- `withTempHome` now resets all 5 process-global caches (parity with 3.23-fix)
- SSRF guard boundary tests for 100.64/10, 198.18/15, 64:ff9b::/96
- Deterministic tests for regen guard, cleanOnce, hotswap trigger, semaphore release
- `.dockerignore` for smaller Docker build contexts
- `CHANGELOG.md`

### Fixed
- Proxy health entry race: stale `UnregisterProxy` no longer nukes fresh registration
- `globalClientJWTStore` race via RWMutex accessor pattern
- Semaphore deadlock: acquire/release extracted to per-attempt function scope
- Provider 30s exit: waits for `<-st.ctx.Done()` before goroutine cleanup
- Node name shadowing in `provideLaunchGoroutines`
- `atomicWriteJSON` race via `os.CreateTemp`
- DoH cache close scoped to correct function lifetime
- Hotswap version check now accepts v2026 releases
- `syscall.Umask` split to platform-specific files for Windows cross-compile
- `setRenewalTestHome` sets HOME env for CI renewal watcher
- `version` reports correctly via ldflags (no longer "dev")

### Removed
- 19 internal porting/analysis docs (review artifacts, parity analyses)
- 3 stale review docs with confirmed false positives
- `mainnnet/` typo directory

## [v2026.9.16-1789602650-meso] — 2026-09-16 (superseded)

Initial release — replaced by v2026.9.17 with additional fixes.
