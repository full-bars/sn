# v2026.10.23 (draft, sn)

**Status:** ready to cut. Nothing is tagged or pushed. Base tag `v2026.10.22-1078667780`. Head: `main` as of the merge of PR 159 (`c158769aa`). PR 160 (upstream sync of `urfoundation/sn` `c9d96068`) is open and is not covered here.
**At the cut:** the tag is `v2026.10.23-<build>` and the build number derives from the tagged commit's time, so compute it at the cut, rename this file to `releases/<tag>.md`, and replace TAG-TBD and DATE-TBD below. That rename is the same one `docs(release): name the notes for the cut` did last time.
**Sourcing:** condensed from the merge commits and PR bodies in this window. The sn-own items were checked line by line against the delta in the pre-release review (progress entry #4713). Not yet confirmed on a live node.

Legend: **[redeploy]** needs the built artifact redeployed. **[installer]** needs the installer or unit refresh. **[no redeploy]** docs, CI or tests only.

This release moves the two libraries the provider is built on. The connect library goes from the fork's `42a8c3348` to `6eeb86ec7`: the synced head `3b9bbd5bb`, which carries all of upstream connect up to `df0482798` plus four new fork commits in the transfer layer, and then the dial observer the proxy-leg evidence below rides on. The sdk goes from `0fefcf85f57b` to upstream's `96c2b6eb`, 27 commits later. The two have to move together: the newer connect removed APIs the old sdk used, and the newer sdk needs the resident move code the newer connect adds. Most of what is below ships through the two pin bumps; sn's own code changes are the URL warmup fix, the trim and lifetime reporting, the proxy-leg evidence, the extender measurement key and the Windows custody repair hints, each with its own PR link.

> [!NOTE]
> Most of what this release brings is transfer-layer correctness, refreshed blocklist data and clearer lifetime counters. The network-change and discovery changes are aimed at devices that move between networks and at the sender-side provider window, so a fixed-address server node will mostly not exercise them. The four fork commits are inert by default; see the retry budget entry.

## URL proxy sources

- **URL proxies launch again when the warmup hold expires** **[redeploy]** (<https://github.com/full-bars/sn/pull/157>). On a node whose registered pool contains proxies that never authenticate, the warmup ratio never reached its 90% gate, so the only exit was the 60-minute backstop: every qualified URL-sourced proxy sat deferred for an hour while the running pool drained, and the URL fetcher was parked behind the same flag, so the list never refreshed either. LA6 showed this as `53 deferred (warmup=53)` for a full hour and then one launch burst at the backstop. The reload gate shipped with a five-minute grace earlier; this release bounds the fetcher's wait the same way, and the reload now fires when the grace expires on its own clock, so replacements launch a few minutes after a start instead of an hour later.

## Observability

- **The lifetime counters say when they began** **[redeploy]** (<https://github.com/full-bars/sn/pull/152>, <https://github.com/full-bars/sn/pull/155>). The `[lifetime]` and `[pqe]` lines printed an `all-time` total with no start date, so a store that began this morning and one counting for months read the same. The lifetime store now persists a start stamp, written once when the store is created and never rewritten, and the lines read `all-time (since 2026-10-22 13:04 PDT)`. A store written before this release keeps its totals but has no recorded start, so its start is pinned to the load time, saved at once so an idle node does not re-pin a later date on every restart, and the line says the start is approximate and the totals include earlier history instead of inventing a date. The process-scoped figures, `since-start` on the `[pqe]` line and `lifetime_recovered` and `lifetime_lost` on `[health][proxies]`, now carry their own start time, so the two scopes can no longer be confused. The scrape gains `urnet_lifetime_start_timestamp_seconds` and `urnet_lifetime_start_approximate` (0 exact, 1 approximate), so a dashboard can show what the log shows.

- **Proxies that cannot carry traffic are counted and flagged** **[redeploy]** (<https://github.com/full-bars/sn/pull/159>). A proxy can pass its SOCKS login and still be unable to reach the platform, with the dial dying inside the proxy's own connect step. Until now that surfaced only as a flattened `http request attempts exhausted` error filed under timeouts, so the auth counters drove the wrong remedies while the proxy parked its client in a one-minute retry loop and status still counted it as authenticated. The connect library now reports the typed result of every control dial, and a failure inside the proxy's own connect step is its own category: `proxy-leg:N` on the `[health][proxies]` line and `urnet_proxy_leg_failures` in the scrape, never an auth or timeout failure. In shadow mode a proxy with at least 5 such failures in a row over 10 minutes and no platform dial succeeding through it, while some other path reached the platform in the last 5 minutes, is flagged once and counted. Nothing is shed; the flag is evidence.

## Operator tools

- **`urnet-tools` reports the proxy trim cap** **[redeploy]** (<https://github.com/full-bars/sn/pull/151>). With no argument `urnet-tools proxy trim` reports the effective cap and which source set it (operator, OOM or thrash), the configured pool size before the cap, how many proxies the cap holds back and the live counts, and `urnet-tools status` carries a `trim` row whenever a cap applies, so a capped node does not read as a small one. The provider's effective cap wins over the value in the file, and trim state is reported only from a complete pool. See the Proxy Management and urnet-tools pages.

## Windows

- **The registration custody repair hints work on any system language** **[redeploy]** (<https://github.com/full-bars/sn/pull/146>). A refusal that named a repair used `SYSTEM` and `Administrators`, group names icacls cannot resolve on a localized Windows. The hints now grant and remove by well-known SID (`*S-1-5-18`, `*S-1-5-32-544`), so the printed command works as pasted everywhere.

## Transfer reliability

- **Zero-window flows recover** **[redeploy]**. Flows that stall on a zero receive window are recovered, and transfer recovery ownership is preserved through the recovery.
- **ACK overflow wake-ups are coalesced** **[redeploy]**. The wake for an acknowledgement overflow no longer blocks its worker.
- **Receive eviction feedback wake-ups and discovery targets are fixed** **[redeploy]**.
- **Continuous receiver head evidence is preserved across mixed ACK clocks** **[redeploy]**.
- **Busy channels are kept through probe expiry and admission refusal** **[redeploy]**. Busy channels are preserved when a probe expires before it is written or local admission refuses it, probe custody is preserved, and native packet admission is isolated.
- **Extra application pacing is disabled** **[redeploy]**, while delivery windows are preserved.
- **Cached WebRTC answers are replayed for matching repeated offers** **[redeploy]**.

## Network changes and discovery

- **A network change re-dials at once** **[redeploy]**. A dial in flight is cancelled and retried on the new path. A young connection that was kicked re-dials without waiting out the reconnect timer, and the old path's backoff is dropped from the pre-dial wait. Kick re-dials use their own slots (`KickReconnectLimit`, 12) instead of queueing behind the fourth exit.
- **A dial the network change cancelled is no longer a backend failure** **[redeploy]**. It says nothing about the platform, so it no longer counts toward the degraded gate.
- **The window wakes on a network change** **[redeploy]**. Each window's resize loop wakes, its platform-error backoff starts over, and the retry waits end. After an outage the first round on the new path no longer waits out a backoff grown while there was no path. A rate-limit backoff is the platform's answer and is kept.
- **Pacing is remeasured on every network change** **[redeploy]**. A hard change used to pass through the quiet interval meant for signal hints, so a switch right after a Wi-Fi hint kept the old link's pacing estimate. Each quality coalescer now lets one measurement through per path generation.
- **Discovery paces itself** **[redeploy]**. Window expansion slows after unproductive passes and ends on a network change or a lost exit. A window asks first for the exits it lost to an outage, reuses discovery identities no provider has seen, and admits fresh providers after silence inference.

## Protocol

- **Resident moves: the platform can direct a transport to an exact edge address** **[redeploy]**. The migrate message gains `move_id`, a kind (drain, advisory or compulsory), per-family target addresses, `close_time` and `prefer_duration`, so the platform can send a client to a specific edge address while it keeps the platform hostname. Auth gains `resident_move_version`, the capability the platform checks before it sends an advisory move, and a new result frame lets a client report a decline or failure. Window transports follow a move, a strict move dials its target direct and is refused before the dial on a relay, so a relay's health is untouched, and a move never breaks a migration before it makes. A drain migrate still decodes as a drain, and older clients ignore the new fields and migrate by name.
- **The provider follows moves too** **[redeploy]**. The sdk moves the provider's transports at the platform's direction, never breaks a platform migration of the provider before it makes, and declines advisory moves while an app is backgrounded or on a metered link. A provider on the alternate path moves over H1 and keeps H3 on the alternate. This release changes none of sn's own settings, so whether a given provider advertises the capability follows the sdk's defaults.

## Extenders

- **A provider's extenders ride on its row** **[redeploy]**. Each provider row carries its extenders and addresses, a stayed-on exit is pinned to the extender that carried it, and the pin is kept through a resident move and saved with the stayed-on location.

- **The extender keeps its lag.dev measurement key beside its identity seed** **[redeploy]** (<https://github.com/full-bars/sn/pull/158>). The provider's space keeps no local state, so the extender role had nowhere to keep a measurement key and never asked the operator for a certificate. The provider now names `.provider.extender.measurement.key` in its state directory in the device's key material. The role writes the PEM private key there with mode 0600 when the file is absent, and it applies only to the direct slot, which is the only slot that runs the extender role. Back this file up with the rest of the state directory and treat it as secret.

## SDK

- **Device RPC limits come from settings** **[redeploy]**. The limits that were constants are read from the device settings, matching the connect change that moved tunables into settings.
- **Native packet reads are independent of remote admission waits** **[redeploy]**.
- **Explicit API-alternate policy for hosted H1 sessions, and `NetworkChanged` exported for processes without a device** **[redeploy]**.
- **Native network space identity is exposed across the SDK bindings, and crypto invoices are added** **[redeploy]**.
- **Go 1.27.3** is required across the SDK modules and documented for the embedded example.

## Fork changes in the transfer layer

- **Timeout resends earn a retry budget** **[redeploy]**. A destination that stops acknowledging used to make the sender rewrite its window every resend interval until the connection closed. Timeout resends now spend credit earned by frames that actually reached the wire, so a silent peer stops consuming while a slow live peer is unaffected. Credit is refunded for a resend that never went out, and recovery resends are counted at their own depth so gap and probe resends stop reporting the same number. **Shadow by default:** an unset govern mode is shadow, and the gate only refuses in enforce mode, so this release changes no behavior until an operator turns enforcement on.
- **The retained-ack timeout is configurable** **[redeploy]**. How long a destination may stay silent before its retained owners are failed used to be a fixed value. It now comes from configuration, zero keeps the shipped behavior, and the defaults carry it through unchanged.

## Data

- **IP security and blocker tables refreshed** **[redeploy]**. The blocklist and CFAA data are updated to upstream's `2026.10.22-1078873780` build, across the several refreshes published since the previous release. The packed tables are now emitted as single string constants, which removes about 21,000 lines of generated source.

## Build

- **Go 1.27.3 is required** **[redeploy]**. The active connect and sdk modules now declare it as their floor, matching the toolchain this release is built with.

## Docs and CI

- **The README is rewritten for this fork** **[no redeploy]**. It now leads with what to run from here (miner container and provider install) and keeps the subnet mechanism material below, instead of opening with upstream's roles diagram.
- **The live-numbers job signs its commits** **[no redeploy]** (<https://github.com/full-bars/sn/pull/154>). The scheduled SN25 cost-table refresh imports the CI signing key and refuses to push an unsigned commit, and the wiki mirror signs too.

## Deploy notes

- **The provider needs a redeploy.** Every change above is in the built artifact. There is no installer or unit change.
- **Upstream's own installer scripts are not part of this release.** The merged range also contains rewrites of upstream's Linux, macOS and Windows installer scripts and their `urnet-tools`. sn ships its own installer and does not use them.
- **Build with the sn version symbol:** `-X main.Version=<tag> -X main.VersionStamp=URNET_VERSION_STAMP=<tag>`, as the release workflow does.

## Caveats

- Three connect tests fail on the fork's own main, before this sync, and pass on upstream alone: `TestControlDialNetworkNeverNarrowsAnIPLiteral`, `TestSmartDialerOrdersTheSerialPath` and `TestSmartDialerOrdersTheParallelPath`. They are not caused by this release, and the cause is fork-side code. They are being diagnosed.
- The retry budget is untested in enforce mode in production. Only the shadow default is what ships.
- The five-minute warmup grace is proven by unit tests and full provider test passes in both repos. The LA6 confirmation (the one-hour deferral becomes a launch a few minutes after the start) has not been recorded yet.
- PR 160 (upstream sync of `urfoundation/sn` `c9d96068`) is open and not covered here. If it merges before the cut, this file and the changelog need entries for what it brings.
- Nothing unmerged is covered here beyond that. If the in-flight log and metrics work merges before the cut, this file needs a section for it.
- Production result: PRODUCTION-TEST-TBD. Do not publish this line until a node has run the tagged build.

**Full Changelog**: https://github.com/full-bars/sn/compare/v2026.10.22-1078667780...TAG-TBD
