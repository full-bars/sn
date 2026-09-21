# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v2026.9.21-1790006725-meso] — 2026-09-21

### Added

- **Audit ring survives hotswap (PR #21)**: the parent flushes the audit ring at the handoff commit point on every path (systemd, Windows, Docker); the successor merges it back from disk after takeover with timezone-safe deduplication. Lifecycle events (start, hotswap, shutdown) join the `set`/`clear` entries in `urnet-tools history`.
- **Sliding severity scale for provider status (PR #21)**: `active` >= 90%, `partial` 70-89%, `degraded` 50-69%, `critical` < 50%, percentage always rendered and clamped at 100.
- **HotSwap how-to and measured costs (PR #14)**: new `docs/HotSwap.md`.
- **Design proposal: per-client make-before-break HotSwap handover (PR #15)**: design doc only.
- **Quick start and buildable release images (PR #10)**: docs fixed and refreshed; the release pipeline builds container images.

### Security

- **Container-discovery ghost hardening, round 2 (PR #12)**: containerized providers no longer appear as host providers; state paths read and written through descriptor-pinned handles.

### Fixed

- **HotSwap unit migration was a silent no-op (PR #14)**: units with no `Type=` now migrate to `Type=notify`; `urnet-tools` goes on PATH; stale processes decline up front.
- **Proxy credential rotation on re-paste (PR #13)**.
- **Docker idle-update poll is bounded (PR #12)**.
- **Interactive delegated subcommands wire stdin (PR #11)**: `proxy remove-dead`/`remove`/`trim` prompts answer properly.

### Changed

- **HotSwap is no longer described as zero-downtime.** Proxy connections still ramp back over about 30 s, the same as a restart. See `docs/HotSwap.md`.

### Maintenance

- **Release prep for this synchronized release (PR #16)**.

### CI

- **`VT_JSON_FILE` machine-readable export (PR #9)**: JSON write failures now fail the scan instead of being skipped.

---

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
