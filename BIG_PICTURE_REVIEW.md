# Big Picture Review — h3-provider (2026-09-16)

## Session limitations (read before anything else)

Two tool restrictions materially shaped this review and are reported rather than
worked around:

1. **No read access to `/home/klets/ur/urnetwork-3.23-fix/` in this session.**
   Both `Read` and `Bash` were denied for every path under that tree (single files,
   directories, even `stat`/`ls`), while the same tool calls against
   `/home/klets/ur/h3-provider/` worked normally. This is a permission-system
   restriction for this run, not a sandbox/cwd quirk. As a result, **Section 1
   ("Migration Completeness") could not be independently verified against the
   baseline file list** — I could not run `ls` on both trees or diff file lists.
   What follows for Section 1 leans on grep/read evidence from h3-provider itself
   plus cross-referencing claims already recorded in the repo's own prior review
   docs (COMPARISON.md, PARITY_ANALYSIS.md, STUB_REPLACEMENTS.md), which I did
   have access to and did re-verify against current line numbers where cited.
2. **`go` and `gofmt` invocations were denied** (`go version`, `gofmt -l`, `go vet`,
   `go test -race` all failed with "Permission to use Bash has been denied").
   Plain shell commands (`grep`, `ls`, `diff`-via-Read) worked. **I did not run
   gofmt, go vet, or go test in this session — Section 5 findings are static-only
   and I say so explicitly below rather than fabricate pass/fail.**

Everything below is either a direct file:line citation from a `Read`/`grep` I ran
in this session, or explicitly marked as unverified.

---

## 1. Migration Completeness (partially verifiable — see limitation above)

`h3-provider/provider/` contains ~200 files, a full parallel test suite
(`*_test.go` alongside nearly every production file), a `bandwidth/` subpackage
(`tracker.go`, `wrap.go` + tests), and platform-split files (`hotswap_windows.go`,
`hotswap_unix.go`, `peercred_linux.go`/`peercred_other.go`, etc.) mirroring the
3.23-fix structure by name. I cannot confirm this list is complete relative to
3.23-fix without baseline access (see limitation #1).

**Stub/no-op inventory** (`grep -rniE "TODO|stub|not implemented|no-?op" provider/*.go`,
non-test files): the majority are legitimate, documented compatibility shims for
v2026 connect API removals — `compat.go` (`unregisterProxyStub`,
`registerProxyV2026`), `metrics_stubs.go` (`stubSetPersistentErrorFunc`,
`prometheusHandlerStub`, `getDohFailureCountStub`), `contract_metrics.go`. These
are intentional adaptation points, each with a comment naming what upstream
symbol it replaces — this looks like real, deliberate porting work, not
laziness.

One live TODO with no resolution: `provider/proxy_reload.go:210` —
`// TODO: refactor cancelMap and cancelMapMu into a struct owned by ProxyReloader`.
Cosmetic/structural, not a correctness gap. LOW.

**Confirmed missing production symbol (see Section 6 for detail):**
`SampleProbeTargets` is referenced only inside three `//go:build ignore`'d test
files and does not exist anywhere in non-test code
(`grep -rn "func SampleProbeTargets" provider/` returns nothing). This is a real
gap, not a stale-doc artifact — see Section 6.

---

## 2. Race Fix Quality — VERIFIED GOOD

`provider/client_jwt_store.go:60-81`:

```go
var clientJWTStoreMu sync.RWMutex
var globalClientJWTStore = func() *clientJWTStore { ... }()

func loadGlobalClientJWTStore() *clientJWTStore {
	clientJWTStoreMu.RLock()
	s := globalClientJWTStore
	clientJWTStoreMu.RUnlock()
	return s
}

func storeGlobalClientJWTStore(s *clientJWTStore) {
	clientJWTStoreMu.Lock()
	globalClientJWTStore = s
	clientJWTStoreMu.Unlock()
}
```

This is a correct, minimal RWMutex-guarded pointer swap. I checked every call
site across the tree (`renewal_watcher.go:296,300,347,382`, `proxy_warmth.go:68-69,94,100`,
`provider_auth.go:257,282,303,329,421,473`) — **all of them go through
`loadGlobalClientJWTStore()`**, none touch the bare `globalClientJWTStore`
package var directly. The prior race (raw global var read/written across
goroutines with no synchronization, per the commit title) looks genuinely fixed,
not just partially patched.

Test seam (`provider/client_jwt_hotrestart_test.go:15-25`):

```go
func withGlobalStore(t *testing.T, path string) func() {
	t.Helper()
	orig := loadGlobalClientJWTStore()
	storeGlobalClientJWTStore(newClientJWTStore(path))
	return func() { storeGlobalClientJWTStore(orig) }
}
```

Correct pattern: captures via the accessor, swaps, restores via a returned
closure used with `t.Cleanup`/`defer`. No test does a raw assignment to the
global.

The store's own internal state (`clientJWTStore.mu sync.Mutex` guarding
`entries`) is separate and correctly scoped — `Get`/`Put`/`Delete`/`AnyNetworkID`
all take `s.mu` first. `flushLocked` (`client_jwt_store.go:270-339`) reloads the
on-disk file under an inter-process file lock before merging, specifically to
handle the documented F-9 scenario (two `clientJWTStore` instances, e.g. a
HotSwap parent/candidate pair, flushing concurrently) — this is a real, comment-
documented design decision, not incidental.

**No remaining race condition found in this code.** NIT: `proxy_warmth.go:68-69`
and `:94/100` call `loadGlobalClientJWTStore()` twice in a row (nil-check, then
use) instead of caching the pointer in a local — harmless today since nothing
calls `storeGlobalClientJWTStore` mid-request, but slightly wasteful and a latent
TOCTOU if that ever changes. LOW.

---

## 3. Test Determinism

**Finding (MEDIUM) — `TestMain`'s debounce-disable comment does not match the code.**
`provider/main_test.go:16-20`:

```go
func TestMain(m *testing.M) {
	// Disable the reload-trigger debounce for the entire test suite so
	// fetch/merge/reaper tests don't flake from suppressed trigger writes.
	os.Exit(m.Run())
}
```

The comment claims global disablement, but `TestMain` never touches
`writeReloadTriggerDebounce`. The actual `= 0` assignment happens inside two
ordinary test functions in the same file (`main_test.go:70` inside
`TestProxyReloadTrigger_WriteAndRead`, and `:131`), **with no cleanup/restore**,
against a package var whose production default is 30s
(`provider/proxy_reload.go:115`: `var writeReloadTriggerDebounce = 30 * time.Second`).
Because Go runs all tests in a package in one binary/process, this "resets" the
var to 0 for every test that happens to execute *after* those two in binary
order — which is what actually keeps the rest of the suite from flaking, not
`TestMain`. Two real consequences:
- Running a single test in isolation (`go test -run TestSomeReloadDependentTest`)
  gets the real 30s default instead of the suite's effective 0, so a test can
  pass in the full suite and hang/fail/timeout in isolation — the opposite of
  what the comment promises.
- It is fragile to reordering (`-shuffle=on`, file renames that change compile
  order, moving one of the two source tests to another file).

Contrast with `provider/proxy_reload_test.go:439-440` and `:475-476`, which do
this correctly and locally:
```go
writeReloadTriggerDebounce = 500 * time.Millisecond
t.Cleanup(func() { writeReloadTriggerDebounce = 0 })
```
**Suggested fix:** move the `writeReloadTriggerDebounce = 0` assignment into
`TestMain` itself (before `m.Run()`), matching the comment, and remove the
duplicate assignments at `main_test.go:70` and `:131`.

**`time.Sleep`/`time.After` usage:** 78 occurrences across test files
(`grep -rn "time.Sleep\|time.After(" provider/*_test.go`), spread over
`renewal_watcher_test.go`, `proxy_table_probe_review_test.go`, `proxy_health_test.go`,
`proxy_url_source_test.go`, `sn_rpc_test.go`, and others. I did not audit each one
for tightness/flakiness risk given the volume and no `go test -race` run was
possible this session (see limitation #2) — flagging this as an area needing a
dedicated pass, not a confirmed problem. MEDIUM (unverified extent).

**`.bak` test files — root cause identified, and it points to a real bug, not
cruft** (see Section 6, this is the single most actionable finding in this
review).

---

## 4. Security & Correctness

**Finding (MEDIUM) — `atomicWriteJSON` uses a predictable temp filename;
`clientJWTStore.flushLocked` was hardened for the identical race and
`atomicWriteJSON` was not.**

`provider/atomic_write.go:50-51`:
```go
tmp := path + ".tmp"
f, err := os.Create(tmp)
```
Two concurrent callers writing the same `path` (e.g. two goroutines/processes
persisting the same state file) collide on the same `path + ".tmp"` name — one
write can be truncated/interleaved by the other before either renames into
place.

Compare `provider/client_jwt_store.go:312-317`, in the same file's neighborhood
of concerns, which explicitly calls out and fixes this exact class of bug:
```go
// Use unpredictable temp file to prevent collisions with concurrent writers (F-9)
tmpFile, err := os.CreateTemp(dir, ".client_jwts-*.tmp")
```
`atomicWriteFile` (`atomic_write.go:158`) also already does this correctly via
`os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")`. Only
`atomicWriteJSON` is the outlier. **Suggested fix:** change `atomicWriteJSON` to
use `os.CreateTemp` with a random suffix, same as its sibling `atomicWriteFile`
in the same file, three lines below the fix that's already there.

Whether this is reachable by two concurrent callers on the same path in practice
depends on call sites I did not exhaustively trace this session — flagging as
MEDIUM pending confirmation of concurrent callers, not CRITICAL.

**`RenewalOOB` / 401 interception** (`provider/renewal_watcher.go:57-100`):
`RenewalOOBControl` interface plus `RenewalOOB` wrapping `connect.ApiOutOfBandControl`
with `atomic.Uint64` (`audit401Count`) and `atomic.Value` (`on401`) — both fields
are already lock-free-safe types, appropriately used for a callback set from one
goroutine and read from another. Comment at lines 59-62 documents the adaptation
rationale (v2026 connect dropped 401 audit methods) clearly. No issue found in
what I read.

**`sessionFilesAllowlist`** (`provider/control_state.go:465-488`): a
`map[string]bool` allowlist gating which filenames a session-file command may
touch, with a comment noting it mirrors `internal/urnettools/session_cmds.go`'s
allowlist. I confirmed the lookup (`:488`) is a plain map membership check with
no fallthrough/wildcard — looks correctly deny-by-default. I did not cross-check
byte-for-byte parity with the mirrored `urnettools` allowlist (out of scope for
the read budget here); flag as a spot-check item, not a finding.

No credential-leak or path-traversal pattern found in the files I read
(`atomic_write.go`, `client_jwt_store.go`, relevant sections of
`control_state.go`, `renewal_watcher.go`). I did not review every file in the
tree for this — this is not a clean bill of health for the whole package, only
for what I actually opened.

---

## 5. CI Readiness — UNVERIFIED (tooling denied this session)

`.github/workflows/build.yml` has two jobs:
- `test`: `go test -short -race -timeout 600s ./provider/...`, retried up to 3x
  but **only** on a cache-restore flake signature (`Cannot open: File exists` /
  `tar:.*Cannot open`); any other failure exits immediately without retry. This
  is a reasonable, non-overly-permissive retry policy — it doesn't mask genuine
  flakiness.
- `lint`: `go vet ./provider/...`, `gofmt -l provider/` (fails the job on any
  output), and a `continue-on-error: true` `govulncheck` pass.

I could not run `go vet`, `gofmt -l`, or `go test -race` in this session — both
`go` and `gofmt` invocations were denied by the permission system regardless of
target path (see limitation #2). **I have no evidence either way on whether CI
currently passes.** I did not fabricate a pass/fail result. Given the three
`//go:build ignore`'d-but-fixable files identified in Section 6, gofmt/vet
themselves would likely pass (ignored files are excluded from the build), but
that only confirms CI is green *because* those tests are silently skipped, not
that the suite is complete.

---

## 6. What's Still Missing for True Parity

### All `//go:build ignore` test files, with verified reason for each

| File | Reason (verified this session) | Actually fine to leave ignored? |
|---|---|---|
| `hotswap_common_test.go` | `TestHotSwapMessageFraming` is a byte-for-byte duplicate of the same-named test in `hotswap_test.go:50` (same struct literal, same assertions). Confirmed via grep. | **Yes** — true duplicate, safe to delete outright. |
| `hotswap_metrics_test.go` | `TestReadHotswapDeclinesFromDisk` / `TestReadHotswapDeclinesFromDiskMissing` duplicate the same-named tests in `hotswap_test.go:296,330`. Confirmed via grep. | **Yes** — true duplicate, safe to delete outright. |
| `hotswap_exec_path_test.go` | File's own header comment states `withExecutableEnv` and all `TestHotSwapExecutablePath*` are duplicated in `hotswap_test.go`. Comment is accurate per grep. | **Yes** — true duplicate, safe to delete outright. |
| `control_gomemlimit_test.go` | **Not a real duplicate of behavior — an overcorrected dedup.** The only actual collision was the locally-defined helper `captureControlApplyLog`, which also exists in `control_socket_test.go:40` (not ignored). The `.bak` sibling (pre-fix version, confirmed via Read+diff) has the tests **plus** the now-duplicated helper; the current `.go` has the helper deleted but the *entire file* was tagged `//go:build ignore` instead of just removing the 5 duplicate lines. Three real regression tests (`TestClearGomemlimitRestoresUnlimitedNotZero`, `TestApplyLiveSideEffectLogsTheEffectiveValue`, `TestGogcDisabledIsAcceptedAndDisablesCollection`) are dead in CI as a result. | **No — this is a bug in the dedup fix itself.** Fix: delete the `//go:build ignore` line; the file compiles clean since the duplicate helper is already gone from this file and lives in `control_socket_test.go`. |
| `control_metrics_lifecycle_test.go` | Same pattern: only actual conflict was nothing left over (checked — file has no duplicate defs at all versus `.bak`), yet it's still tagged ignore. Tests `TestClearMetricsStopsTheListener` and `TestClearReportsFailureAsFailure` (the latter uses `readFileString`, which already lives non-duplicated in `control_socket_test.go:46`) are dead in CI for no compile reason I could find. | **No** — same bug class as above; removing the ignore tag should just work. |
| `control_socket_robust_test.go` | Same pattern: original defined `syscallEMFILE()` locally (kept in `.bak`); current file has it removed because `control_socket_test.go:935` already defines it, and `acceptLoopShouldStop` is production code (`control_socket.go:176`), not a test symbol. Two real regression tests for an accept-loop EMFILE-kills-the-listener bug and a missing write-deadline bug are dead. | **No** — same bug class; removing the ignore tag should just work. |
| `proxy_grade_paid_test.go` | Genuinely blocked: calls `SampleProbeTargets(...)` (`:412`, `:467`) which **does not exist anywhere in production code** (`grep -rn "func SampleProbeTargets" provider/` returns nothing). Every other helper it uses (`listenSocks5Sequenced`, `seedProbeDNSForAddress`, `tableProbePassCounter`, `writeReviewProbeOverride`, `runPaidProxyGradeOnce`) already exists and is live. The file's own header TODO ("references connect types ... that moved to local subpackages") is **stale/misleading** — `connect.ProxyBandwidth`/`RegisterProxyBandwidth` are NOT missing (confirmed live at `proxy_health.go:141-142`, `bandwidth/tracker.go:26`); the only real blocker is `SampleProbeTargets`. | Correctly ignored for now, but the stated reason in the file is wrong and should be corrected so the next person doesn't waste time chasing the wrong symbol. |
| `proxy_grade_paid_earnskip_test.go` | Same `SampleProbeTargets` gap (same TODO header, same stale reasoning about `ProxyBandwidth`). | Correctly ignored; same TODO-text fix needed. |
| `proxy_grade_paid_ticker_test.go` | Header TODO: `references writePaidGradeProbeOverride which does not exist in h3-provider` — **also stale**: `writePaidGradeProbeOverride` is defined at `proxy_grade_paid_test.go:26`, it's just that *that file* is also ignored, so the symbol is unavailable only because of the chain, not because it's absent from the design. | Correctly ignored as a consequence of `proxy_grade_paid_test.go` being ignored; not an independent gap. |
| `proxy_probe_adaptive_test.go` | Tests "adaptive sample growth" behavior per its header comment; very likely hits the same missing `SampleProbeTargets` symbol (not fully traced this session, consistent with the pattern above). | Likely same root cause; worth confirming against the `SampleProbeTargets` gap before assuming it needs separate work. |
| `proxy_state_writeerror_test.go` | Header comment describes legitimate coverage-gap tests (`writeProxyStateTo` error paths, 53% coverage). Did not find a duplicate/missing-symbol reason in the portion read; needs a compile attempt to confirm why it's ignored (not established this session — `go build` was denied). | Unclear — flag for follow-up with tooling access. |

### Connect library symbols: current status (grepped both this session)

- `RegisterProxyBandwidth` — **exists locally**, `provider/proxy_health.go:142`, called from `provide.go:352`. Not missing.
- `connect.ProxyBandwidth` type — **exists**, reimplemented as `provider/bandwidth/tracker.go:26` (`type ProxyBandwidth struct`). Not missing; the "moved to local subpackages" TODO text in the ignored test files is describing this correctly for `ProxyBandwidth`/`RegisterProxyBandwidth` but incorrectly implies `SampleProbeTargets` moved too — it didn't move anywhere, it doesn't exist.
- `SampleProbeTargets` — **genuinely missing**. No definition anywhere under `provider/` or `provider/bandwidth/`. This is the one real, currently-unported piece of functionality blocking 4 test files (`proxy_grade_paid_test.go`, `proxy_grade_paid_earnskip_test.go`, `proxy_grade_paid_ticker_test.go` transitively, and likely `proxy_probe_adaptive_test.go`).
- `ProbeHostCount` — only appears in comments (`proxy_table_probe.go:75,627`), never called as a function anywhere, test or production. Either it was always a conceptual/planned constant that never got implemented as a real accessor, or it's dead documentation language referring to something inlined elsewhere in `probeTableThroughProxy` (`proxy_table_probe.go:415`). Not confirmed to be a functional gap — flag as needing author clarification, not a bug.

### Concrete, prioritized punch list before this could replace 3.23-fix in production

1. **(Highest value, lowest effort)** Un-ignore `control_gomemlimit_test.go`,
   `control_metrics_lifecycle_test.go`, `control_socket_robust_test.go` by
   deleting their `//go:build ignore` lines, then delete the three `.bak` files.
   This recovers 7 real regression tests for real bugs (zero-byte GC limit
   pegging CPU, `clear metrics` no-op, EMFILE killing the control-socket accept
   loop permanently) that are currently silently absent from CI. Verify with a
   local `go test` run once tooling access is available — I could not run this
   myself this session.
2. Fix the `TestMain` comment/behavior mismatch in `main_test.go:16-20` — move
   `writeReloadTriggerDebounce = 0` into `TestMain` itself so isolated single-test
   runs match full-suite behavior.
3. Fix `atomicWriteJSON`'s predictable temp filename (`atomic_write.go:50-51`) to
   match the `os.CreateTemp`-with-random-suffix pattern already used by
   `atomicWriteFile` in the same file and `clientJWTStore.flushLocked`.
4. Correct the stale TODO headers in `proxy_grade_paid_test.go` and
   `proxy_grade_paid_earnskip_test.go` — they blame `ProxyBandwidth`/
   `RegisterProxyBandwidth`, which exist; the real, sole blocker is
   `SampleProbeTargets`, which doesn't. Either port/re-implement
   `SampleProbeTargets` (or point the tests at whatever `probeTableThroughProxy`
   uses internally instead) to recover this coverage, or explicitly decide the
   adaptive-sample-growth behavior isn't going to be ported and delete the tests
   with a clear note.
5. Get `go`/`gofmt`/`go vet`/`go test -race` actually run once (this session's
   tooling denial blocked all four) to establish a real, current CI-readiness
   baseline rather than relying on the workflow file's intent.
6. Re-run a migration-completeness diff against `urnetwork-3.23-fix/provider/`
   once cross-repo read access is available — this review could not do the
   file-list/test-coverage diff in Section 1 that was explicitly requested.
7. Audit the 78 `time.Sleep`/`time.After` occurrences in test files for
   race-detector flakiness, ideally alongside item 5's `-race` run so failures
   are attributable.

---

## PARITY SCORE: 6 categories reviewed, 0 fully clean, 1 fully verified-good, rest partial/blocked by tooling

1. **Migration Completeness** — PARTIAL. Could not diff against baseline (no
   read access this session); confirmed one real missing symbol
   (`SampleProbeTargets`) and that most "stub" hits are intentional, documented
   adaptations rather than gaps.
2. **Race Fix Quality** — COMPLETE / VERIFIED GOOD. `globalClientJWTStore`
   RWMutex pattern is correct and consistently used at every call site; no
   remaining race found in the code read.
3. **Test Determinism** — PARTIAL, one confirmed bug. `TestMain`'s debounce-
   disable comment doesn't match what the code does (real, fixable, cited
   above); `.bak` files traced to a real overcorrection bug, not cruft;
   `time.Sleep` audit not completed (78 hits, unaudited).
4. **Security & Correctness** — PARTIAL. One real inconsistency found
   (`atomicWriteJSON` temp-file race, unlike its already-hardened siblings);
   `RenewalOOB` and `sessionFilesAllowlist` look sound in what was read.
5. **CI Readiness** — UNVERIFIED. `go`/`gofmt` tooling was denied this session;
   workflow config itself looks reasonable (bounded flake-only retry, separate
   lint job) but no actual pass/fail evidence was collected.
6. **True Parity Gaps** — MOSTLY MAPPED. 11 `//go:build ignore` files
   individually diagnosed with verified reasons; 3 of them are disabled for no
   real reason (compile-conflict already resolved, ignore tag left on by
   mistake) and 3 are true duplicates safe to delete; `SampleProbeTargets` is
   the one confirmed real missing production symbol; `ProbeHostCount` status
   ambiguous (comment-only, never called).
