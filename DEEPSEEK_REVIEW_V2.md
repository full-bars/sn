I'll write a comprehensive review based on the code I can see. Since the code is truncated, I'll focus on the visible portions and note where I couldn't verify.

<｜DSML｜ calls>
<｜DSML｜ invoke name="Write">
<｜DSML｜ parameter name="content" string="true"># Deep Code Review — URNetwork H3/QUIC Provider (v2026 port)

Scope: `provider` package, ported from connect v3.23 to connect v2026 (H3/QUIC).
Reviewer focus: race conditions, security, code quality, operational readiness, test quality.

Note: the supplied source was truncated (~126 KB omitted from `provider/main.go` and
~9.5 KB omitted from the test file). Findings below are limited to what is visible;
areas that could not be verified are called out explicitly.

---

## CRITICAL

### C1. `provide()` calls `os.Exit(0)` and bypasses every deferred cleanup
**File:** `provider/main.go` — `provide()`

```go
defer st.cancel()
defer flushRetentionEvents()
...
closeAllCaches(st)
os.Exit(0)
```

`os.Exit` does not run deferred functions. Therefore:

- `st.cancel()` is never invoked on the normal shutdown path, so the root context
  is never cancelled and any goroutine still blocked on `st.ctx.Done()` is leaked
  until the process dies.
- `flushRetentionEvents()` is never invoked, so retention events buffered in memory
  are silently dropped on every clean shutdown.
- Any other deferred cleanup registered by callees (e.g. `unregSocketCloser` in
  `provideSetupSignals`, see C2) is also skipped.

**Why it matters:** Data loss (retention events), leaked goroutines, and a shutdown
path that does not actually exercise the cancellation logic it claims to. The
`closeAllCaches(st)` call is the only cleanup that runs, and it is not a substitute
for context cancellation.

**Fix:** Replace `os.Exit(0)` with a normal `return` from `provide()` (or
`runtime.Goexit()` from a goroutine) so defers run. If a hard exit is required for
systemd, do it in `main()` after `provide()` returns, and only after the deferred
cleanup has completed.

---

### C2. `defer unregSocketCloser()` unregisters the control-socket closer immediately
**File:** `provider/main.go` — `provideSetupSignals()`

```go
unregSocketCloser := RegisterCoordinatorCloser(func() {
    if st.cleanupControlSocket != nil {
        st.cleanupControlSocket()
        st.cleanupControlSocket = nil
    }
})
defer unregSocketCloser()
```

The `defer` is scoped to `provideSetupSignals`, which returns immediately after
registering the closer. The closer is therefore unregistered before the provider
ever reaches steady state, and the control socket is never cleaned up through the
coordinator path. The inline comment claims cleanup is handled by
`RegisterCoordinatorCloser` and `closeAllCaches`, but the `defer` defeats the
former.

**Why it matters:** The control socket (a Unix domain socket used by `urnet-tools`)
is left on disk after shutdown, and any coordinator-driven cleanup is a no-op. On
restart, `startControlSocket` may fail to bind because the stale socket file is
still present.

**Fix:** Remove the `defer unregSocketCloser()`. Register the closer for the
lifetime of the process (or until `closeAllCaches` runs), and unregister it only
from the shutdown path.

---

### C3. `st.wg` is never incremented — the 30 s shutdown wait is dead code
**File:** `provider/main.go` — `provide()` and `provideLaunchGoroutines()`

```go
done := make(chan struct{})
go func() {
    st.wg.Wait()
    close(done)
}()

select {
case <-done:
case <-time.After(30 * time.Second):
    tlog("[provider] timed out waiting for goroutines to exit\n")
}
```

No `st.wg.Add(...)` call appears anywhere in the visible code. Every goroutine
launched in `provideLaunchGoroutines` (`runHealthHeartbeat`, `runBandwidthReporter`,
`runHeartbeatReporter`, `runJWTRefresher`, `runEarningWindows`, `runLifetimeCollector`,
`runProfitHeartbeat`, `runBillableRateWriter`, `paceMonitor`) is started with
`go connect.HandleError(func() { ... })` and is not tracked by the wait group.

**Why it matters:** `st.wg.Wait()` returns immediately, so the 30 s timeout branch
is unreachable. The provider reports "normal shutdown" without ever waiting for
background reporters to flush. Combined with C1, this means the shutdown path is
purely cosmetic.

**Fix:** Either wrap each goroutine with `st.wg.Add(1)` / `defer st.wg.Done()`, or
remove the wait group and the timeout entirely and document that shutdown is
fire-and-forget. Do not leave a wait group that is never used.

---

### C4. `provideWithProxy` deletes the proxy from `proxyCancelMap` without cancelling it
**File:** `provider/main.go` — `provideWithProxy()`

```go
st.proxyCancelMu.Lock()
delete(st.proxyCancelMap, proxySettings.Address)
st.proxyCancelMu.Unlock()
return "", connect.Id{}, false, fmt.Errorf("proxy dropped after %s of continuous failure — %s", ...)
```

The cancel function stored in `st.proxyCancelMap` is removed from the map but never
invoked. The proxy's goroutine (and its `proxyCtx`) continues running until the
parent context is cancelled.

**Why it matters:** A "dropped" proxy is not actually dropped. Its transport,
dialers, and any in-flight auth attempts keep running, consuming resources and
potentially re-registering itself. The map deletion also means the cancel function
is now unreachable, so the goroutine cannot be cancelled by address later.

**Fix:** Capture the cancel function before deleting, then call it:

```go
st.proxyCancelMu.Lock()
cancel := st.proxyCancelMap[proxySettings.Address]
delete(st.proxyCancelMap, proxySettings.Address)
st.proxyCancelMu.Unlock()
if cancel != nil {
    cancel()
}
```

---

## HIGH

### H1. `RegisterProxy` does not reset per-instance state on re-registration
**File:** `provider/proxy_health.go` — `RegisterProxy()`

```go
h, ok := proxyHealthByIndex[index]
if !ok {
    h = &proxyHealth{}
    proxyHealthByIndex[index] = h
}
if h.address != "" && h.address != address {
    delete(proxyHealthByAddr, h.address)
}
h.address = address
h.connecting = true
h.connectingSince = time.Now()
proxyHealthByAddr[address] = h
```

The struct is reused across registrations, but only `address`, `connecting`, and
`connectingSince` are reset. The following fields retain values from the previous
instance:

- `everUp`, `downSince` — acknowledged in comments, but the workaround (checking
  `connectingActive` first) is fragile and only covers the "fresh connecting" case.
- `deadLogged` — a re-registered proxy that was previously logged as dead will
  never be logged as newly dead again, even if it fails to connect for another
  65 minutes.
- `failures` (`AuthFailures`, `TimeoutFailures`, `TransportDrops`) — counters
  accumulate across instances, so a proxy that is replaced after 100 auth failures
  starts its replacement at 100.
- `lastError`, `lastErrorAt` — stale error text is reported for the new instance.
- `lastSeenUp` — the transition baseline is inherited, so the first heartbeat after
  re-registration may emit a spurious `Recovered` or `NewlyDegraded` event.

**Why it matters:** Health reporting is wrong for respawned proxies. Operators see
inflated failure counts, missed dead-proxy alerts, and phantom recovery events.

**Fix:** Reset the per-instance fields explicitly in `RegisterProxy`:

```go
h.everUp = false
h.downSince = time.Time{}
h.deadLogged = false
h.lastSeenUp = false
h.lastError = ""
h.lastErrorAt = time.Time{}
h.failures = ProxyFailureCounters{}
```

If `bw` should survive re-registration (it is a pointer), keep it; otherwise reset
it too.

---

### H2. `RegisterProxyBandwidth` creates a health entry with an empty address that is never indexed by address
**File:** `provider/proxy_health.go` — `RegisterProxyBandwidth()`

```go
h, ok := proxyHealthByIndex[index]
if !ok {
    h = &proxyHealth{address: "", connecting: true}
    proxyHealthByIndex[index] = h
}
if h.bw == nil {
    h.bw = &bandwidth.ProxyBandwidth{}
}
return h.bw
```

When the entry is created here (rather than by `RegisterProxy`), it is added to
`proxyHealthByIndex` but **not** to `proxyHealthByAddr`. Consequences:

- `ProxyBandwidthByAddress("")` returns nil even though a health entry exists.
- `ProxyHealthByAddress()` will emit a `result[""]` entry (empty key), which
  pollutes the proxy.state snapshot.
- `IsDegraded("")` returns false even if the entry is degraded.
- `UnregisterProxy` will not delete the address entry because `h.address == ""`.

**Why it matters:** Inconsistent registry state. The `proxyHealthByAddr` map is
documented as "kept in sync with `proxyHealthByIndex`", but this path violates that
invariant.

**Fix:** Either do not create the entry in `RegisterProxyBandwidth` (return nil and
let `RegisterProxy` create it), or add the empty-address entry to
`proxyHealthByAddr` and make all consumers handle the empty key. The former is
cleaner.

---

### H3. `markProxyDown` leaves `downSince` zero for never-up proxies, causing immediate "inactive" classification
**File:** `provider/proxy_health.go` — `markProxyDown()`

```go
if h.currentlyUp {
    h.downSince = time.Now()
}
h.currentlyUp = false
```

If a proxy is marked down before it was ever marked up, `downSince` remains the
zero `time.Time`. `ProxyHealthByAddress` then computes:

```go
health = degradedTierFromDuration(time.Since(h.downSince))
```

`time.Since(zero)` is roughly the Unix epoch offset (~55 years), so
`degradedTierFromDuration` returns `"inactive"` immediately. The proxy is reported
as inactive (7+ days down) the moment it fails its first connection.

**Why it matters:** A brand-new proxy that fails its first dial is misreported as
long-dead, which can trigger cleanup (`runProxyURLCleanupOnce` removes `inactive`
proxies unconditionally) and evict proxies that are still warming up.

**Fix:** Stamp `downSince` on the first down transition regardless of prior state:

```go
if h.downSince.IsZero() || h.currentlyUp {
    h.downSince = time.Now()
}
h.currentlyUp = false
```

Or guard the `time.Since(h.downSince)` call with `!h.downSince.IsZero()`.

---

### H4. Inconsistent health classification across three functions
**File:** `provider/proxy_health.go` — `ProxyHealthSnapshot`, `ProxyHealthHeartbeat`,
`ProxyHealthByAddress`

The same proxy can be classified differently depending on which function is called:

| Function | 7+ days down, everUp |
|---|---|
| `ProxyHealthSnapshot` | `dead` |
| `ProxyHealthHeartbeat` | `Degraded` |
| `ProxyHealthByAddress` | `inactive` (via `degradedTierFromDuration`) |

`ProxyHealthSnapshot` uses `time.Since(h.downSince) >= 7*24*time.Hour` to move a
proxy to `dead`, while `ProxyHealthHeartbeat` never moves an `everUp` proxy to
`Dead` (it only populates `Dead` for `!everUp`). `ProxyHealthByAddress` returns
`"inactive"` for the same condition.

**Why it matters:** The `[health]` report, the heartbeat report, and the
`proxy.state` snapshot disagree about the same proxy. Operators cannot trust any
single view, and automated cleanup (`runProxyURLCleanupOnce` treats `inactive` as
removable) may act on a classification that the heartbeat report contradicts.

**Fix:** Extract a single `classify(h, now) string` function and use it from all
three call sites. Document the intended mapping (up / connecting / recently_offline
/ offline / long_offline / inactive / dead) in one place.

---

### H5. `slowRetrySemaphore` can be held across a blocking `Admit` call and leaked on panic
**File:** `provider/main.go` — `provideWithProxy()`

```go
if !isURLSourced && authFailures >= maxAuthFailures {
    select {
    case slowRetrySemaphore <- struct{}{}:
    case <-proxyCtx.Done():
        return "", connect.Id{}, false, proxyCtx.Err()
    }
}
release, waitErr := globalProxyAdmissionGate.Admit(proxyCtx, admitFailureCount)
if waitErr != nil {
    if !isURLSourced && authFailures >= maxAuthFailures {
        <-slowRetrySemaphore
    }
    return "", connect.Id{}, false, waitErr
}
...
byClientJwt, clientId, reused, err = provideAuth(...)
release()
if !isURLSourced && authFailures >= maxAuthFailures {
    <-slowRetrySemaphore
}
```

Problems:

1. The semaphore is acquired **before** `Admit`. If `Admit` blocks (it takes a
   context and may wait), the semaphore is held for the entire wait. With a small
   buffer, this can serialize all slow-retry proxies behind one blocked `Admit`.
2. If `provideAuth` panics, `release()` and the semaphore release are skipped. The
   semaphore slot is leaked permanently.
3. The release is duplicated in two branches; a future edit that adds a third
   return path will leak the slot.

**Why it matters:** A single panic or a slow `Admit` can permanently reduce the
slow-retry concurrency budget, eventually stalling all slow-retry proxies.

**Fix:** Use `defer` for both releases, and acquire the semaphore **after**
`Admit` returns (or wrap the whole block in a helper that uses `defer`):

```go
release, waitErr := globalProxyAdmissionGate.Admit(proxyCtx, admitFailureCount)
if waitErr != nil { ... }
defer release()

if !isURLSourced && authFailures >= maxAuthFailures {
    select {
    case slowRetrySemaphore <- struct{}{}:
        defer func() { <-slowRetrySemaphore }()
    case <-proxyCtx.Done():
        return "", connect.Id{}, false, proxyCtx.Err()
    }
}
```

---

### H6. `dailyTimer` with a non-positive `waitTime` fires immediately, causing a tight retry loop
**File:** `provider/main.go` — `provideWithProxy()`

```go
waitTime := globalProxySlowRetryState.TimeUntilNextAttempt(proxySettings.Address)
...
dailyTimer := time.NewTimer(waitTime)
select {
case <-proxyCtx.Done():
    dailyTimer.Stop()
    return "", connect.Id{}, false, proxyCtx.Err()
case <-dailyTimer.C:
    continue
}
```

If `TimeUntilNextAttempt` returns 0 or a negative duration (e.g. because the
recorded next-attempt time is in the past, or because the state was just cleared),
`time.NewTimer` fires immediately and the loop spins. There is no floor on
`waitTime`.

**Why it matters:** A tight auth-retry loop against the API, which is exactly the
behavior the slow-retry state machine is supposed to prevent. It can also trip the
API's rate limiter and cause a cascade of 429s.

**Fix:** Clamp `waitTime` to a minimum (e.g. `max(waitTime, time.Second)`) before
constructing the timer, and log when the clamp is applied.

---

### H7. `shmLogFatal`, `os.Exit(1)`, and `os.Exit(2)` bypass cleanup
**File:** `provider/main.go` — `provideSetupMemory()`, `provideWithProxy()`

```go
// provideSetupMemory
if err != nil {
    fmt.Printf("network config error: %s\n", err)
    os.Exit(1)
}
...
if err := runHotSwapChildHandshake(...); err != nil {
    tlog("[hotswap] Candidate pre-flight failed: %v\n", err)
    st.hotSwapIPC.Close()
    os.Exit(2)
}

// provideWithProxy
if errors.Is(err, ErrTokenInvalid) {
    shmLogFatal(78, "token invalid or expired — exiting so the startup script can refresh it")
}
```

All three paths terminate the process without running deferred cleanup. In
`provideSetupMemory`, `defer st.cancel()` and `defer flushRetentionEvents()` are
registered in `provide()` before `provideSetupMemory` is called, so they are
skipped. In `provideWithProxy`, the proxy's own cleanup (cancel, unregister) is
skipped.

**Why it matters:** Same class of bug as C1, but on error paths where cleanup
matters more (partial state, half-open sockets, unflushed events).

**Fix:** Return an error up the stack and let `provide()` handle it after running
defers. For `shmLogFatal`, consider a `panic` with a recover in `provide()` that
runs cleanup before exiting.

---

### H8. Silent encryption downgrade when cert/key/seed files are missing
**File:** `provider/main.go` — `provideWithProxy()`

```go
if seed, err := readProviderClientKeySeed(); err == nil && 0 < len(seed) {
    clientSettings.ClientKeySeed = seed
}
if certPem, keyPem, err := readProviderTlsCertAndKey(); err == nil && 0 < len(certPem) && 0 < len(keyPem) {
    ...
}
enableProviderEncryption(clientSettings)
```

If either read fails, the provider silently continues without the seed or the TLS
cert/key. `enableProviderEncryption` then sets `Mode = Opportunistic`, which means
the provider will still serve plaintext consumers but cannot answer post-quantum
handshakes. There is no log line indicating the downgrade.

**Why it matters:** A misconfigured or partially-provisioned host silently loses
PQE capability. Operators have no signal that encryption is degraded.

**Fix:** Log a warning (or a critical log) when the seed or cert/key read fails,
including the error. Consider failing fast if the operator has explicitly opted
into encryption.

---

## MEDIUM

### M1. `proxyIndexByAddr` is never cleaned up — unbounded growth
**File:** `provider/main.go` — `setProxyIndex` / `getProxyIndex`

`proxyIndexByAddr` is a package-level `sync.Map` populated in `provideLauncherLoop`
and read in `provideWithProxy`. Nothing ever deletes entries. Over a long-running
process that reloads proxy lists (URL fetcher, file reload), the map grows without
bound.

**Why it matters:** Memory leak proportional to the number of distinct proxy
addresses ever seen. On a fleet node that rotates through thousands of public
proxies, this is non-trivial.

**Fix:** Delete entries in `UnregisterProxy` (or wherever a proxy is dropped), or
use a bounded LRU.

---

### M2. `getProxyIndex` returns 0 for unknown addresses, colliding with the "direct" index
**File:** `provider/main.go` — `getProxyIndex`

```go
func getProxyIndex(addr string) int {
    if v, ok := proxyIndexByAddr.Load(addr); ok {
        return v.(int)
    }
    return 0
}
```

Index 0 is used for the direct/native proxy. Any unknown address silently maps to
0, so `RecordProxyAuthFailure(0, err)` and `RegisterProxyBandwidth(0)` will
attribute failures to the direct proxy.

**Why it matters:** Misattributed health metrics. A URL-sourced proxy that was
never registered will increment the direct proxy's failure counters.

**Fix:** Return a sentinel (e.g. `-1`) for unknown addresses and have callers skip
registration when the index is negative. Alternatively, allocate a fresh index on
miss.

---

### M3. `readURLCacheSize` returns 0 on lock failure, defeating the fetch gate
**File:** `provider/main.go` — `readURLCacheSize()`

```go
release, err := acquireProxyLock()
if err != nil {
    return 0
}
```

`shouldFetchNow` treats `cacheSize < 50` as the starvation floor and skips the
pressure stretch. If the lock is contended (e.g. by a concurrent cleanup or
reload), `readURLCacheSize` returns 0 and the fetcher will fetch at the base
interval regardless of pressure.

**Why it matters:** Under load, the fetch gate is supposed to stretch the interval
up to 8×. A contended lock silently disables that protection, causing fetch
amplification exactly when the box is already struggling.

**Fix:** Distinguish "lock failed" from "cache is empty". Return `(int, error)` and
have `shouldFetchNow` treat an error as "do not fetch" (conservative) rather than
"cache is empty" (aggressive).

---

### M4. `runProxyURLCleanup` advances `lastRun` even when the cleanup is skipped
**File:** `provider/main.go` — `runProxyURLCleanup()`

```go
if time.Since(lastRun) >= effective {
    if activeScope := resolveProxyCleanupScope(scope); activeScope == "url" || activeScope == "all" {
        runProxyURLCleanupOnce(activeScope)
    }
    lastRun = time.Now()
}
```

When `scope` is `"none"` (or any disabling value), the cleanup is skipped but
`lastRun` is still advanced. If the operator later flips the scope to `"url"` via
the control socket, the next cleanup will not run until the full effective interval
(up to 6 h) has elapsed.

**Why it matters:** Runtime toggles take up to 6 h to take effect, which
contradicts the function's own comment ("the loop still runs so that runtime
toggles (off→on) work live").

**Fix:** Only advance `lastRun` when the cleanup actually ran:

```go
if activeScope := resolveProxyCleanupScope(scope); activeScope == "url" || activeScope == "all" {
    runProxyURLCleanupOnce(activeScope)
    lastRun = time.Now()
}
```

---

### M5. `runProxyURLCleanupOnce` uses `state.StartedAt` without validating it
**File:** `provider/main.go` — `runProxyURLCleanupOnce()`

```go
uptime := time.Since(state.StartedAt)
const minUptime = 65 * time.Minute
```

If `state.StartedAt` is the zero `time.Time` (e.g. a freshly-created state file, or
a corrupted one), `time.Since` returns ~55 years and `uptime > minUptime` is true
immediately. The uptime guard that is supposed to protect warming proxies is
defeated.

**Why it matters:** On a fresh install with an empty `proxy.state`, the first
cleanup pass can mass-evict proxies that have not yet authed.

**Fix:** Validate `state.StartedAt` before using it:

```go
if state.StartedAt.IsZero() {
    state.StartedAt = time.Now()
}
```

Or treat a zero `StartedAt` as "not yet running long enough".

---

### M6. `runProxyURLCleanupOnce` silently ignores malformed `DegradedCleanupThreshold` and `DownSince`
**File:** `provider/main.go` — `runProxyURLCleanupOnce()`

```go
if d, err := time.ParseDuration(urlState.DegradedCleanupThreshold); err == nil && d > 0 {
    degradedThreshold = d
}
...
if ds, err := time.Parse(time.RFC3339, e.DownSince); err == nil && time.Since(ds) >= degradedThreshold {
```

Both parse errors are swallowed. An operator who typos the threshold gets no
feedback and no cleanup.

**Why it matters:** Silent misconfiguration. The operator believes degraded cleanup
is enabled but it is not.

**Fix:** Log a warning on parse failure, including the offending value.

---

### M7. `connectingStaleAfter` is coupled to `deadConfirmDelay` by comment only
**File:** `provider/proxy_health.go` — `connectingStaleAfter`

```go
// The 65-minute value matches the provider's deadConfirmDelay
// (provider/main.go), but the two are independent timers with different origins
// and scopes...
const connectingStaleAfter = 65 * time.Minute
```

The comment explicitly acknowledges the coupling and warns that "if one constant
changes, the other must too". This is a maintenance hazard: a future edit to
`deadConfirmDelay` will silently desynchronize the two timers.

**Why it matters:** The ~1 h staging window promised in the docs depends on both
constants agreeing. A drift causes either premature dead-confirmation or a
never-confirmed dead proxy.

**Fix:** Define a single exported constant (e.g. `ProxyStagingWindow = 65 * time.Minute`)
and reference it from both sites. If the two timers genuinely need different
values, document the invariant in a test that asserts the relationship.

---

### M8. `ProxyHealthSnapshot` and `ProxyHealthHeartbeat` key bandwidth by formatted entry, but `ProxyBandwidthByAddress` uses raw address
**File:** `provider/proxy_health.go`

```go
bwMap[formatProxyEntry(idx, h.address)] = pb
```

vs.

```go
func ProxyBandwidthByAddress(addr string) *bandwidth.ProxyBandwidth {
    ...
    if h, ok := proxyHealthByAddr[addr]; ok {
        return h.bw
    }
}
```

Two different key spaces for the same data. Callers must know which one to use, and
a lookup that uses the wrong key silently returns nil.

**Why it matters:** Easy to introduce a bug where bandwidth is reported as zero
because the caller used the raw address against a map keyed by `proxy[idx] (addr)`.

**Fix:** Pick one key space. Prefer raw address for both, or expose a
`ProxyBandwidthByIndex` and deprecate the address-keyed map.

---

### M9. `jwtNetworkId` and `parseJWTExpiryTime` parse JWTs without signature verification
**File:** `provider/main.go` — `jwtNetworkId`, `parseJWTExpiryTime`

Both use `ParseUnverified` (or equivalent). If either is used for any
authorization decision (e.g. deciding which network to join, or whether to skip
auth), an attacker who can write the JWT file can forge the claims.

**Why it matters:** Local file write is required, so the practical risk is low, but
the pattern is dangerous if the function is ever reused on a network-supplied JWT.

**Fix:** Document that these functions are for diagnostics only and must not be
used for authorization. If the network ID is used for routing, verify the signature
against the API's public key.

---

### M10. `applyStagedSession` moves files without validation or atomicity
**File:** `provider/main.go` — `applyStagedSession()`

The function moves `jwt`, `.client_jwts.json`, and `.provider.key` from
`.session-staging` to `~/.urnetwork/` when the `.session-pending` marker exists.
There is no validation of file contents, no check that the staging directory is
owned by the current user, and no atomicity across the three files.

**Why it matters:** A crash mid-move leaves a partially-applied session (e.g. new
JWT with old key), which can cause auth failures that are hard to diagnose. A
malicious local user who can write to `.session-staging` can inject a forged JWT.

**Fix:** Validate file ownership and permissions before moving. Move to a temp
name and `rename(2)` each file. Consider writing a single tarball and extracting
atomically, or at least record which files were moved so a retry can complete the
set.

---

### M11. `provide()` calls `provideStatusServer(st)` and `provideLauncherLoop(st)` without checking whether they return
**File:** `provider/main.go` — `provide()`

```go
provideLaunchGoroutines(st)
provideLauncherLoop(st)
provideStatusServer(st)
```

If either `provideLauncherLoop` or `provideStatusServer` blocks (