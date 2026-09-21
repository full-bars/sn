# NEXT-RELEASE

> [!IMPORTANT]
> **HotSwap is not zero-downtime for proxy connections.** It removes the 2 to 3 second window with no provider process, but proxy connections still ramp back over about 30 s, the same as a restart. This release corrects the wording everywhere and fixes the migration that kept nodes from ever reaching a hotswap. See `docs/HotSwap.md`.

> [!NOTE]
> **First update after this release:** a node set up by an earlier installer still has a unit with no `Type=` line. The first `urnet-tools update` converts it to `Type=notify` and restarts once; updates after that hotswap. Windows keeps the documented limitation that state paths are not walked handle by handle (Windows has no `openat`).

## What's Changed

### Added

- **Proxy audit**: automated proxy quality auditing and parking (`urnet-tools proxy audit` / `urnet-docker proxy audit`). The provider evaluates proxy reachability scores every 5 minutes and temporarily parks proxies that grade as proven junk (two bad decidable grades at 0.4 or lower, idle, not earning) with a 6h to 7d backoff ladder, and restores them on a healthy regrade. With proxy audit off it operates in observe-only mode. Guarded by a per-pass cap, a 24h budget, a running floor of `max(10, half)`, a thin-pass check, and a mass-failure breaker. Controllable on the fly via control socket (`urnet-tools proxy audit on|off|status|release`) with affirmative CLI feedback (`✓ ...`). `urnet_proxy_audit_*` gauges export telemetry (with legacy aliases). See `docs/Configuration.md` and `docs/Proxy-Management.md`.
- **HotSwap how-to and measured costs** (<https://github.com/full-bars/sn/pull/14>): new `docs/HotSwap.md` covering requirements, the update flow, what you will see, and measured costs (connection ramp, memory, drain).
- **Design proposal: per-client make-before-break HotSwap handover** (<https://github.com/full-bars/sn/pull/15>): `docs/design/hotswap-make-before-break.md` plans a handover that keeps every client connected throughout. A proposal only, no code; it needs research before development.

### Security

- **Container-discovery ghost hardening, round 2** (<https://github.com/full-bars/sn/pull/12>): containerized providers no longer appear as host providers (cgroup classification when the mount namespace is unreadable); provider state is read and written through descriptor-pinned handles that walk from the kernel-attributed owner home without following symlinks; state-dir arguments are validated against the owner captured in the same process scan; `docker cp` output is decoded from the tar stream and size-capped; Docker commands have deadlines and the exec fallback rejects option injection; session load and save, the pending-overrides lock, self-heal, hotswap counters, the direct toggle, the reload trigger and the unit backup and replace paths no longer re-resolve a user-controlled pathname as root; a recovered provider binary is chmod'ed and chown'ed on the open file descriptor.

### Fixed

- **Paid grading sampled the same hosts every sweep** on a box with no URL sources: the probe rotation only advanced during URL fetches, so a second grade was not independent of the first. It now rotates once per paid pass.
- **HotSwap unit migration was a silent no-op** (<https://github.com/full-bars/sn/pull/14>): the installer writes a unit with no `Type=` line and `update` only rewrote an explicit `Type=simple`, so nodes set up by the current installer never reached a hotswap. A unit with no `Type=` now gets `Type=notify` and `NotifyAccess=all`, and the update says so when a unit cannot be migrated.
- **HotSwap declines up front when the running provider has no notify socket** (<https://github.com/full-bars/sn/pull/14>): a migrated unit whose provider has not restarted no longer aborts after SIGUSR2 and rolls back; new decline label `needs_restart`.
- **pprof diagnostics disappeared on every other hotswap** (<https://github.com/full-bars/sn/pull/14>): a candidate retries the diagnostics bind until its parent releases the port.
- **`urnet-tools` not found** (<https://github.com/full-bars/sn/pull/14>) from non-interactive shells, zsh and root: the installer links `urnet-tools` and `urnetwork` into `~/.local/bin` and `/usr/local/bin` and writes the PATH block to `~/.bashrc`, `~/.profile` and `~/.zshenv`; `urnet-tools update` repairs older installs.
- **Proxy credential rotation on re-paste** (<https://github.com/full-bars/sn/pull/13>): pasting an address that already exists with different credentials now rotates the running proxy instead of silently keeping the old credentials; all duplicate entries for an address are scanned before an add is skipped.
- **Docker idle-update poll is bounded** (<https://github.com/full-bars/sn/pull/12>): a hung Docker daemon can no longer stall the idle wait past its own timeout.

### Changed

- **Runtime decoupling**: `proxy audit` is an independent runtime feature with its own control socket actions (`audit on|off|status|release`) and does not depend on `self-heal`. `self-heal` remains dedicated to resource-pressure actuators.
- The systemd status line (`urnet-tools status` on Linux) counts proxies held by proxy audit as intentional: `active: 47/50 proxies authenticated, 3 parked by proxy audit` instead of `partial`, and notes when audit is paused.
- The paid-grade header comments no longer claim grades are never consulted by anything that changes a proxy: trim shed ranking and proxy audit read them.
- **HotSwap is no longer described as zero-downtime.** Measured on a live node, a hotswap removes the 2 to 3 second window with no provider process, but the old process drops its proxy connections at the handover and the new one rebuilds them over about 30 s, the same ramp as a restart. Earlier entries that say "zero-downtime" describe the process handover only. See `docs/HotSwap.md`.

