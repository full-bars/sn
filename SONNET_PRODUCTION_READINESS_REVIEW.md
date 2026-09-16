# Production Readiness Review — h3-provider (connect v2026 / H3+QUIC migration)

Reviewer: Sonnet 5, 2026-09-16. Scope: `provider/`, `cmd/provider/`, compared against
`/home/klets/ur/urnetwork-3.23-fix/provider/` (production fork) and the pinned
`github.com/full-bars/connect@v0.0.0-20260916141202-065bcdd9b85d` module source.

This is a second-pass review — prior sessions already replaced several metrics/health
stubs (see `STUB_REPLACEMENTS.md`) and ported `main.go` into `provide.go` + `cmd_main.go`
+ `cmd/provider/main.go`. This pass focuses on whether the wiring is actually load-bearing
in the request path, not just whether it compiles and the CLI dispatches.

---

## CRITICAL-1: Bandwidth/billing counters are never incremented — earnings, grading, and the paid-savings feature are permanently dead

**Files:** `provider/provide.go:658-661`, `provider/bandwidth/tracker.go`, `provider/bandwidth/wrap.go`

`provideWithProxy` creates a bandwidth counter and then throws it away:

```go
// provide.go:658-661
// Register bandwidth (tracks internally, bw not passed to connect in v2026).
_ = RegisterProxyBandwidth(proxyIndex)

// Note: NewLocalUserNat in v2026 no longer takes a bw parameter.
localUserNat := connect.NewLocalUserNat(proxyCtx, clientId.String(), localUserNatSettings)
```

The comment is accurate about *why* (v2026's `NewLocalUserNat`/`NewRemoteUserNatProvider`
dropped the `bw` parameter the fork used to pass in), but the replacement mechanism the
migration built to compensate — `bandwidth.WrapDialContextSettings` in
`provider/bandwidth/wrap.go` — is **never called from any production code path**. I
grepped the whole `provider/` tree:

```
$ rg -n "WrapDialContextSettings\(" --type go
bandwidth/wrap.go:21:   (doc comment example)
bandwidth/wrap.go:24:   func WrapDialContextSettings(...)   <- definition
bandwidth/wrap_test.go:13, 36                                <- only test callers
```

It is dead code outside its own test file. Nothing in `provide.go` sets
`clientStrategySettings.DialContextSettings` (or any settings struct) to the wrapped
version.

Separately, even if it *were* wired up, `DialContextSettings` in the pinned connect
module is only consulted for DoH/websocket signaling dials (`net_http.go`,
`net_http_doh.go`, `tun.go:694`), not for the actual relayed client IP traffic that flows
through `LocalUserNat`/`RemoteUserNatProvider`/the H3 packet-conn path — the traffic that
was billable in the fork. So this wrapper, even fixed, would count the wrong bytes.

**Consequence — confirmed by reading every consumer:**

`ProxyBandwidth.BillableRx`/`BillableTx` (`provider/bandwidth/tracker.go:27`) has **zero
writers anywhere in the codebase** — I grepped for `.Add(` / `.Store(` on those fields and
found none outside the struct's own `Snapshot()` copy method. `TotalRx`/`TotalTx` do have
a writer (`bandwidth.Conn.Read/Write`, `bandwidth.PacketConn.ReadFrom/WriteTo`), but
`bandwidth.NewConn`/`NewPacketConn` are likewise never instantiated outside tests — so
those are dead too.

This permanently-zero counter is read as ground truth by:
- `earn_tracker.go:76` — the earn-skip/paid-savings liveness signal. Per the file's own
  doc comment: "the paid-savings feature would be dead in production" if this mismatches
  — it does, unconditionally, because the numerator never moves.
- `proxy_earnings_store.go:122` — persisted earnings history used to rank/promote proxies
  (`prioritizeAndScheduleProxies` in `provide.go:848` reads this history at startup).
- `proxy_health_log.go:54,206-215` — health log sorts/displays proxies by billable bytes.
- `health_heartbeat.go:137,175` — the midnight-checkpoint / daily billable-bytes report
  sent upstream.
- `bandwidth_reporter.go:358-359` — `BillRX`/`BillTX` fields sent in the bandwidth report
  to the hub/dashboard.
- `metrics_collectors.go:138,269,379,490` — Prometheus `/metrics` billable-byte gauges.

**Net effect:** the provider will start, authenticate, and (as far as the actual
blockchain/contract-based settlement in `connect`'s `transfer_contract_manager.go` is
concerned — which is a separate accounting path this review did not find broken) may
still get paid. But every *operator-facing* signal that depends on this local counter —
dashboards, `/metrics`, earnings history, proxy grading/promotion, the paid-savings
earn-skip feature — will silently report zero forever. There is no error, no log line, no
crash: it looks like the box is running fine and simply never earns. This is exactly the
"no signal" failure mode this project's own bug-report template calls out as worse than
an absent feature.

**Suggested fix:** either (a) find the actual v2026 seam where per-client relayed bytes
are observable (likely inside `LocalUserNat`/`RemoteUserNatProvider`'s packet path, or via
a new `H3PacketConnFactory`/read-write interceptor at the QUIC transport boundary
referenced in `transport.go:285`) and wire `ProxyBandwidth.TotalRx/Tx` (and whatever
subset counts as "billable" per the fork's original definition) from there, or (b) if no
such seam exists in this connect version, file it as a tracked gap rather than shipping
code that pretends to count bytes it never sees. Do not ship with `WrapDialContextSettings`
built but uncalled — it creates false confidence that this was handled.

---

## HIGH-1: `pqeTotalCounts()` always returns the zero struct — PQE/classical session metrics are fabricated as "0 active" rather than "unknown"

**File:** `provider/metrics_stubs.go:84-101`

```go
func pqeTotalCounts() PQETotalCounts {
	return PQETotalCounts{}
}
```

The comment is honest that v2026's `EncryptionSessionManager` "carries no PQE/classical
session tracker at all." That's a real upstream removal, not a porting bug. But the
function still returns a struct of all-zero *counts*, which downstream metrics code will
render as "0 active PQE sessions" — indistinguishable from "PQE is disabled" or "working
correctly with nothing active." An operator watching `/metrics` for PQE rollout health has
no way to tell "definitely zero" from "not measured." Prefer a separate exported/omitted
metric (or a `-1`/absent sentinel documented in the exposition) so this doesn't read as a
real measurement. Lower severity than CRITICAL-1 because it's cosmetic/observability-only
and was already flagged in `STUB_REPLACEMENTS.md`, but worth fixing before external
dashboards start alerting on a number that was never real.

---

## MEDIUM-1: `getDohFailureCountStub()` hardcoded to 0 despite a real local DoH cache existing

**File:** `provider/metrics_stubs.go:74-82`, `provider/doh_cache.go`

Comment says failures are "tracked locally via doh_cache.go," but `getDohFailureCountStub`
still just `return 0` rather than reading from that cache. Worth confirming whether
`doh_cache.go` actually exposes a failure counter yet; if it does, this stub should read
it instead of hardcoding zero. If it doesn't yet, same "fabricated zero" concern as HIGH-1
applies at lower severity — DoH failures are a diagnostic signal, not a billing one.

---

## MEDIUM-2: `RegisterProxyBandwidth` return value discarded — check for other silently-dropped constructors

**File:** `provider/provide.go:661`

Beyond the billing consequence covered in CRITICAL-1, `_ = RegisterProxyBandwidth(...)`
is a pattern worth a repo-wide sweep: any other `v2026`-adaptation constructor whose
return value used to be threaded into a live subsystem and is now silently dropped is a
candidate for the same class of bug. I did not have time in this pass to audit every
`_ =` discard in the `provider/` package; recommend `rg -n '_\s*=\s*\w+\(' provider/*.go`
as a follow-up, manually checking each hit for whether the discarded value was
load-bearing in the fork.

---

## Things that looked solid (verified, not just skimmed)

- `cmd/provider/main.go` → `provider.Main()` → docopt dispatch → `provide(opts)` is a real,
  complete call chain; the binary does start and run a provider (CLICAL-2 concern from the
  prior parity pass — "SNProvider not instantiated" — is not what I found; `provide()` in
  `provide.go` directly builds `connect.NewClient`/`NewLocalUserNat`/
  `NewRemoteUserNatProvider` per proxy, which is the actual v2026 provider pattern, not the
  old `SNProvider` type).
- `provideWithProxy`'s auth retry loop (`provide.go:374-535`) is intricate but the state
  machine (proven vs unproven proxy failure thresholds, slow-retry ramping, admission
  gating) looks internally consistent, and every branch either `continue`s the loop,
  returns, or blocks on `<-proxyCtx.Done()` — I did not find a path that busy-loops or
  hangs without a cancellation check.
- `proxyCancelMap` access is consistently guarded by `st.proxyCancelMu` at every call site
  I checked (`provide.go:480-482,694-696,747-758,895-897`), including inside the deferred
  cleanup closures — no obvious data race there.
- `watchReusedIdentityForRevocation` (`provider_auth.go:443+`) exits cleanly on
  `ctx.Done()` or `revocationDone`, and the double-check-after-select comment
  (`provider_auth.go:460-467`) shows real thought was given to the
  renewal-vs-eviction race; no goroutine leak found.
- `provide()`'s shutdown path (`provide.go:76-91`) waits on `st.wg` with a 30s timeout
  and logs rather than hanging forever if a goroutine doesn't drain — reasonable.

---

## Recommendation

Do not treat this as production-ready for fleet deployment until CRITICAL-1 is resolved.
The binary will run and *look* healthy — that's precisely the danger: an operator (or an
automated fleet-health check) watching `/metrics` or the earnings dashboard has no signal
that anything is wrong. Given this was ported in one session against an estimated 2-4
week timeline, I'd expect more gaps of this shape (real code exists, compiles, is called,
but the actual v2026 data seam it depends on was never located) rather than obvious stubs
— the obvious stubs already got caught and fixed in the prior `STUB_REPLACEMENTS.md` pass.
