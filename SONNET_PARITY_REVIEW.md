# H3-Provider Parity Review (Batches A–I)

Reviewer: Sonnet 5, 2026-09-16
Scope: `/home/klets/ur/h3-provider/provider/` (92 files, 28,967 lines) vs
`/home/klets/ur/urnetwork-3.23-fix/provider/` (production fork).
Method: `rg`/`diff` file-list comparison, targeted source reads, `go build`,
`go test ./...`.

## Bottom line

File-level porting of batches A–I is essentially complete — every non-test
`.go` file in the fork has a counterpart here, plus five new stub files that
exist only to satisfy the compiler while batch J (`main.go`) is unported. The
package builds cleanly and `go vet` is clean. But two of those stub files
(`main_stubs.go`, `metrics_stubs.go`) are **silent no-ops sitting underneath
the core proxy-selection pipeline** (`proxy_url_source.go`,
`proxy_grade_paid.go`, `proxy_reload.go`), so a chunk of what "tests pass"
covers is exercising code paths that currently do nothing. This is expected
and by design for a partial port, but it means "tests pass" does not yet mean
"proxy management works" — that's gated entirely on batch J.

## 1. File parity: complete except tests and main.go

```
diff <(ls .../urnetwork-3.23-fix/provider/*.go) <(ls .../h3-provider/provider/*.go)
```

- **Missing here, present in fork**: `main.go`, `main_test.go`,
  `read_fd_frac_unix.go`, `read_fd_frac_windows.go`, and ~75 `*_test.go`
  files whose corresponding source file *is* ported (e.g. `proxy_grade_paid.go`
  exists, `proxy_grade_paid_test.go` doesn't).
- **New here, absent from fork**: `main_stubs.go`, `metrics_stubs.go`,
  `stubs_batch_b.go`, `stubs_batch_c.go`, `stubs_batch_d.go`,
  `stubs_batch_h.go`, plus `proxy_health.go`/`proxy_health_test.go`,
  `proxy_state_test.go`, `promtext_lint_test.go` (net-new, not a fork
  equivalent).

No fork source file is missing without a ported replacement. The gap is
exactly what batch J was scoped to cover, plus a large backlog of unported
unit tests for already-ported files (see §4).

## 2. Confirmed bug: `readFDFrac` lost its Windows build tag

Fork: `readFDFrac()` lives in two files split by build tag —
`read_fd_frac_unix.go` (`//go:build !windows`, uses `syscall.Rlimit` +
`/proc/self/fd`) and `read_fd_frac_windows.go` (`//go:build windows`, stub
returning `-1`).

Here: both were collapsed into `resource_pressure.go` with **no build tag**.
The unix implementation (`syscall.RLIMIT_NOFILE`, `os.ReadDir("/proc/self/fd")`)
now compiles unconditionally:

```go
// resource_pressure.go:294 (h3-provider)
func readFDFrac() float64 {
	var rLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
		return -1
	}
	...
	fdDir, err := os.ReadDir("/proc/self/fd")
```

`syscall.Rlimit`/`RLIMIT_NOFILE` don't exist on `GOOS=windows`. This file will
fail to compile there. Not caught by the current test run (Linux-only CI).
**Fix**: split back into `_unix.go`/`_windows.go` with matching build tags, as
the fork does, or gate the body with a build-tag'd helper.

## 3. Silent no-op stubs under the proxy pipeline

`main_stubs.go` is explicitly labeled as a placeholder for batch J
("stubbed here so the intermediate modules compile... will be removed when
main.go is ported"). That's a reasonable porting strategy, but three of its
stubs are live dependencies of already-ported, already-tested modules:

| Stub (always no-op / empty) | Called from |
|---|---|
| `readProxyConfig()` → `&ProxyConfig{}` | `proxy_url_source.go:248` |
| `writeProxyConfig()` → no-op | `proxy_url_source.go:259` |
| `readProxySettings()` → `nil` | `proxy_url_source.go:344`, `proxy_grade_paid.go:159,453`, `proxy_reload.go:344` |
| `readProxySettingsFromFile()` → `nil, nil` | `proxy_url_source.go:339`, `proxy_grade_paid.go:153,436`, `proxy_reload.go:337` |
| `registerProxyV2026()` → no-op | `proxy_reload.go:429,655` |
| `proxyBandwidthByAddressV2026()` → zero struct | `proxy_reload.go:573,592` |
| `proxyHealthByAddressV2026()` → empty map | `proxy_url_source.go:974` |
| `activeConnectionCount()` (`stubs_batch_h.go`) → `0` | `bandwidth_reporter.go:388` |

Practical effect: the "desired proxy set" computation in
`proxy_url_source.go`/`proxy_grade_paid.go`/`proxy_reload.go` currently always
sees zero configured proxies (no config file, no persisted settings) and
never actually registers a proxy or reads its live bandwidth. Unit tests for
these files pass because they construct `ProxySettings`/state directly rather
than going through `readProxyConfig`/`readProxySettings` — so the stub gap is
invisible to `go test` and will only surface once `main.go` wires a real CLI
flow through this path. Confirmed by checking `connect v2026`
(`/home/klets/h3-workspace/connect`, via the `go.mod` replace directive): it
has no `RegisterProxy`/`ProxyBandwidthByAddress`/`ProxyHealthByAddress`
functions to call, so these aren't just unwired — the upstream API they
adapted no longer exists and the replacement (presumably local proxy-state
tracking, not a connect call) hasn't been designed yet. This is the single
biggest open design question blocking batch J, not a mechanical port task.

## 4. Dead code introduced during the port

`stubs_batch_d.go` defines `atomicBool` ("used by `proxyWarmupDone`" per its
own comment) but `proxyWarmupDone` is actually declared in `main_stubs.go` as
`sync/atomic.Bool`, the real stdlib type. `atomicBool` has zero callers
anywhere in the tree — it's unused scaffolding from an earlier draft. Low
severity, but worth deleting before batch J lands to avoid it silently
becoming load-bearing later.

## 5. Test suite: passes, with one flaky-under-load test

`go test ./...` at the full-suite level fails intermittently:

```
--- FAIL: TestHotSwapParentDrainExitsCleanlyRepeatedly (60.00s)
FAIL	github.com/urfoundation/sn/provider	61.895s
```

Run in isolation (`go test . -run TestHotSwapParentDrainExitsCleanlyRepeatedly -v`)
it passes in 0.02s. This looks like resource contention (fd/goroutine/port
pressure from the ~90 other test files running in the same binary) tripping
the test's internal 30s drain timeout, not a logic regression. Worth a closer
look before batch J adds more concurrent-process tests to the same package,
since the failure mode (hang to a hard timeout) is exactly what a real
hotswap deadlock would also look like — a flaky test with that failure shape
is worse than a flaky test that just errors out.

The ~75 missing `*_test.go` files (§1) are the bulk of the fork's test
coverage for already-ported modules (grading, probing, benchmark, earnings,
auth history, reload, etc.). Batches A–I are code-complete but
test-incomplete relative to the fork; that gap should close before any
batch is called "done," independent of batch J.

## 6. What's NOT a gap

- No fork `.go` source file lacks a ported counterpart (batch J's `main.go`
  aside).
- `go build ./...` and `go vet ./...` are clean.
- `metrics_stubs.go`'s stubs (`stubSetPersistentErrorFunc`,
  `stubSetExtraMetricsProvider`, `prometheusHandlerStub`,
  `getDohFailureCountStub`) are genuine, intentional replacements for connect
  hooks removed in v2026 — each is wired to a local equivalent
  (`providerExtraMetrics`, `doh_cache.go`, `metrics_listen.go`) rather than
  silently dropped. These are fine as-is.
- `gradeTier`, `atomicWriteFile`, `validateJWTExpiry`, `probeHostCount`,
  `sampleProbeTargets` in `stubs_batch_b.go` are full reimplementations, not
  placeholders — they match the fork's behavior and aren't part of the gap.

## Recommendations before/alongside batch J

1. **Fix the Windows build tag on `readFDFrac`** (§2) — small, mechanical,
   independent of batch J.
2. **Delete unused `atomicBool`** (§4).
3. **Decide the replacement design for proxy registration/bandwidth/health**
   before writing batch J's `main.go` port — this is a design gap, not a
   translation gap, since the v2026 connect APIs those stubs adapted don't
   exist to be called. Batch J can't just "port the call site" here.
4. **Backfill the ~75 missing unit test files** for batches A–I, or
   explicitly scope them out — right now "tests pass" is true but covers
   less of the ported surface than the fork's suite does.
5. Investigate `TestHotSwapParentDrainExitsCleanlyRepeatedly` flakiness under
   full-suite load before trusting it as a deadlock canary.
