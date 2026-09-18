# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v2026.9.18-1049118720-meso] — 2026-09-18

### Added
- Seven CI workflows restored (dash-compat, lifecycle x2, CFAA sync, tool smoke, functional soak, docker multi-container) plus pre-release shakedown workflows
- Release pipeline aligned: installers bundled in tarballs, monitoring bundle, `releases/<tag>.md` notes with auto-fallback, parallel non-blocking security scans
- Six installers ported (Mac, Win32, Deps, Uninstall x2, install-urnet-docker), all downloads GitHub-official
- Pelican panel egg with var-contract validation
- `cmd/fake-provider` CI test double for Windows lifecycle verification

### Fixed
- Docker container discovery: `isDockerCandidate` now matches `full-bars/sn` (was silently broken for the new image name)
- `.dockerignore` excluded the `provider/` source dir (pattern matched the directory, not the binary)
- `third_party/` copied before `go mod download` (npipe replace target)
- CI smoke workflow built `./provider/` (new layout is `./cmd/provider/`)
- docker-multi-container `proxy clear` now passes `-y` for non-interactive CI
- Shakedown workflows now trigger on `v2026.*` tags (were watching `v3.23.0-fix.*`)

### Docs
- `FORK_CHANGES.md` and `PROJECT_STRUCTURE.md` added
- Internal rollout document removed

### Changed
- DoH server-score cache removed after a live-fleet probe showed it inert under v2026 connect; the DoH resolver remains
- Docker entrypoint auto-selects jwt build mode when an auth code is supplied
- Six installer/update regression scripts ported (sn tag scheme), all review findings addressed
- Fork wiki ported into `docs/` with automated wiki sync

## [v2026.9.17-1789639761-meso] — 2026-09-17 (superseded)

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
