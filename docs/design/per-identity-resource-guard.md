# Per-identity resource guard: feasibility and harness

Task D. Question: can a deterministic Go test in sn measure goroutines and heap
per proxied provider identity without a live platform, so that a regression
like "three platform transports and five connection strategies per identity"
fails in CI?

Sources read (every citation below is at these revisions):

- sn: branch `claude/d-per-identity-resource-guard`, base `20353851`.
- sdk: `github.com/full-bars/sdk@v0.0.0-20261025034911-5dfc8a7c2a50` (the go.mod
  replace at go.mod:226), module cache copy.
- connect: `github.com/full-bars/connect@v0.0.0-20261025034911-49d75e264480`
  (go.mod:241), module cache copy.

## Verdict

Feasible for the device half of the per-slot function, with no production
change. Every dependency the slot's device needs already has a seam. The
run-level half (client JWT acquisition, admission gate, wallet, progress,
hot swap) is not reachable from a test without running the whole
`runProvider`, and it is not where per-identity transports and strategies are
built, so the harness replicates the device half instead and records the drift
risk (section "Limits").

## What one proxied slot builds (sn run.go, 20353851)

`provideWithProxy` (miner/run.go:1430-1803) does, per identity:

1. `slotClientStrategySettings` (run.go:1466, defined run.go:2146-2180): default
   strategy settings, `ProxySettings` set, `DisableExtenderNetwork = true` for
   a proxied slot (run.go:2163), and `ConnectSettings` rewritten by
   `networkSpaceConnectSettings` (miner/h3_packet_conn.go:154-164), which calls
   `bandwidth.WrapConnectSettingsTotal` (miner/bandwidth/wrap.go:110-115). For a
   proxied slot that wrap captures the SOCKS5 dial as the base dial
   (wrap.go:145-146) and installs it as `DialContextSettings`.
2. Strategy 1: `connect.NewClientStrategy` for the run's own dials
   (run.go:1492).
3. Strategy 2: `sdk.NewNetworkSpaceWithUrls(ctx, apiUrl, connectUrl, ...)`
   (run.go:1503; sdk network_space.go:1277-1384) builds the space's strategy
   and its api (network_space.go:1331-1332).
4. Auth: `provideClientJWT` (run.go:1562-1578). Run-level, see Limits.
5. `startSmartDialerIdentity` (run.go:1623): registers data in the process
   scheduler, no per-identity goroutine (miner/smart_dialer_scheduler.go:424-442).
6. Device: `newProviderDeviceLocal` (run.go:1709-1721; miner/provider_work_capture.go:316-412)
   calls `sdk.NewDeviceLocal` (provider_work_capture.go:385). With
   `AllowProvider` the sdk builds `newDeviceLocalProviderWithOverrides`
   (sdk device_local.go:1636-1666), whose transport is
   `newProviderPlatformTransport` (sdk device_local_provider.go:493-519) =
   `connect.NewFamilyPlatformTransportGroup` (connect transport_family.go:1187-1293).
7. The group builds, when the family urls derive:
   - strategy 3 and transport A: `NewDirectClientStrategy(..., 4)` + v4 pinned
     `PlatformTransport` to `connect-v4.<domain>` (transport_family.go:1246-1257);
   - strategy 4 and transport B: same for v6 to `connect-v6.<domain>`
     (transport_family.go:1258-1269);
   - transport C: the family-agnostic standby to `connect.<domain>`
     (transport_family.go:1271-1285), on strategy 5 when the provide mode is
     public (`standbyClientStrategy`, sdk device_local_provider.go:395-420,
     another `NewDirectClientStrategy`), otherwise on the space strategy.
   The family urls derive only from a dotted host name whose first label is not
   already `-v4`/`-v6` (sdk network_space.go:1656-1688, `familyServiceUrl`); an
   ip-literal platform url gives no pinned transports and the group degrades to
   the single standby ("with no pinned transport at all the group is the legacy
   single transport", transport_family.go:1273-1275).
8. Per-slot watchers: `watchProxyTransportHealth` (run.go:1740),
   `watchProxyBilling` (run.go:1754), `registerContractOutcomes` (run.go:1765).

So the "three transports, five strategies" shape is exactly items 2, 3, 6, 7.
A harness that wants to see it MUST use a dotted platform host name, not
`127.0.0.1`, or the group never builds the pinned pair and the regression is
invisible.

## Dependencies and their seams

| Dependency | Seam | Exists? |
|---|---|---|
| api URL | `sdk.NewNetworkSpaceWithUrls` apiUrl argument (sdk network_space.go:1277-1283); miner passes `self.apiUrl` (run.go:1503) | yes |
| platform URL | same function, platformUrl argument | yes |
| connect-v4 / connect-v6 host | derived from the platform host label (network_space.go:1623-1625, 1656-1688). Not configurable, but every TCP dial of a proxied slot goes through its SOCKS5 proxy by host NAME: connect's proxy path does not resolve locally ("the proxy resolves names on its side", connect net.go:254-258), the miner's wrap keeps that dial (wrap.go:145-146), and `NewDirectClientStrategy` clears `ProxySettings` but keeps an injected `DialContextSettings.DialContext` (transport_family.go:234, 250-253), which for a proxied slot IS the SOCKS dial. So the fake SOCKS5 proxy is the name seam: it maps every `*.<test domain>` name to the in-process server | yes (via the proxy) |
| DNS | `ConnectSettings.Resolver` (connect net.go:202); the sdk suite uses an offline resolver the same way (sdk main_test.go:46-63). Needed only for names that do not go through the proxy | yes |
| TLS verification | `ConnectSettings.TlsConfig` honored by the normal dialer; precedent miner/h3_provider_policy_test.go:146-150 (RootCAs of the test server) | yes |
| JWT auth | the device takes the client JWT as a string and parses it unverified (sdk local_state_client_startup.go:28-31); unsigned fixture token precedent miner/provider_registration_test.go:81-92 | yes |
| by-jwt client id | `newProviderDeviceLocal` clientId argument (provider_work_capture.go:316) | yes |
| extender | `settings.ProvideExtenderEnabled = proxySettings == nil` (run.go:1687) and `DisableExtenderNetwork` (run.go:2163); production already turns both off for a proxied slot | yes |
| DNS pump host | `DeviceLocalSettings.DnsPumpHost`; precedent h3_provider_policy_test.go:157-160 | yes |
| QUIC through the proxy | `newH3PacketConnFactory` (h3_packet_conn.go:71-95) runs the proxy UDP check first; a fake SOCKS5 that refuses UDP ASSOCIATE makes it fail closed, and `h3SlotTransportSettings(proxy)` keeps a proxied slot on H1 under the default h3 mode (h3_provider_policy_test.go:114-115) | yes |
| client JWT acquisition (`provideClientJWT`), admission gate, wallet | inside `runProvider`'s closure; needs keyOwner, state dir, slot identities | no seam (see Limits) |

The fake platform needs only a TLS websocket upgrade followed by a read loop:
the sdk's own provider tests keep a provider transport connected that way (sdk
device_local_provider_move_test.go:86-115, client_limit_status_test.go:86-104).

## Design of the harness

- One in-process TLS server (self-signed cert for `*.<domain>`) serving both
  the api (404 for everything, plus `/hello` 200) and the platform (websocket
  upgrade, then read until close). It records each upgrade's `Host` and the
  `X-UR-InstanceId` header, so it can count platform connections per identity
  per host (`connect.` vs `connect-v4.` vs `connect-v6.`).
- One fake SOCKS5 proxy per identity (distinct `127.0.0.1:port`, so identities
  are distinct proxy keys exactly as in production), CONNECT only, no auth,
  mapping any name under the test domain to the server and refusing
  everything else; UDP ASSOCIATE is refused.
- Per identity, the device half of `provideWithProxy`, using the same miner
  helpers in the same order: `slotClientStrategySettings`,
  `withProxyLegObserver`, `connect.NewClientStrategy`,
  `sdk.NewNetworkSpaceWithUrls`, `startSmartDialerIdentity`,
  `ProviderDeviceSettings`, `ProvideExtenderEnabled = false`,
  `providerSlotDialSettings`, `newProviderDeviceLocal` with
  `h3SlotTransportSettings(proxy)`, the three watchers, and
  `SetProvideControlMode(ProvideControlModeAlways)`. The only test changes to
  the settings: TLS roots, offline resolver, `DnsPumpHost`, and an unsigned
  client JWT.
- Settle by polling: the fake platform's connection count stops changing and
  `runtime.NumGoroutine` is unchanged across consecutive short polls (no sleep
  longer than a few hundred ms).
- Report: goroutines per identity (delta against a baseline taken before any
  slot), HeapAlloc per identity after `runtime.GC()` twice, the goroutine
  profile (`pprof.Lookup("goroutine").WriteTo(w, 1)`) grouped by the goroutine's
  entry function, and platform connections per identity per host.
- Detecting task E's change (a `DisablePlatformFamilyTransports` flag for proxied
  slots): with the flag on, the group should build only the standby. The
  harness sees that three ways: platform upgrades per identity by host
  (pinned `connect-v4.`/`connect-v6.` hosts go to zero), the count of
  `connect.(*PlatformTransport)` entry goroutines per identity, and total
  goroutines per identity. Bounds are asserted on all three; the measured
  values are recorded next to them.

## Limits

- The harness replicates the device half of `provideWithProxy`; it does not
  call the closure. A future edit to the closure that adds per-slot work is
  not seen until the harness is updated too. The smallest seam that would
  remove this drift is a production refactor: move run.go:1466-1505 and
  1666-1797 into a named function (inputs: proxy settings, bw registry, api
  and connect urls, client JWT and id, seed, memory target, h3 policy;
  outputs: device, strategy, space, and a cleanup), called by the closure and
  by the harness. Not built here (no production change in this task).
- Run-level per-slot work (`provideClientJWT`, the admission gate, wallet,
  progress attach, hot swap) is not covered. None of it constructs transports
  or strategies; `provideClientJWT` may build an api request on the slot's
  existing strategy.
- Production may set a per-device memory target (`applyProviderMemoryTarget`,
  run.go:845-853, from the run's memory plan). The harness leaves the sdk
  default, so its heap numbers are for the default target.
- Numbers are for an idle, connected provider with no clients, contracts or
  traffic.

## Results (harness built)

Test: `TestPerIdentityResourceGuard` in miner/per_identity_resource_test.go.
Skipped under `-short`; about 5 s of test time at N=50 (plus the package
build).

Command, run three times:

    go test ./miner/ -run 'TestPerIdentityResourceGuard$' -count=1 -timeout 300s -v

| run | settle | slot goroutines / identity (fakes excluded) | all goroutines / identity | heap / identity after GC | strategies / identity | transports / identity (pinned) | platform upgrades / identity (open) |
|---|---|---|---|---|---|---|---|
| 1 | 4.2 s | 49.5 | 63.5 | 820 KiB | 5.00 | 3.00 (2.00) | 5.18 (1.00) |
| 2 | 5.0 s | 48.8 | 61.3 | 799 KiB | 5.00 | 3.00 (2.00) | 5.46 (1.00) |
| 3 | 4.8 s | 48.6 | 60.8 | 799 KiB | 5.00 | 3.00 (2.00) | 6.60 (1.00) |

After teardown 1 slot goroutine remained above the baseline in each run (the
allowance is 10).

"Strategies" counts `connect.NewClientStrategy.func1` entry goroutines (one per
`ClientStrategy`); "transports" counts `connect.NewPlatformTransportWithTargetMode.func1`;
"pinned" counts `connect.(*PlatformTransport).runFamilyHoldWatcher`, which only
a family-pinned transport starts. These three are the host-independent
signal: the group builds its transports whatever the host's interfaces say.

Asserted upper bounds (in the test, with the measured values beside them):
strategies <= 5, transports <= 3, pinned <= 2, open platform connections of one
identity <= 3, slot goroutines <= 80 per identity, heap <= 2 MiB per identity,
and teardown back to within 10 goroutines of the baseline.

### Detecting the group-vs-plain difference

A one-off run (not committed) with the platform url set to the ip literal
`wss://127.0.0.1:<port>`, which makes the group build only its standby, is a
stand-in for task E's `DisablePlatformFamilyTransports`:

| shape | slot goroutines / identity | heap / identity | strategies | transports (pinned) |
|---|---|---|---|---|
| family group (base) | 48.6 to 49.5 | 799 to 820 KiB | 5.00 | 3.00 (2.00) |
| plain standby only | 39.2 | 754 KiB | 3.00 | 1.00 (0.00) |

So the harness separates the two shapes by 2 strategies, 2 transports and
about 10 goroutines per identity; heap moves only about 50-65 KiB per idle
identity, which is inside run-to-run noise at a 2 MiB bound, so the
structural counts, not heap, are the regression signal. When task E lands,
tighten `maxStrategiesPerIdentity` to 3, `maxTransportsPerIdentity` to 1,
`maxPinnedPerIdentity` to 0 and the goroutine bound accordingly.

### Findings along the way

- Host dependence. Whether the pinned transports DIAL depends on the host:
  `pinnedFamilySupported` (connect transport_family.go:179-193) asks
  `probeFamilySupport` (connect control_family.go:503-536), which wants a
  global unicast, non-reserved interface address of the family. On this
  machine the only address is in 192.0.2.0/24, which connect treats as
  reserved, so both pinned transports sleep and only the standby connects
  (all platform upgrades go to `connect.<domain>`). On a CI runner with a
  private or public v4 address the v4 pinned transport would dial. The
  structural counts are unaffected; the goroutine and open-connection numbers
  could differ there. UNVERIFIED on such a host. connect has no exported
  seam to pin the probe (`controlFamilyLedger.probe`, control_family.go:181-185,
  and `controlFamilyProbeSource.interfaces`, control_family.go:443-448, are
  unexported); the smallest seam would be an exported test setter in connect
  for that probe. Not built.
- What a pinned dial does through a SOCKS5 proxy (by name, or resolved
  locally first) is therefore not exercised here. UNVERIFIED.
- A proxied slot still builds an extender directory, its ping reporter and
  the space's extender status watch (one goroutine each per identity:
  `connect.NewExtenderDirectory.func1`, `connect.NewExtenderPingReporter.func1`,
  `sdk.(*NetworkSpace).startExtenderStatusWatch.func1`) even with
  `DisableExtenderNetwork`, because `NewNetworkSpaceWithUrls` builds the
  directory whenever the api url names a dotted host (sdk network_space.go:1317-1319).
- The fake api answers 404 to `/auth/refresh`; the token manager retries it
  (about 120-150 requests across 50 identities while settling). It does not
  prevent settling.
- The platform sees about 5 to 6.6 upgrades per identity during bring-up for
  one connection that stays open; the strategy races its dialers and drops
  the losers. Settled counts are stable.
