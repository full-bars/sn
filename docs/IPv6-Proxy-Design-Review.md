# Attack report: "make the sn miner as cheap per identity as 3.23"

## 0. What I read, and at which commit

| Tag used below | Repo | Commit | Notes |
|---|---|---|---|
| `sn` | full-bars/sn | `65910c22feca1d1de62aa05dcf4a35d64474d019` | main head (includes the READY=1 merge) |
| `connect` | full-bars/connect | `49d75e2644808df51a642fff92b321f12a1adec0` | the commit sn pins (sn go.mod:241). Main head is `f0e0b3fa26dd26d5a784bfcb01b3633c4f251c0f`. Every connect file cited below is byte-identical between the two (checked with `git diff`). |
| `sdk` | full-bars/sdk | `5dfc8a7c2a50c04c0ccc8dfa78eb19422936e8d4` | the commit sn pins (sn go.mod:226). It is **not on sdk main** (main head `96c2b6eb14962329d24ef2b738a7f252e86873b2`). The cited files match main except network_space.go. |
| `323` | full-bars/urnetwork-3.23-fix | `9f0db99771ea6d768b848cb3307f9d6844dc7aea` | main head |
| `xnet` | golang.org/x/net | module `v0.60.0` | the version sn's go.mod:27 resolves |
| `glog` | urnetwork/glog | `80a11b434ae90ae2221deb932daa21913919875a` | sn go.mod:228 |

I did not read urnetwork/server, urnetwork/connect or urnetwork/sdk. Every server-side claim below is UNVERIFIED. Nothing was compiled or run. The diffs in section 6 are sketches.

## 1. Verdict in five lines

1. **The design's main premise is wrong.** 3.23 does have FamilyPlatformTransportGroup. It deliberately gives proxied identities a single family-agnostic transport. That choice is why 3.23 runs one transport per proxy, and it is the reference fix the design marked as not applicable.
2. **P2's hold is lost almost immediately.** Every transport rebuild ("generation") builds a fresh, enabled v6 transport. A rebuild happens right after start-up for any identity whose provide mode includes public.
3. **A "held" transport is not cheap.** It still starts its run loop, its mode runners, its own connection strategy, and an H1 budget reservation it acquires before it checks the hold.
4. **P1 does nothing on LA7.** Its long tier needs a process-wide "platform answered recently" stamp, and LA7 never produces one.
5. **L2 as designed breaks hot-swap** (the successor process can be killed by SIGPIPE) and loses its queue on the ~61 `os.Exit` paths. In ramlog mode, the parked-on-glog convoy has a concrete cause that L2 does not touch.

## 2. Findings, ranked by severity

### C1 (Critical, premise): 3.23 already solves this by not building the group for proxied identities
- **Evidence:**
  - 323 `provider/family_transport.go:12-16`: "Only the direct identity ever gets the group ... a proxied identity stays on the single legacy transport".
  - `:200-202`: `useFamilyTransports` returns `false, "proxied-identity"` when `proxySettings != nil`.
  - `:237-240`: if not using the group, build one `connect.NewPlatformTransport`.
  - The 3.23 repo root has its own `transport_family.go`, so the design's check `git grep FamilyPlatformTransportGroup` would have printed hits.
  - In sn, the sdk always builds the group whenever the space derives family URLs: sdk `device_local_provider.go:347` and `:496-517`. `familyServiceUrl` is unconditional (sdk `network_space.go:1619-1625`).
  - Nothing in `sn/miner` gates this. A grep for `PlatformUrlV6|useFamilyTransports` in miner finds nothing.
- **Failing scenario:** the design ships P1, P2 Phase A and S1. Each proxied identity still runs three transports, two direct strategies and the group goroutines. The 3.23 baseline it is measured against runs one.
- **Fix:** port 3.23's gate into the sdk. When the provider's dial is injected (proxied), build the agnostic transport alone and enabled. Then use P2 to add a v6 pin only on positive evidence (diff 6.1).
- **IPv6 constraint:** the agnostic transport sends a hostname to the proxy. xnet `internal/socks/client.go:96-103` uses the domain-name address type for a hostname. So the proxy picks the family it can use. UNVERIFIED: whether connect resolves the name locally first on the TCP path, and whether the platform credits a pinned-v6 connection differently from an agnostic one. Both are open questions for the server source.

### C2 (Critical): the P2 hold does not survive a transport rebuild, and the v6 transport dials once on construction
- **Evidence:**
  - connect `transport_family.go:1233-1238`: `pinnedSettings` forces `copied.StartDisabled = false`, so a caller cannot construct a held pin.
  - Pinned transports start their run loop inside the constructor (connect `transport.go:1829-1833`).
  - With zero failures, `delay()` returns 0 (connect `transport_family.go:372-378`), so the first dial goes out at once, raced up to ParallelBlockSize=4 ways (connect `net_http.go:47`).
  - Rebuilds come from `migratePlatformTransportWithPolicy`, which calls `newProviderPlatformTransport` (sdk `device_local_provider.go:902`). That reads `self.platformUrlV6` without the lock (`:512`).
  - The provider "starts at provide mode none ... a mode that includes public rebuilds the transports" (sdk `device_local_provider.go:290-292`; `setProvideMode` at `:427-460`).
- **Failing scenario:** a slot starts. The miner calls `SetPinnedIpv6Enabled(false)` on generation 1. The device then hands over provide mode public, which flips the public flag and rebuilds. Generation 2's v6 pin is enabled and has a fresh backoff, and the 43k-dial storm returns within seconds. A platform drain migration does the same later.
- **Fix:**
  - Store the hold as provider state.
  - Pass it into every group as a per-family `StartDisabled`.
  - Re-apply it under `stateLock` at install time, which closes the race with a migration already in flight (diff 6.1).
  - Do not "blank platformUrlV6": that mutates a field read without the lock and makes a later live re-enable impossible without a rebuild.

### H1 (High): a held (Phase A) transport still creates goroutines, a strategy and a budget claim
- **Evidence:**
  - `run()` acquires `h1BudgetReservation` before anything checks the hold (connect `transport.go:2160-2164`). It then starts mode runners, the H3 group and `runModeElection` (`:2166-2190`). The reservation is registered as required (`:1792-1796`) and is released only when `run()` exits (`:2148-2158`).
  - Only the H3 runner is hold-aware ("a held transport neither dials nor holds an optional lease", `:2030-2035`).
  - `runFamilyHoldWatcher` starts for every pin (`:1825-1828`).
  - The group's `run` spawns one notify goroutine per transport on each pass (connect `transport_family.go:1349-1360`), and `Transports()` includes the held v6 (`:1414-1424`).
  - Its `NewDirectClientStrategy` is built regardless of hold (`:242-251`).
- **Failing scenario:** the design's "holding v6 removes about a third [of goroutines]" is false for Phase A; the saving is zero. Worse: if a device's transport budget admits only as many H1 carriers as it has active transports, the held v6 sits on an H1 slot while v4 or the standby parks in `Acquire`. Your dump shows `platformTransportBudgetReservation.Acquire` 3.0 parked per identity, which fits this. The sdk's budget sizing is UNVERIFIED, so this is PLAUSIBLE as a contributor to "zero [t]connect".
- **Fix:** wait for admission before the H1 acquire (diff 6.2, which needs the budget owner's review), or make a held pin not exist at all (C1).

### H2 (High): the SOCKS5 probe misclassifies several error classes
How xnet reports SOCKS outcomes:
- A reply code arrives as an untyped string `"unknown error " + Reply.String()` (xnet `internal/socks/client.go:115-116`; names at `socks.go:40-62`).
- Every failure is wrapped as `net.OpError{Op:"connect", Net: network, ...}` (`socks.go:170-176`). That is where "socks connect tcp6 ..." comes from.
- The stage of an EOF is lost: it can come from the greeting read (`client.go:68`), auth (`:79`, the `Authenticate` callback), the reply header (`:109`) or the bound address (`:131`, `:143`).
- `io.ReadFull` returns `EOF` on zero bytes and `unexpected EOF` on a partial read. Your measured string is the partial one, so "immediate EOF" is the wrong label.
- The `network` argument (`"tcp6"`) is only validated (`socks.go:152`). It never reaches the proxy, so "tcp6" in the probe proves nothing by itself. Only the target name having no A record makes it a v6 test (DNS records for connect-v6 are UNVERIFIED).

RFC 1928 section 6 defines X'00'..X'08' and says the server must close the connection shortly after a failure reply.

| Outcome | Design's class | Correct class | Why |
|---|---|---|---|
| X'03' network unreachable | missing | **no**, if a v4 control probe passes in the same round | this is the canonical "no v6 route" (ENETUNREACH) |
| X'04' host unreachable | no | **no**, with v4 control | an AAAA-only name on a v4-only resolver also gives this |
| X'08' address type not supported | no | **no** | only fires for an IPv6 literal (address type 4). A hostname probe never triggers it, so the probe must send exactly what the transport sends. |
| X'02' not allowed by ruleset | no | **no only if v4 control is allowed**, else unknown | an account ACL or rate limit refuses v4 too |
| X'01' general failure | missing | unknown | Dante and others use it for everything |
| X'05' connection refused | missing | **unknown, leaning yes** | the proxy reached a v6 host |
| X'06' TTL expired, X'07' command not supported | missing | unknown | proxy-side |
| EOF during greeting or auth | "immediate EOF = no" | **unknown** | an overloaded or connection-capped proxy, or bad credentials. Some proxies close instead of sending status 1. |
| EOF while reading the CONNECT reply, v4 control OK | no | no | |
| X'00' then TLS fails or EOF | yes | **unknown** | lazy-connect proxies answer success before dialing upstream (PLAUSIBLE). The probe must finish the TLS handshake to call it "yes". |
| timeout | unknown, "retry soon" | unknown **with backoff** | a proxy that black-holes v6 never replies, so an unbounded "retry soon" re-probes forever. The existing UDP check uses a 5 min unknown TTL (sn `miner/proxy_udp_check.go:111-112`). |

Two footguns:
- `proxy.SOCKS5(network, ...)` takes the **proxy's** network as its first argument (connect `net.go:341-346`). A probe that passes "tcp6" there dials an IPv4 proxy over tcp6, fails locally, and lands in "unknown" forever.
- The v6 strategy keeps the injected `PacketConnFactory` (connect `transport_family.go:247-252`), so v6 H3 runs over the SOCKS UDP relay. The miner's UDP check verifies the relay's v4 exit only (sn `miner/h3_packet_conn.go:76-85`). A TCP "yes" says nothing about v6 H3.

Fix: a staged hand-rolled probe (the pattern already exists at sn `miner/socks5_udp.go:105-189`), plus a v4 control probe and a TLS step (diff 6.3).

### H3 (High): L2's os.Stderr swap breaks hot-swap and loses lines on exit
- **Evidence:**
  - The candidate is spawned with `cmd.Stderr = os.Stderr` (sn `miner/hotswap_unix.go:96-97`, again at `:107-108`). The parent exits via `exitFunc` (os.Exit) after the drain (sn `miner/hotswap.go:121`, `:447-470`).
  - The miner has about 61 `os.Exit`/Fatal sites (for example sn `miner/run.go:510-932`), so there is no single "path that flushes today".
  - `tlog` writes to `proxyManagerLogWriter`, captured as `os.Stderr` at package init (sn `miner/proxy_manager_log.go:29-34`). Swapping the variable therefore does not move `tlog`, which carries most `importantLogMarkers` lines ([proxy][url], [govern], ...).
- **Failing scenarios:**
  1. Non-ramlog mode. The successor inherits the parent's L2 pipe as fd 2. When the parent exits, the next write to fd 2 hits a broken pipe, and the Go runtime kills a process that writes to a broken pipe on fd 1 or 2 with SIGPIPE. The successor dies about one drain window after takeover, and nothing is serving. The ramlog code already guards this exact hazard for exec (sn `miner/shmlog_linux.go:60-70`).
  2. Any `os.Exit` loses up to 250 ms of queued lines, including important ones.
  3. glog lines are delayed by the queue while `tlog` lines are not, so order inverts in the log.
- **Fix:** give children the saved original fd. Flush the queue from a wrapped `exitFunc` and from `critLogPanic`. Redirect with dup2 on fd 2, not by reassigning the variable.

### H4 (High): P1 is inert on the box it targets
- **Evidence:**
  - `platformReachedRecently` reads `lastPlatformSuccessNano` (connect `transport.go:391-402`). That is written only by `noteBackendSuccess` (`:517-518`).
  - Callers of `noteBackendSuccess`: agnostic dial success (connect `transport_family.go:710`), contract out-of-band (OOB) success (connect `transfer_contract_manager.go:1704`) and `noteBackendNetworkChanged` (connect `transport.go:533-535`).
  - The "same rule as the agnostic path" is off in sn: `ReconnectLongAfter` defaults to zero (connect `transport.go:650-659`), and nothing in sdk or miner sets it (grep).
- **Failing scenario:** LA7 completes no [t]connect, so there are no agnostic successes and no contracts. `platformReachedRecently()` stays false, `failIsolated(false)` never counts, and the v6 pin stays on the 5 min cap forever.
- Also wrong: "8-15 min after about 7 attempts". `delay()` doubles on *total* failures from base 5 s (connect `transport.go:831`, `transport_family.go:381-385`). Reaching [7.5, 15) min takes 9 failures.
- Also, defaulting P1 on in connect changes behaviour for direct users: v6 can take up to 15 min to come back after a transient outage with no network change. Default it to 0 in connect and enable it from the sdk for injected-dial identities.

### H5 (Medium-High): in ramlog mode the convoy has a concrete cause that L2 does not touch
- **Evidence:**
  - `initSHMLogger` already dup2s fd 1 and fd 2 onto a pipe drained by one goroutine (sn `miner/shmlog_linux.go:139-151`, `:175-221`). That reader takes `fMu` for every write (`:186-188`).
  - The trimmer holds `fMu` while it allocates, reads and rewrites up to about 3.3 MiB every 5 s (`:252-262`; sn `miner/shmlog_trim.go:36-80`; 5 MiB cap and ratio 3 at `shmlog_linux.go:19-21`).
  - The syncer holds `fMu` for `f.Sync` every 500 ms (`:264-272`).
  - While `fMu` is held, the 64 KiB pipe fills and every glog `Emit` blocks inside `stderrSink.mu` (glog `glog_file.go:246-254`).
  - The important-line mirror lives in the same reader (`shmlog_linux.go:192-218`), so "the /dev/shm important buffer stays independent" is false in this mode.
- **Fix (smaller than L2):** keep the pipe reader off `fMu`. Trim by writing a new file and renaming it, outside the reader's path. Check with L0 before building anything bigger.

### M1 (Medium): fixing P2 in sdk main would silently undo the extender opt-out
- **Evidence:** sn pins sdk `5dfc8a7c` ("per-space opt-out from the extender network client"), which exists only on `origin/feat/extender-optout`. `git grep DisableExtenderNetwork main` in sdk finds nothing. sn relies on it at `miner/run.go:2160`.
- **Failing scenario:** the P2 sdk change is branched from sdk main and the pin is bumped. All 500 identities regain an extender client and a gossip node each.

### M2 (Medium): persisting verdicts in proxy.state loses updates around hot-swap
- **Evidence:**
  - Writes replace the whole file atomically by rename (sn `miner/proxy_manager_state.go:121-150`), guarded only by the in-process `proxyStateMu` (`proxy_manager_grade_paid.go:133`), from at least five call sites.
  - The candidate starts slots before it sends the ACK, while the parent drains (sn `miner/hotswap.go:1062-1072`, `:399-470`).
- **Failing scenarios:**
  - Parent and successor read-modify-write proxy.state concurrently, and the last writer wins.
  - 500 verdict flips mean 500 full JSON rewrites on one core.
  - Both processes dial the same proxies during the drain, so per-account connection caps produce greeting-stage EOFs, which feed H2's misclassification.
- **Fix:** a separate small verdict file, written in batches, only by the current owner, and merged on takeover the way audit.json is (`hotswap.go:1080-1092`).

### M3 (Medium): Phase B lazy construct/close races the group's lock-free readers
- **Evidence:** `Transports()` reads fields without the lock (connect `transport_family.go:1414-1424`). `SetAuth` iterates it (`:1468-1472`). `CanMakeBeforeBreakFrom` pairs fields (`:1494-1505`). `run` builds its notify set per pass (`:1301-1305`).
- **Failing scenarios:** a v6 transport created after a JWT refresh starts with stale auth. `run` never wakes on the new transport's connect.
- **Fix:** add the v6 pin through the existing make-before-break generation path instead of mutating a live group.

### M4 (Medium): P2(c)'s in-service flip has no signal to read
- **Evidence:** `Status()` exposes only coarse state (connect `transport_family.go:1447-1462`). "connecting" covers both "failing" and "waiting out backoff".
- **Failing scenario:** "3 consecutive isolated v6 failures" needs a new connect hook. Without one, a proxy whose TCP probe says yes while v6 H3 over the relay fails (see H2) oscillates every 15 min.
- **Fix:** add a per-transport failure-callback hook in connect, and grow the "no" TTL each time the probe says yes and the transport then fails.

### M5 (Medium): S1 claims are inaccurate, and S1(ii) breaks the J4 rule
- **S1(i):**
  - The space's strategy adds more than "the extender directory and alt url": also `VlessConfigs`, `DohSettings` and `InternalDohDomains` (sdk `network_space.go:1290-1331`).
  - Smart-dialer probes (sn `miner/smart_dialer_scheduler.go:820-836`) feed ranking (connect `net_http_smart_dialer.go:90-107`). Moving them onto the space's strategy reorders the dialers the API, the device and the standby use.
  - Acceptable, but it is a behaviour change, not a free saving.
- **S1(ii):**
  - The direct standby exists so the platform sees the provider's own address (sdk `device_local_provider.go:377-397`).
  - `NewDirectClientStrategy(...,0)` keeps the injected proxy dial, because it replaces the dial only for `ipFamily != 0` (connect `transport_family.go:241`). It still strips VLESS and extenders (`:229-237`).
  - Reusing the space's strategy lets the standby connect via VLESS or extenders: a public provider tagged with someone else's address, which is exactly what J4 forbids.

### M6 (Medium): S2 as written either does nothing or causes disruption
- **Evidence:**
  - Workers and queues are sized once, at construction (connect `ip.go:8462-8485`), so the count cannot change live.
  - "Start on first client" means rotating the provider. Rotation drops flows (sdk `device_local.go:6144-6168`).
  - `ReturnSendWorkerCount` also sets the source-retirement worker count (connect `ip.go:7950-7953`). Whether retirement backlog can trigger a rotation is UNVERIFIED.
  - The memory-budget path already clamps workers to 2 and the queue to 16 (connect `ip.go:7847-7852`) whenever the LocalUserNat has a `MemoryBudget` (sdk `device_local.go:6032-6034`, `device_local_provider.go:273-275`).
- The dump's 8.0 per identity therefore implies the miner's devices run with `transferMemory == nil` (UNVERIFIED). Wiring the budget is the cheaper fix.

### L1 (Low): a held v6 counts as "can connect" for the standby
- **Evidence:** `pinnedCannotConnectWithLock` returns false for `Disabled` (connect `transport_family.go:1389-1402`), even though its comment says "or held".
- **Failing scenario:** with v6 held and v4 sleeping or unresolvable, the standby waits the full 15 s (`:1137`) instead of releasing at once. That is extra delay in recovery.

### L2 (Low): `[start ]`/`[end ]` lines are not unconditional
- Both [t]connect trace sites are gated by `log.V(2)` (connect `transport.go:2555-2559`, `:3612-3616`; the same gate in 323 `transport.go:2344-2348`).
- At default verbosity those ~50k lines disappear, which strengthens the L0 gate.

### L3 (Low): the important lane can be flooded by warnings
- L2 routes every W/E line to the important lane. connect `transfer.go` alone has 23 Warningf/Errorf sites (grep count).
- A warning storm can push [profit] lines out through drop-oldest.
- **Fix:** select the lane by marker, and throttle W/E per template.

### Line-number drift in the design
The design's connect `transport.go` cites (1564, 783, 1572, 2348-2356) match neither the pin nor connect main. The actual locations are 1762, 1757-1760, 1768 and 2607-2616. Its `network_space.go:1318` matches sdk main, not the pin (1331).

## 3. Everything that resets the pinned backoff or brings v6 back

| Path | Location (connect unless noted) | Undoes |
|---|---|---|
| `pinnedBackoff.reset` in `noteDialSuccess` | `transport_family.go:703-706` | P1 (a flapping v6 that connects briefly never reaches the tier) |
| `pinnedBackoff.reset` in `noteKick` | `transport_family.go:748-751` | P1 |
| `noteKick` callers, H1 loop | `transport.go:2544` (kick while waiting), `:2620` (kick during dial), `:2634` (kick after failure) | P1 |
| `noteKick` callers, H3 loop | `transport.go:3583`, `:3679`, `:3691` | P1 |
| Kick sources | `Kick()` (`transport.go:1264-1270`) registered per transport on the process-wide `NetworkChanged` (`:1821`); group `Kick` (`transport_family.go:1475-1479`) | One `NetworkChanged` resets all ~1,500 ladders at once, then they redial together. No sn/miner caller today; callers exist only at sdk `device_local.go:4597-4599` and `sdk.go:603`. Any future host-network hook triggers this. |
| `updateHold` notify | `transport_family.go:497-501` | Does **not** reset: it does not bump `kickGeneration`, so `kicked()` stays false |
| Group rebuild | sdk `device_local_provider.go:902` (migration), `:427-460` (provide-mode flip) | P1 and P2 both (C2) |
| Slot or device restart, process restart, hot-swap | in-memory state | P1 (P2 survives only if persisted, see M2) |
| `noteBackendNetworkChanged` stamps "success" | `transport.go:533-535` | Makes every failure count as isolated for 2 min after a network change |

## 4. Tests in the plan that would pass without the fix

1. **P1 delay test with `setLongTier`.** It passes on today's code, because the struct already supports both (connect `transport_family.go:372-445`; existing tests at connect `transport_dial_backoff_test.go:42-79`). It never touches the constructor wiring or `noteDialFailure`. Replace it with a test that builds a pinned transport from settings, stamps `noteBackendSuccess`, calls `noteDialFailure` N times, and asserts the tier.
2. **"A held transport makes no dials".** If the test calls `SetEnabled(false)` and then counts, it passes today. It misses the constructor dial (C2) and the rebuild. Test through `deviceLocalProvider` instead: hold, then `setProvideMode(public)` and `migratePlatformTransport`, and assert zero v6 dials, counted from construction.
3. **`HasIpv6 == false` with the setting off, implemented by blanking the URL.** It passes while the live setter path stays broken.
4. **A classification table over error strings.** It passes even if real proxies never produce those strings (you measured "unexpected EOF"). Use a fake SOCKS5 server on `net.Pipe` that answers each reply code and closes at each stage, and check the v4 control.
5. **L2 test with a blocking writer.** It misses `os.Exit`, the hot-swap child inheriting fd 2, and the `tlog` bypass.
6. **Ledger state machine with an injected clock.** It misses the two-process write race (M2).

## 5. UNVERIFIED
- Server crediting of agnostic vs pinned connections.
- Whether the connect TCP path sends an FQDN or a literal to the proxy (your logged `socks connect` line shows the target and settles it).
- DNS records for connect-v6.
- The sdk's transport budget sizing (H1).
- Whether `transferMemory` is nil for miner devices (M6).
- Whether retirement backlog can trigger a rotation.
- Your GUESS that the v6 retries block v4. H1 is an alternative explanation I could not confirm.

## 6. Diffs (inline only, not compiled, not pushed)

### 6.1 Hold v6 through every generation (connect plus sdk)
```diff
--- connect/transport_family.go
 type FamilyPlatformTransportGroupSettings struct {
 	StandbyDelay time.Duration
+	// Ipv6StartDisabled constructs the v6 pin held (StartDisabled): it never
+	// dials until the owner calls SetEnabled(true). An owner without positive
+	// evidence that v6 works (a proxied identity) sets it on every generation.
+	Ipv6StartDisabled bool
 }
@@ pinnedSettings := func(ipFamily int, platformUrlFamily string) *PlatformTransportSettings {
 		copied := *settings
 		copied.IpFamily = ipFamily
-		copied.StartDisabled = false
+		copied.StartDisabled = ipFamily == 6 && groupSettings.Ipv6StartDisabled
@@ func (self *FamilyPlatformTransportGroup) pinnedCannotConnectWithLock() bool {
-		case PlatformTransportStateSleeping, PlatformTransportStateIdlePolicy:
+		case PlatformTransportStateSleeping, PlatformTransportStateIdlePolicy, PlatformTransportStateDisabled:
 			return true
```
```diff
--- sdk/device_local_provider.go
 type deviceLocalProvider struct {
 	...
+	// pinnedIpv6Held is the owner's v6 hold. Every generation is built with it
+	// and re-applies it at install, so a rebuild cannot re-enable v6.
+	pinnedIpv6Held atomic.Bool
 }
@@ func (self *deviceLocalProvider) newProviderPlatformTransport(
+	groupSettings := connect.DefaultFamilyPlatformTransportGroupSettings()
+	groupSettings.Ipv6StartDisabled = self.pinnedIpv6Held.Load()
 	return connect.NewFamilyPlatformTransportGroup(
 		...
-		settings,
-		nil,
+		settings,
+		groupSettings,
 	)
@@ installNext := func() (migratablePlatformTransport, bool) {
 		previous := self.platformTransport
 		self.platformTransport = next
+		applyPinnedIpv6Hold(next, self.pinnedIpv6Held.Load())
 		self.platformTransportChangedWithLock()
+
+// SetPinnedIpv6Enabled stores the hold before touching the transport, so a
+// generation built after this call sees it, and one built before it is
+// corrected when installed (installNext).
+func (self *deviceLocalProvider) SetPinnedIpv6Enabled(enabled bool) {
+	self.pinnedIpv6Held.Store(!enabled)
+	self.stateLock.Lock()
+	defer self.stateLock.Unlock()
+	applyPinnedIpv6Hold(self.platformTransport, !enabled)
+}
+
+func applyPinnedIpv6Hold(transport migratablePlatformTransport, held bool) {
+	if group, ok := transport.(*connect.FamilyPlatformTransportGroup); ok {
+		if v6 := group.Ipv6Transport(); v6 != nil {
+			v6.SetEnabled(!held)
+		}
+	}
+}
```
Per C1, also gate the v4 pin. For an injected-dial provider, pass an empty `platformUrlV4` so the agnostic transport runs alone and enabled, mirroring 323 `provider/family_transport.go:200-202`. Whether to keep a v4 pin is the open server question.

### 6.2 A held transport takes no H1 admission (needs the budget owner's review)
```diff
--- connect/transport.go  func (self *PlatformTransport) run()
 	if self.h1BudgetReservation != nil {
+		// a held transport holds no admission: wait out the owner's hold first,
+		// so a parked pin cannot sit on the H1 slot its siblings need
+		if !self.waitDialAdmission(self.ctx) {
+			return
+		}
 		if !self.h1BudgetReservation.Acquire(self.ctx) {
 			return
 		}
 	}
```
Known gap: a transport held after it has acquired still keeps the slot.

### 6.3 Staged probe classification (miner)
```go
// ipv6ProbeStage is how far one SOCKS5 CONNECT got before it ended.
type ipv6ProbeStage int

const (
	ipv6ProbeStageProxyDial ipv6ProbeStage = iota
	ipv6ProbeStageGreeting
	ipv6ProbeStageAuth
	ipv6ProbeStageConnectReply
	ipv6ProbeStageTls
	ipv6ProbeStageDone
)

// ipv6ProbeAttempt is one staged CONNECT (handshake as in socks5_udp.go).
// rep is the RFC 1928 reply code, or -1 when no reply header was read.
type ipv6ProbeAttempt struct {
	stage   ipv6ProbeStage
	rep     int
	timeout bool
}

// classifyIpv6Probe reads a v6 attempt against a v4 control attempt made
// through the same proxy in the same round. Only a failure at or after the
// CONNECT reply, with the v4 control fully done, says anything about v6.
func classifyIpv6Probe(v6 ipv6ProbeAttempt, v4 ipv6ProbeAttempt) proxyIpv6Verdict {
	if v6.stage == ipv6ProbeStageDone {
		return proxyIpv6Yes
	}
	if v6.timeout || v6.stage < ipv6ProbeStageConnectReply || v4.stage != ipv6ProbeStageDone {
		return proxyIpv6Unknown
	}
	switch v6.rep {
	case -1:
		// the proxy hung up instead of replying, while v4 went through
		return proxyIpv6No
	case 0x02, 0x03, 0x04, 0x08:
		return proxyIpv6No
	default:
		// 0x00 then TLS failed, 0x01, 0x05 (a v6 host answered), 0x06, 0x07
		return proxyIpv6Unknown
	}
}
```
Two requirements for the probe: send the target exactly as the transport does (go through `pinnedDialContext(6, injected)`, or the same FQDN), and keep the proxy dial on `proxySettings.Network`. Give unknown a growing TTL starting at 5 min.
