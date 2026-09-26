# Changelog

## [Unreleased]

### Added

- **Proxies are identified by account, not just by address** (<https://github.com/full-bars/sn/pull/30>): a shared-gateway proxy provider hands one `host:port` to several accounts, and the account decides the backend IP. Every store that tracked a proxy by address alone now tracks it by identity — the address, plus the user when the proxy is authenticated — so two accounts at one gateway no longer collapse into a single entry. That covers the proxy state file, the client-JWT store, the health registry, the earnings and grade lookups, the audit and trust decisions, and the add/rotation semantics. A credential rotation keeps the same identity on purpose, so a restart still reuses the existing client identity instead of resetting its reliability reputation.
- **Live traffic, runtime internals and a reworked `top` view** (<https://github.com/full-bars/sn/pull/31>): the live status block separates billable from total traffic, so non-billable bytes (a direct socket, for instance) are visible instead of folded into one number, and session totals follow proxies being removed or respawning. `top` gains a zoomable graph, a runtime panel (goroutines, heap, descriptors), a theme/graph menu, and a layout that adapts to a small terminal. The provider answers three new light control-socket commands — `traffic`, `internals`, `goroutines` — so the 100ms poll no longer rebuilds a full snapshot; an older provider is detected and falls back to the snapshot's own rates.
- **A stated reason for the node's state** (<https://github.com/full-bars/sn/pull/31>): `starting` and `degraded` now say why — still resolving proxies, a source that could not be read, a source that returned nothing, or how many proxies are dead against how many are configured.

### Changed

- **A node that deliberately runs with no proxies now reads `active`, not `degraded`** (<https://github.com/full-bars/sn/pull/31>): serving on the direct transport with no proxy source configured is a valid, completed configuration, and was previously indistinguishable from a real outage. A configured source that came back empty still reads degraded, and so does a node whose direct transport is off with nothing to serve.
- **The idle hint blames auth only when it explains the idleness** (<https://github.com/full-bars/sn/pull/31>): a steady trickle of auth retries on a large healthy pool is no longer reported as an auth outage. Auth is blamed for a failure wave, or when most of the pool is not connected while failures are happening; otherwise the hint states what is true and shows the numbers behind it.
- **The reload summary says where additions came from** (<https://github.com/full-bars/sn/pull/31>): the `reloaded: +N added` line breaks the additions down by source, and URL-sourced launches get their own line instead of being folded into a bare count.
- **Proxy keys are never printed raw** (<https://github.com/full-bars/sn/pull/30>): an identity key embeds the account, so operator-facing listings show a display form instead of the raw key.

### Fixed

- **Two accounts at one gateway no longer overwrite each other** (<https://github.com/full-bars/sn/pull/30>): the stores were keyed by identity but several consumers still read a bare address, so a shared-gateway proxy silently merged the accounts. The earn tracker, the launch generation, the degraded-proxy reaper and the grade report now all use the identity. Credentialed proxies were previously invisible to the reaper, could report to the hub as ungraded, and could be left stuck in the cancel map after a failure.
- **A credentialed URL proxy is no longer counted twice** (<https://github.com/full-bars/sn/pull/30>): the match collector added it under its identity key and again under its bare address, so it appeared twice in `display` and inflated the `proxy remove --match` count.
- **A removed proxy's login is dropped** (<https://github.com/full-bars/sn/pull/30>): the client-JWT store migrated legacy keys but never pruned, so a removed proxy's login stayed on disk for the store's whole retention window and a re-add at the same address inherited a JWT minted for the old account. A rotation — same identity, new password — deliberately keeps its login.
- **A source that goes empty stops the proxies it used to supply** (<https://github.com/full-bars/sn/pull/31>): the reload returned before the removal pass, so the old proxies kept dialling and the stale configured count made the status line ignore the new empty-source state. A proxy with live clients is now drained rather than cut, and the state file is reconciled and persisted on that path.
- **A file source that cannot be read is reported as a failure** (<https://github.com/full-bars/sn/pull/31>): it left the resolution pending, so the status line read `starting: resolving proxies` and the snapshot later read it as a stuck startup. Running proxies are deliberately left alone.
- **A proxy source URL cannot leak its token into the logs** (<https://github.com/full-bars/sn/pull/31>): the per-source log lines and the operator warning use a redacted label that strips the query, the userinfo, and credential-bearing path segments, so a source like `.../token/SECRET/list` no longer reaches the important log or the warning, and one source no longer produces two different warning keys.
- **A no-source node stops claiming it is retrying** (<https://github.com/full-bars/sn/pull/31>): that configuration has nothing to retry, and the reason now matches the systemd status line.
- **The `top` menu now persists** (<https://github.com/full-bars/sn/pull/31>): the menu wrote through a settings path that nothing ever set and never loaded it back, so every choice was silently discarded and the theme reset on the next run. A theme forced by the environment (`NO_COLOR`, a dumb terminal) is not saved, so it does not outlive the terminal that forced it, and a config directory created by a first run under `sudo` is handed to the invoking user instead of staying root-owned and blocking their next save.

---

## [v2026.9.22-1052862940-meso] — 2026-09-22

### Changed

- **Cross-platform compile gate on every PR** (PR #28): PR CI now compiles the provider, `urnet-tools`, and `urnet-docker` binaries for Linux, macOS, and Windows across the amd64 and arm64 architectures, so a change that breaks a build on any platform fails the PR instead of surfacing only at release time.

### Maintenance

- **Deterministic test suite** (PR #27).

---

## [v2026.09.21-1790052822-meso] — 2026-09-22

### Added

- **Audit ring survives hotswap** (PR #21): the parent flushes the audit ring at the handoff commit point on every path (systemd, Windows, Docker); the successor merges it back from disk after takeover with timezone-safe deduplication. Lifecycle events (start, hotswap, shutdown) join the `set`/`clear` entries in `urnet-tools history`.
- **Sliding severity scale for provider status** (PR #21): `active` >= 90%, `partial` 70-89%, `degraded` 50-69%, `critical` < 50%, percentage always rendered and clamped at 100.
- **HotSwap how-to and measured costs** (PR #14): new `docs/HotSwap.md` covering requirements, the update flow, what you will see, and measured costs (connection ramp, memory, drain).
- **Design proposal: per-client make-before-break HotSwap handover** (PR #15): `docs/design/hotswap-make-before-break.md` plans a handover that keeps every client connected throughout. A proposal only, no code.
- **Quick start and buildable release images** (PR #10): docs fixed and refreshed; the release pipeline builds container images.
- **`urnet-tools top`, a live full-screen view of a provider**: the last 10 minutes of throughput as a graph, current and average rate, clients, the proxy pool, memory and descriptors, and recent events such as restarts and state changes. It reads only the provider's control socket (the live snapshot above) and changes nothing. Also available as `urtop`, a link the installer and `urnet-tools update` now create. Keys: `q`, `Esc` or `Ctrl-C` quit; `Tab` and `Shift-Tab` switch provider; `+` and `-` change the refresh rate; `?` shows help. When the provider does not answer (stopped, or an older build) the screen stays up, shows `DISCONNECTED` with the reason and a countdown, and resumes by itself. It needs an interactive terminal; use `status` for scripts.
- **Live node snapshot and a live block in `urnet-tools status`**: the provider keeps a snapshot of the node, cached for about a second, and serves it over the control socket as `snapshot`. It carries the billable rate now and as 1 and 5 minute averages with a 10 minute history, active clients and sessions, the proxy pool by status, pressure, memory, descriptors and goroutines, the restart reason, and a flowing, idle, degraded or starting verdict with a hint when idle. `urnet-tools status` shows it as a live block, `urnet-tools status --json` prints it for scripts, and a host with several providers gets a one-line summary of each. A provider that predates the command is handled: the block is skipped, and only `--json` reports it as unavailable.
- **Restart reason**: `urnet-tools update`, `hotswap` and `restart` record why the provider is about to restart, and the provider reports it after it starts (`update`, `hotswap`, `manual`, `clean`, `unclean` or `first-start`) in the snapshot and as `urnet_restart_reason`.
- **Resource metrics**: `urnet_mem_limit_bytes`, `urnet_rss_bytes`, `urnet_open_fds` and `urnet_fd_limit` (the last three on Linux).
- **Prometheus alert rules in the Monitoring bundle**: `UrnetworkNodeDown` and `UrnetworkRestartLoop` are on by default, and four more (old version, memory near limit, descriptors near limit, no billable traffic) are commented out because their thresholds depend on your fleet. The dashboard gains lifecycle and limit panels. See `docs/Monitoring.md`.

### Security

- **Container-discovery ghost hardening, round 2** (PR #12): containerized providers no longer appear as host providers; state paths read and written through descriptor-pinned handles.

### Fixed

- **Paid grading sampled the same hosts every sweep** on a box with no URL sources: the probe rotation only advanced during URL fetches, so a second grade was not independent of the first. It now rotates once per paid pass.
- **HotSwap unit migration was a silent no-op** (PR #14): the installer writes a unit with no `Type=` line and `update` only rewrote an explicit `Type=simple`, so nodes set up by the current installer never reached a hotswap. A unit with no `Type=` now gets `Type=notify` and `NotifyAccess=all`, and the update says so when a unit cannot be migrated.
- **HotSwap declines up front when the running provider has no notify socket** (PR #14): a migrated unit whose provider has not restarted no longer aborts after SIGUSR2 and rolls back; new decline label `needs_restart`.
- **pprof diagnostics disappeared on every other hotswap** (PR #14): a candidate retries the diagnostics bind until its parent releases the port.
- **`urnet-tools` not found** (PR #14) from non-interactive shells, zsh and root: the installer links `urnet-tools` and `urnetwork` into `~/.local/bin` and `/usr/local/bin` and writes the PATH block to `~/.bashrc`, `~/.profile` and `~/.zshenv`; `urnet-tools update` repairs older installs.
- **Proxy credential rotation on re-paste** (PR #13): pasting an address that already exists with different credentials now rotates the running proxy instead of silently keeping the old credentials; all duplicate entries for an address are scanned before an add is skipped.
- **Docker idle-update poll is bounded** (PR #12): a hung Docker daemon can no longer stall the idle wait past its own timeout.
- **Interactive delegated subcommands wire stdin** (PR #11): `proxy remove-dead`/`remove`/`trim` prompts answer properly.

### Changed

- **Runtime decoupling**: `proxy audit` is an independent runtime feature with its own control socket actions (`audit on|off|status|release`) and does not depend on `self-heal`. `self-heal` remains dedicated to resource-pressure actuators.
- The systemd status line (`urnet-tools status` on Linux) counts proxies held by proxy audit as intentional: `active: 47/50 proxies authenticated, 3 parked by proxy audit` instead of `partial`, and notes when audit is paused.
- The paid-grade header comments no longer claim grades are never consulted by anything that changes a proxy: trim shed ranking and proxy audit read them.
- **HotSwap is no longer described as zero-downtime.** Measured on a live node, a hotswap removes the 2 to 3 second window with no provider process, but the old process drops its proxy connections at the handover and the new one rebuilds them over about 30 s, the same ramp as a restart. Earlier entries that say "zero-downtime" describe the process handover only. See `docs/HotSwap.md`.

### Maintenance

- **Release prep for this synchronized release** (PR #16).

### CI

- **`VT_JSON_FILE` machine-readable export** (PR #9): JSON write failures now fail the scan instead of being skipped.

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
