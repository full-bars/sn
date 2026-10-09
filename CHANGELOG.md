# Changelog

## [Unreleased]

### Added

- **The provider captures goroutine and heap profiles on the way into a stall**: it samples its goroutine count and its heap against the soft limit every 10 seconds and, on a build-up (1.5 times the 30-minute low and at least 20,000 goroutines, heap at 90%, heap at 120%), writes the biggest goroutine stacks, the full goroutine profile, a heap profile and the numbers to `~/.urnetwork/incidents/`. At most 6 a day, newest 8 kept, disk only. `URNETWORK_INCIDENT_CAPTURE=0` turns it off.

- **A swap-thrash watchdog with a supervised restart** ([#77](https://github.com/full-bars/sn/pull/77)): memory pressure alone does not say "thrashing". The watchdog senses PSI memory `full`, swap activity, page refaults and direct reclaim, and tracks one state: `calm`, `under-pressure`, `thrashing`, `critical`. It first freezes pool growth; when the condition persists it restarts the provider by exiting with status 75, which the shipped unit (`Restart=on-failure`) turns into a supervised restart. The restart is capped at 3 per 24 hours with a growing backoff, is skipped while a hot-swap is in flight, and refuses to act when the swap belongs to another process. A restart leaves a thrash cap in `~/.urnetwork/thrash_cap.json` so the next start begins leaner — about 60% of the previous running count, at least one proxy, and no new cap when nothing was running before the restart. It rides the existing self-heal switch; with self-heal off it senses and logs but never restarts anything.
- **`urnet-tools status` prints the pressure summary sentence**: the live block's pressure row shows the provider's one-sentence summary (the `summary` the provider persists to `~/.urnetwork/pressure_status`), instead of only the score. Older providers keep the bare score.
- **The installer warns when a restart-policy override weakens the watchdog**: a drop-in with `Restart=no`, `Restart=on-success`, `Restart=on-abnormal`, `Restart=on-watchdog` or `Restart=on-abort`, a `RestartPreventExitStatus` listing 75, or a `SuccessExitStatus` marking 75 a success under `Restart=on-failure` would leave a detected thrash unrecovered; the installer now says so. The policy itself is never rewritten.
- **A container can opt in to the thrash watchdog's exit 75 restart** ([#84](https://github.com/full-bars/sn/pull/84)): the watchdog's supervised restart used to be allowed only under systemd, so a container always declined it. A container (`/.dockerenv` or `URNETWORK_CONTAINER=1`) now counts as a supervisor only when `URNETWORK_EXIT75_OK=1` is also set and the state directory holding `thrash_cap.json` can be written and read back. Without the ack, nothing changes: the escalation is still refused as `no-supervisor`. With an unwritable state directory it is refused as `persist-failed`, so a self-exit cannot loop without its throttle record. The write probe is bounded by the same timeout as the cap write, so a hung volume cannot stall the watchdog. The image default stays unset.

### Fixed

- **A provider that is alive but stalled is now restarted**: a unit that sets `WatchdogSec=` is fed `WATCHDOG=1` only while the pressure monitor keeps ticking, so after 10 quiet minutes systemd restarts a provider whose garbage collector has left no goroutine able to run, and a lean start cap (60% of the running proxies) is recorded first; and the thrash watchdog counts a heap at least 1.4 times its soft limit on a box short of free RAM as severe on its own. The feed starts at READY with a 30 minute startup grace, follows the self-heal switch and the thrash restart budget (3 per 24 hours), keeps a standing cap instead of compounding it, and takes the cap back out if the stall resolves itself. A heap-runaway episode on a box without PSI now relaxes when the heap recovers. The watchdog is off unless the unit sets `WatchdogSec=`.

- **A failed renewal replaced a client identity** ([#82](https://github.com/full-bars/sn/pull/82)): when a stored client token had expired and renewing it failed, `provideAuth` minted a new client and abandoned the old client id and the history attached to it, even when the failure was only a timeout or a dropped connection. A renewal that fails without a platform verdict (a transport error, a 5xx, a 408, a 429, a cancelled context) is now transient: the identity is kept and the caller's backoff retries. A definite answer (the client does not exist, a mismatched client id, a permanent 4xx) still mints a fresh client as before.
- **`urnet-tools` did not run on Windows** ([#79](https://github.com/full-bars/sn/pull/79)): the log views ran `tail -n 20 -f`, which Windows does not have, `urnet-tools start` set the provider's working directory before the state directory existed so Windows refused to launch it, and the sudo checks had no Windows branch. The commands now run natively. A native Go follower prints the last lines and then streams new output, and it is the fallback wherever `tail` is missing. It opens the log with delete sharing, so a rotation, a clear or an update is no longer blocked by a sharing violation. If the path starts naming a different file, the follower finishes the old one and follows the new one from its start, and a truncated log is followed from its start. It also opens paths longer than `MAX_PATH`, and a 64 KiB tail window that starts exactly on a line boundary keeps its first line.

### Changed

- **The provider's operator logs are readable sentences** ([#77](https://github.com/full-bars/sn/pull/77)): the pressure regime, pool target, trim receipt and apply, reload summary, health verdict, auth limiter and GC governor lines say what happened in plain words, with machine counters kept in a trailing `(...)` for grep. The new `[health] Verdict:` line summarizes state in one sentence. Dialer and connect lines are unchanged.
- **The container start scripts treat exit 75 as a planned restart** ([#84](https://github.com/full-bars/sn/pull/84)): `start_stable.sh`, `start_nightly.sh`, `start_jwt.sh` and the provider loop of `pelican_panel.sh` counted 75 as a crash, which counted toward the three-crash JWT clear and slept 60 seconds. It now restarts after 5 seconds and leaves the JWT alone. Every other status is unchanged.

## [v2026.10.6-1064742750-meso] — 2026-10-06

### Added

- **Proxies are identified by account, not just by address** (<https://github.com/full-bars/sn/pull/30>): a shared-gateway proxy provider hands one `host:port` to several accounts, and the account decides the backend IP. Every store that tracked a proxy by address alone now tracks it by identity (the address plus the user when the proxy is authenticated), so two accounts at one gateway no longer collapse into a single entry. That covers the proxy state file, the client-JWT store, the health registry, the earnings and grade lookups, the audit and trust decisions, and the add/rotation semantics. A credential rotation keeps the same identity on purpose, so a restart still reuses the existing client identity instead of resetting its reliability reputation.
- **Live traffic, runtime internals and a reworked `top` view** (<https://github.com/full-bars/sn/pull/31>): the live status block separates billable from total traffic, so non-billable bytes (a direct socket, for instance) are visible instead of folded into one number, and session totals follow proxies being removed or respawning. `top` gains a zoomable graph, a runtime panel (goroutines, heap, descriptors), a theme/graph menu, and a layout that adapts to a small terminal. The provider answers three new light control-socket commands (`traffic`, `internals`, `goroutines`), so the 100ms poll no longer rebuilds a full snapshot; an older provider is detected and falls back to the snapshot's own rates.
- **A stated reason for the node's state** (<https://github.com/full-bars/sn/pull/31>): `starting` and `degraded` now say why: still resolving proxies, a source that could not be read, a source that returned nothing, or how many proxies are dead against how many are configured.
- **A built-in baseline recorder** (<https://github.com/full-bars/sn/pull/42>): every provider keeps a small local record of its own behaviour in `~/.urnetwork/baseline.jsonl`, on by default and with no setup, so an upgrade can be judged against what the box did before it. Counts and totals only, never proxy addresses, usernames or passwords, and an unmeasured field is omitted rather than written as zero. `urnet-tools baseline show|mark|compare` reads it, the comparison derives its rate from the lifetime counter's delta, excludes the ramp after every start from both sides, and warns when capacity changed. `set baseline off` stops recording without a restart and keeps the file. It starts with this release, so there is no earlier record on a box upgrading from an older one. See [Baseline recorder](docs/Baseline.md).
- **H3/QUIC now travels through the proxy** (<https://github.com/full-bars/sn/pull/34>): an identity that won the H3 race used to reach the platform from the host's own address, because the transport opened a UDP socket on the host. QUIC is now relayed through the identity's SOCKS5 proxy with a UDP association, so the platform sees the proxy's address. The proxy's real address plus its reported port is the relay, and the reported address is never trusted. Only datagrams from the relay are accepted, the association ends with its control connection, and a proxy that will not relay UDP is an error rather than a fallback to a host socket, so the transport falls back to a TCP mode through the proxy. See `provider/h3_packet_conn.go`.
- **The proxy's UDP path is checked before H3 uses it** (<https://github.com/full-bars/sn/pull/34>): the grade and MITM probes only see the TCP path. Before H3 goes through a proxy's UDP relay, the relay is checked once per identity. It must answer, and the address a STUN server sees through it must equal the address an address service sees over TCP through the same proxy. A dead relay or a different exit disables H3 for that identity. The result is keyed by proxy identity, remembered for 30 minutes, limited to 8 concurrent checks, and forgotten after two lifetimes. See `provider/proxy_udp_check.go`.
- **The smart dialer probes every transport in the background** (<https://github.com/full-bars/sn/pull/35>, <https://github.com/full-bars/sn/pull/61>): the provider measures every transport with background probes and chooses a transport from the measured cost. The probe runs only after auth, behind a global limiter that yields to auth, and the log carries the numbers behind the choice.
- **The self-management stack, ported from the mature fork** (<https://github.com/full-bars/sn/pull/38>): `urnet-tools` gains the autopilot, oom-cap and smart-dialer controls, and the provider records its own actions in an action ledger.
- **Dashboards and alerts get the metric families they query** (<https://github.com/full-bars/sn/pull/41>): the provider exports the families the Grafana dashboards and alert rules read, so the panels no longer depend on a removed producer.
- **Host observability and host memory** (<https://github.com/full-bars/sn/pull/48>, <https://github.com/full-bars/sn/pull/49>): the provider records GC state, conntrack fill and TCP socket states, and it reads host memory on Windows and macOS as well as Linux.
- **The H3 identity set and QUIC DATAGRAM are gated by live control keys** (<https://github.com/full-bars/sn/pull/66>): the `h3` setting chooses which identities run H3. It takes `off`, `direct`, a proxy count, or `all`. It applies to the running provider with no restart. `on` is an alias for `direct`, and `0` aliases `off`. The default is `all`, which is what sn did before this setting existed. A change reconnects only the identities that join or leave the set. A bare integer caps the proxy set only. The direct identity is always additionally eligible and never consumes one of the N. So `h3=10` runs ten H3 proxies plus the direct identity. Two settings gate QUIC DATAGRAM. `h3-datagram` gates the receive and offer lane. `h3-datagram-send` gates the send lane. Both default off and both apply live. The offer is read at H3 dial, so a change re-dials the connection. The send threshold is read per message, so a change is immediate. One predicate, `snH3Eligible` in `provider/h3_mode.go`, decides eligibility. The `[health]` line carries `h3=<mode> h3_set=<n> h3_proxies=<m>` and the datagram counters. `/metrics` carries `urnet_h3_mode_info`, `urnet_h3_set_size`, `urnet_h3_proxy_set_size` and the `urnet_h3_datagram_*` families. The offered, accepted and blackhole counters are not exported, because the pinned connect revision cannot count them. See `docs/Configuration.md` and `docs/Monitoring.md`.

### Changed

- **The dropped bucket is labeled on its own, and the idle hint reads again**: the live status and `top` view called the was-up-now-down bucket `down`, the same word the whole-pool figure uses for dead plus dropped. The breakdown bucket is `dropped` now, matching the proxy health report, so `down` has one meaning. The idle hint no longer calls every offline proxy dead. Labels and wording only; the reaper inputs are unchanged.
- **A node that deliberately runs with no proxies now reads `active`, not `degraded`** (<https://github.com/full-bars/sn/pull/31>): serving on the direct transport with no proxy source configured is a valid, completed configuration, and was previously indistinguishable from a real outage. A configured source that came back empty still reads degraded, and so does a node whose direct transport is off with nothing to serve.
- **The idle hint blames auth only when it explains the idleness** (<https://github.com/full-bars/sn/pull/31>): a steady trickle of auth retries on a large healthy pool is no longer reported as an auth outage. Auth is blamed for a failure wave, or when most of the pool is not connected while failures are happening; otherwise the hint states what is true and shows the numbers behind it.
- **The reload summary says where additions came from** (<https://github.com/full-bars/sn/pull/31>): the `reloaded: +N added` line breaks the additions down by source, and URL-sourced launches get their own line instead of being folded into a bare count.
- **Proxy keys are never printed raw** (<https://github.com/full-bars/sn/pull/30>): an identity key embeds the account, so operator-facing listings show a display form instead of the raw key.
- **The build uses upstream modules instead of the stale forks** (<https://github.com/full-bars/sn/pull/36>, <https://github.com/full-bars/sn/pull/37>): `sdk`, `glog`, `goidenticons`, `server`, `warp`, `operator-proxy`, `proxy` and `userwireguard` now resolve to the upstream `urnetwork` modules instead of the fork's own copies.
- **The `connect` pin moves to the 5 October build** (<https://github.com/full-bars/sn/pull/54>, <https://github.com/full-bars/sn/pull/55>, <https://github.com/full-bars/sn/pull/56>, <https://github.com/full-bars/sn/pull/60>, <https://github.com/full-bars/sn/pull/64>, <https://github.com/full-bars/sn/pull/65>): the pin advances several hundred engine commits, and it carries the retrying out-of-band contract send, the contract verification reason log, the unconditional successor pre-buy, and the canceled-dial classification.
- **H3 datagram gating changes the datagram default, a change from the previous silent default** (<https://github.com/full-bars/sn/pull/66>): before this release sn offered QUIC DATAGRAM silently and by default. It built from connect's own settings, and nothing in `provider/` overrode it. The pinned library had no send gate, so small frames also went out as datagrams on their own. Now both lanes are gated, and `h3-datagram` and `h3-datagram-send` default off. After an upgrade an H3 connection no longer offers DATAGRAM unless you turn it on. Run `urnet-tools set h3-datagram on` to offer it. Both keys apply live.
- **Proxy health now says `down`, `dropped` and `never up` in place of `degraded` and `dead`** (<https://github.com/full-bars/sn/pull/67>): the proxy health report headline shows `down` as one total with its two parts named beside it, `dropped` for a proxy that was up and then went down, and `never up` for one that never connected, instead of the old `Dead` and `Degraded` pair that double-counted the dropped bucket inside `Down`. The report `STATUS` column and the `proxy_health.log` rows say `DROPPED` and `NEVER UP`. The `top` view and the live status block say `down` in place of `degraded`. The snapshot state reason reads `N of M proxies down`. The report and the `top` view now agree on the word. The behaviour and the proxy pruning are unchanged: the reaper predicate and every internal identifier are byte-for-byte identical, and `degraded` stays for the systemd status band that measures how much of the pool is live. See [Proxy management](docs/Proxy-Management.md) and `LOG_REFERENCE.md`.

### Fixed

- **A proxy-pool reload now re-applies the H3 identity set** (<https://github.com/full-bars/sn/pull/69>): a reload that re-resolved the `h3` cap stored the new resolution but never re-applied it to the running proxies, so a proxy displaced from the top N kept running H3 and a promoted proxy stayed on H1. The reload now re-applies the resolved set on the path `SetH3Mode` uses, so a proxy starts or stops H3 as soon as it enters or leaves the cap, instead of waiting for the next control change. The mode swap is also a compare-and-swap against the pointer that was read, so a concurrent control update such as `h3 = off` is no longer overwritten by a stale re-resolve, and the re-resolve is a no-op when the eligible set is unchanged.
- **Billable bytes were the socket byte count, so they always equaled total** (<https://github.com/full-bars/sn/pull/62>): the connection wrapper incremented billable and total by the same amount, which made billable the bytes on the relay-egress socket (and, when H3 won, the tunnel too), not the traffic relayed for clients. Billable now comes from the engine's own relay accounting for each proxy (`RemoteIngressByteCount` as `BillableTx`, `RemoteEgressByteCount` as `BillableRx`), the same IP-packet bytes the previous stable release line counts in its user NAT provider. Expect a step DOWN in billable, in the billable rate, in the earnings ranking that the trim reads and in the Grafana billable panels at deploy: benchmark, probe and H3 tunnel bytes no longer count. The platform's own payout is unaffected. Total is now every byte on the sockets that go through the proxy: relay egress, the provider's own API, auth and H1 tunnel connections (counted by a total-only wrapper that never touches clients or billable, and leaves a direct identity's dial untouched), and the H3 tunnel socket when H3 wins, the same quantity the previous stable release line reports; expect total to step UP at deploy. The platform's own per-contract usage is exposed as `urnet_contract_used_bytes_total{direction}` as a cross-check against billable.
- **Two accounts at one gateway no longer overwrite each other** (<https://github.com/full-bars/sn/pull/30>): the stores were keyed by identity but several consumers still read a bare address, so a shared-gateway proxy silently merged the accounts. The earn tracker, the launch generation, the degraded-proxy reaper and the grade report now all use the identity. Credentialed proxies were previously invisible to the reaper, could report to the hub as ungraded, and could be left stuck in the cancel map after a failure.
- **A credentialed URL proxy is no longer counted twice** (<https://github.com/full-bars/sn/pull/30>): the match collector added it under its identity key and again under its bare address, so it appeared twice in `display` and inflated the `proxy remove --match` count.
- **A removed proxy's login is dropped** (<https://github.com/full-bars/sn/pull/30>): the client-JWT store migrated legacy keys but never pruned, so a removed proxy's login stayed on disk for the store's whole retention window and a re-add at the same address inherited a JWT minted for the old account. A rotation (same identity, new password) deliberately keeps its login.
- **A file source that cannot be read is reported as a failure** (<https://github.com/full-bars/sn/pull/31>): it left the resolution pending, so the status line read `starting: resolving proxies` and the snapshot later read it as a stuck startup. Running proxies are deliberately left alone.
- **A proxy source URL cannot leak its token into the logs** (<https://github.com/full-bars/sn/pull/31>): the per-source log lines and the operator warning use a redacted label that strips the query, the userinfo, and credential-bearing path segments, so a source like `.../token/SECRET/list` no longer reaches the important log or the warning, and one source no longer produces two different warning keys.
- **A no-source node stops claiming it is retrying** (<https://github.com/full-bars/sn/pull/31>): that configuration has nothing to retry, and the reason now matches the systemd status line.
- **The `top` menu now persists** (<https://github.com/full-bars/sn/pull/31>): the menu wrote through a settings path that nothing ever set and never loaded it back, so every choice was silently discarded and the theme reset on the next run. A theme forced by the environment (`NO_COLOR`, a dumb terminal) is not saved, so it does not outlive the terminal that forced it, and a config directory created by a first run under `sudo` is handed to the invoking user instead of staying root-owned and blocking their next save.
- **Live relay-egress connections count as client sessions** (<https://github.com/full-bars/sn/pull/57>): the session counter now reflects the connections that are actually carrying client traffic.
- **Proxy health follows the real transport connection** (<https://github.com/full-bars/sn/pull/58>): a proxy's health is driven by its real connection state, not by a stale signal.
- **The audit ring re-merges and persists after a hotswap drain** (<https://github.com/full-bars/sn/pull/43>): the successor re-merges the ring as a timestamp-ordered union after the parent drains, so the commands issued just before an update are neither lost nor duplicated.
- **`set baseline off` now stops the recorder** (<https://github.com/full-bars/sn/pull/44>): the command used to be sent in a form the recorder ignored. It now arrives as a value, so the recorder actually stops.
- **The pressure control no longer chases its own tail** (<https://github.com/full-bars/sn/pull/46>): the memory controller used to sawtooth against the GC governor. The control now stops reacting to its own effect.
- **The pressure, pool and reaper loops are supervised** (<https://github.com/full-bars/sn/pull/47>): a failed loop is restarted and a failure stays neutral, instead of taking the node down with it.
- **A slow proxy auth dial is no longer a failure** (<https://github.com/full-bars/sn/pull/32>): a slow auth dial was counted as a dead proxy. The provider now waits within a tolerance, and the transport choice reacts to measured cost.
- **The saved client login is looked up by proxy identity** (<https://github.com/full-bars/sn/pull/53>): the lookup used a bare address, so two accounts at one gateway could share the wrong login. It now uses the identity.
- **Parity fixes ported from the mature fork** (<https://github.com/full-bars/sn/pull/59>, <https://github.com/full-bars/sn/pull/45>): the small identity-key, audit and bandwidth differences are closed, the slow-retry semaphore survives a real give-up, and a source URL can no longer leak its token into a fetch error.
- **The OOM marker and retention log** (<https://github.com/full-bars/sn/pull/40>): the OOM start marker is written in every mode, and the retention log reopens after rotation. Linux-only tests stay out of the macOS and Windows test builds.

### Maintenance

- **The sn line is rolled onto current upstream, the nativefee era** (<https://github.com/full-bars/sn/pull/64>): this is the large sync in the range. It brings the `nativefee`, `chain`, `evmrpc` and `diagnostics` trees, an expanded `mainnet`, `sim-testnet`, `validator` and `miner` set, the qualification and mainnet release-build scripts, and a vendored `third_party/go-substrate-rpc-client`. This sync is the subnet, validator and miner side. It does not touch `provider/`, so it does not change how the provider relays traffic.
- **Provider log lines use one set of emoji markers** (<https://github.com/full-bars/sn/pull/63>).

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

Initial release, replaced by v2026.9.17 with additional fixes.
