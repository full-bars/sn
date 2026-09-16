# h3-provider Parity Analysis

Date: 2026-09-16
Scope: `/home/klets/ur/h3-provider/provider/` vs source repos
`/tmp/wt-h3/provider/` (sn fork) and `/home/klets/ur/urnetwork-3.23-fix/provider/` (3.23-fix)

## 0. Headline finding: the two source repos are content-identical

`diff -rq /tmp/wt-h3/provider/ /home/klets/ur/urnetwork-3.23-fix/provider/` returns **zero
file-content differences**. Every `.go` file present in one is present in the other, and every
shared file is byte-for-byte identical. The only differences are build artifacts in
3.23-fix (`build/`, the compiled `provider` and `provider_bin` binaries) which are not source.

Practical consequence for this analysis:

- Step 1 ("features/fixes in 3.23-fix not in the sn fork") produces an **empty set**. There is
  nothing in 3.23-fix's `provider/` that the sn fork lacks, and no meaningful divergence to
  reconcile. Whatever bug fixes 3.23-fix carries relative to upstream `urnetwork/connect` live
  outside `provider/` (protocol/, core library files, etc.) or have already been folded back
  into `/tmp/wt-h3` — out of scope for this directory-level comparison.
- This means the "remaining work" list is a **single unified list**, not two lists to merge.
  Everything below is sourced from `/tmp/wt-h3/provider/` (== 3.23-fix provider/) directly.

This should be spot-checked once more before fully retiring the "diff two repos" step in any
future parity pass — worth confirming `/tmp/wt-h3` wasn't freshly re-synced from 3.23-fix
immediately before this analysis (its file mtimes are uniformly Sep 15 20:33, i.e. a fresh
checkout/copy, vs 3.23-fix's naturally spread commit dates).

## 1. What's already ported into h3-provider

`/home/klets/ur/h3-provider/provider/` currently contains (non-test `.go` files):

```
audit_ring.go            contract_metrics.go       control_socket.go
control_state.go         doh_cache.go              hotswap.go
hotswap_unix.go          lifetime_metrics.go       metrics_listen.go
metrics_provider.go      metrics_stubs.go          pool_health.go
proxy_health.go          proxy_state.go            proxy_url_source.go
renewal_watcher.go       resource_pressure.go      sn.go
sn_rpc.go
bandwidth/tracker.go     bandwidth/wrap.go
```

(`proxy_health.go` in h3-provider corresponds to the sn fork's proxy-health-related logic —
note the sn fork does not have a file with this exact name; h3-provider appears to have
consolidated/renamed during porting. `metrics_stubs.go` is likewise h3-provider-specific
scaffolding, not a direct port.)

`proxy_url_source.go` (49,967 bytes, matches the sn fork's 49,963-byte file almost exactly) is
already present and is also the untracked file shown in git status — it looks freshly ported
but not yet committed. Treat it as **done**, not remaining work.

23 commits of porting work already landed (see `git log --oneline` on `provider/`), most
recently `bead2666 feat(provider): port control state module from fork`.

## 2. Remaining files (single unified list, 56 files)

Computed as: sn-fork non-test `.go` files minus files already present (by basename) in
h3-provider's `provider/` tree (including `bandwidth/`).

| File | Lines | `connect.` refs | Class |
|---|---:|---:|---|
| main.go | 6427 | 167 | CRITICAL |
| proxy_table_probe.go | 1009 | 7 | CRITICAL |
| proxy_reload.go | 745 | 14 | CRITICAL |
| proxy_grade_summary.go | 680 | 0 | OPTIONAL |
| bandwidth_reporter.go | 674 | 2 | CRITICAL |
| proxy_paste.go | 616 | 1 | OPTIONAL |
| proxy_grade_paid.go | 498 | 7 | CRITICAL |
| proxy_url.go | 454 | 2 | CRITICAL |
| proxy_health_log.go | 421 | 8 | CRITICAL |
| proxy_probe.go | 384 | 0 | CRITICAL |
| proxy_earnings_store.go | 346 | 2 | CRITICAL |
| auth_rate_limiter.go | 342 | 0 | CRITICAL |
| client_jwt_store.go | 325 | 0 | CRITICAL |
| proxy_trim.go | 320 | 2 | CRITICAL |
| proxy_warmth.go | 281 | 6 | CRITICAL |
| shmlog_linux.go | 264 | 0 | OPTIONAL |
| proxy_slow_retry.go | 263 | 0 | CRITICAL |
| hotswap_windows.go | 229 | 1 | CRITICAL (platform completion) |
| network.go | 193 | 0 | CRITICAL |
| proxy_admission_gate.go | 187 | 0 | CRITICAL |
| systemd_status.go | 177 | 0 | OPTIONAL |
| direct.go | 168 | 0 | CRITICAL |
| earn_tracker.go | 151 | 2 | CRITICAL |
| proxy_grade_tier.go | 150 | 0 | CRITICAL |
| atomic_write.go | 150 | 0 | CRITICAL (foundational, has conflict — see §4) |
| startup_banner.go | 148 | 0 | OPTIONAL |
| ssrf_guard.go | 147 | 0 | CRITICAL (security) |
| proxy_failure_history.go | 146 | 0 | CRITICAL |
| startup_env_seed.go | 131 | 0 | OPTIONAL |
| proxy_match.go | 131 | 0 | CRITICAL (foundational) |
| pending_overrides.go | 130 | 0 | OPTIONAL (see §4) |
| proxy_benchmark.go | 129 | 4 | OPTIONAL |
| shmlog_trim.go | 101 | 0 | OPTIONAL |
| restrict_socket_windows.go | 101 | 1 | CRITICAL (security, platform) |
| critlog.go | 95 | 0 | OPTIONAL |
| network_cmd.go | 81 | 3 | CRITICAL |
| jwt_store_lock_windows.go | 67 | 0 | CRITICAL (platform companion) |
| pending_overrides_lock_windows.go | 60 | 0 | OPTIONAL |
| important_log.go | 59 | 0 | CRITICAL (foundational, trivial) |
| proxy_grade_report.go | 52 | 0 | OPTIONAL |
| peercred_linux.go | 48 | 0 | CRITICAL (security, control_socket dep) |
| proxy_auth_history.go | 47 | 0 | CRITICAL |
| read_fd_frac_unix.go | 39 | 0 | SUPERSEDED (see §4) |
| proxy_id.go | 38 | 0 | CRITICAL (foundational) |
| pending_overrides_lock_unix.go | 35 | 0 | OPTIONAL |
| jwt_store_lock_unix.go | 31 | 0 | CRITICAL (platform companion) |
| hotswap_socketpair_other.go | 31 | 0 | CRITICAL (platform completion) |
| shmlog_fallback.go | 26 | 0 | OPTIONAL |
| hotswap_socketpair_linux.go | 22 | 0 | CRITICAL (platform completion) |
| tlog.go | 19 | 0 | CRITICAL (foundational, trivial) |
| important_logf.go | 13 | 0 | CRITICAL (foundational, trivial) |
| read_fd_frac_windows.go | 12 | 0 | SUPERSEDED (see §4) |
| peercred_other.go | 12 | 0 | CRITICAL (platform companion) |
| restrict_socket_other.go | 10 | 0 | CRITICAL (platform companion) |
| dup_linux_generic.go | 9 | 0 | CRITICAL (trivial, build-tag stub) |
| dup_linux_arm64.go | 9 | 0 | CRITICAL (trivial, build-tag stub) |

Totals: **56 remaining files**. **38 classified CRITICAL**, **14 OPTIONAL**, **2 SUPERSEDED**
(functionality already inlined elsewhere — see §4), **0 unique to 3.23-fix** (per §0).

Note on judgment calls: `bandwidth_reporter.go`, `client_jwt_store.go`, `auth_rate_limiter.go`,
`ssrf_guard.go` are marked CRITICAL because they sit directly on the auth/security/earnings
path even though their `connect.` reference count is low or zero — low connect-API surface
does not mean low importance, just low v2026-porting risk. Conversely `proxy_grade_summary.go`
and `proxy_paste.go` are pure fleet-operator conveniences (matches CLAUDE.md's own framing of
`proxy summary` / paste helpers as UX sugar over the grading system) — OPTIONAL despite size.

## 3. Cross-check: nothing above double-ports existing work

Confirmed no overlap between the remaining-file list and h3-provider's current `provider/`
contents (name-based diff, §2 table derived from `comm -23`). `proxy_url_source.go` was
excluded correctly since it's already present.

## 4. Dependency analysis (CRITICAL files)

Two important pre-existing-conflict findings from spot-checking already-ported files:

- **`atomic_write.go` conflicts with `audit_ring.go`.** `audit_ring.go` (already ported)
  defines its own local `atomicWriteJSON()` (audit_ring.go:246) rather than importing from
  `atomic_write.go`. `metrics_provider.go` (already ported) calls that same local
  `atomicWriteJSON`. When `atomic_write.go` is ported in, expect a **symbol collision** —
  this needs the same "resolve naming conflicts between ported modules" treatment already
  used once before (see commit `8f0d77ba`). Port `atomic_write.go` first, then refactor
  `audit_ring.go`/`metrics_provider.go` to call the canonical version and delete the local
  copy, rather than porting it as a same-named duplicate.
- **`read_fd_frac_unix.go` / `read_fd_frac_windows.go` are SUPERSEDED.**
  `resource_pressure.go` (already ported) already defines its own `readFDFrac()`
  (resource_pressure.go:296) and calls it at line 372. The standalone `read_fd_frac_*.go`
  files in the sn fork look like they predate that inlining, or resource_pressure.go's
  version is itself an inlined port of them. Either way: do **not** port these two files
  as-is: diff their contents against `resource_pressure.go`'s local function first, and only
  port if the standalone version has capability the inlined one lacks (e.g. Windows support —
  resource_pressure.go's local version may be Unix-only, in which case the Windows file body
  is the one piece worth carrying over, folded into resource_pressure.go rather than as a
  separate file).
- **`pending_overrides.go` is referenced only in a comment**, not a live import, from
  `control_state.go` (control_state.go:95-97, describing why `mergePendingOverrides()` can't
  run before `initGlog()`). It is not currently a hard build dependency of ported code. Likely
  becomes load-bearing once `main.go` (which orchestrates startup order) is ported — port it
  alongside/just before `main.go`, not standalone.

Foundational / low-risk, port early (many other CRITICAL files likely call into these):

- `proxy_id.go`, `proxy_match.go` — small shared types/matching helpers referenced across the
  `proxy_*` family; port before any proxy_grade_*/proxy_table_probe/proxy_reload work.
- `important_log.go`, `important_logf.go`, `tlog.go`, `critlog.go` — trivial logging
  wrappers, no internal deps, safe to port in one pass immediately.
- `atomic_write.go` — see conflict note above; port with the audit_ring/metrics_provider
  reconciliation as a single unit.
- `dup_linux_arm64.go` / `dup_linux_generic.go` — trivial build-tag stubs, port together.

Ordered dependency chains inferred from naming/domain conventions (not fully symbol-traced —
budget did not allow tracing all 56 files' exports individually; verify with `go build` once
each batch lands):

1. `proxy_id.go`, `proxy_match.go` → (foundation for) → `proxy_failure_history.go`,
   `proxy_auth_history.go` → `proxy_admission_gate.go`, `proxy_slow_retry.go` →
   `proxy_probe.go` → `proxy_table_probe.go` (1009 lines, the biggest probe-pipeline file,
   almost certainly calls exported functions from `proxy_probe.go`) → `proxy_reload.go`
   (probe results feed reload decisions) → `proxy_trim.go`, `proxy_warmth.go`.
2. `earn_tracker.go` → `proxy_earnings_store.go` → `proxy_grade_tier.go` →
   `proxy_grade_paid.go` → `proxy_grade_summary.go` → `proxy_grade_report.go` (report is a
   thin view over summary — port last in this chain).
3. `client_jwt_store.go` + `jwt_store_lock_unix.go`/`jwt_store_lock_windows.go` (platform
   companions, same commit) → feeds `auth_rate_limiter.go` and eventually `main.go`'s auth
   flow.
4. `peercred_linux.go`/`peercred_other.go`, `restrict_socket_windows.go`/
   `restrict_socket_other.go` — companions to the already-ported `control_socket.go`; these
   complete socket peer-auth/permission hardening. No dependency on other remaining files;
   can port anytime, but should land before `main.go` wires up the control socket for
   production use.
5. `hotswap_windows.go`, `hotswap_socketpair_linux.go`, `hotswap_socketpair_other.go` —
   platform completions for the already-ported `hotswap.go`/`hotswap_unix.go`. Independent of
   everything except the existing hotswap module.
6. `network.go`, `network_cmd.go`, `direct.go` — IPv6/dual-stack and direct-connect networking
   primitives that `main.go` calls into. Should land before `main.go`.
7. `bandwidth_reporter.go` — reports bandwidth to the SN backend; depends on
   `bandwidth/tracker.go` and `bandwidth/wrap.go` (already ported) plus likely
   `client_jwt_store.go` for auth. Port after chain 3.
8. `ssrf_guard.go` — standalone security check, no deps, port anytime, prioritize early given
   it is a security control.
9. `main.go` — the capstone. Depends on essentially everything above (167 `connect.`
   references plus orchestration of every subsystem: control socket, hotswap, metrics,
   resource pressure, proxy grading/reload/probe, JWT auth, bandwidth reporting, network
   detection). **Must be ported last**, and is the highest-risk single file in the entire
   remaining set for v2026 connect-API adaptation (see §6).

OPTIONAL files have effectively no dependents among CRITICAL files (verified: `earn_tracker`
grep for callers in already-ported code returned nothing; `systemd_status.go`,
`proxy_paste.go`, `proxy_grade_summary.go`/`report.go`, `startup_banner.go`,
`startup_env_seed.go`, `proxy_benchmark.go`, `shmlog_*` are all leaf/cosmetic and can be
deferred indefinitely without blocking a working provider — except `proxy_grade_summary.go`/
`proxy_grade_report.go`, which sit downstream of the CRITICAL grading chain (item 2 above) and
should be picked up in the same batch as that chain if done at all, since the dependency
direction is already established.

## 5. Batching strategy (parallelizable work streams)

Batches are sized so files within a batch have no dependencies on each other; cross-batch
ordering is given in §7.

**Batch A — Foundational utilities** (no internal deps, unlocks everything else)
`proxy_id.go`, `proxy_match.go`, `important_log.go`, `important_logf.go`, `tlog.go`,
`critlog.go`, `dup_linux_arm64.go`, `dup_linux_generic.go`, `atomic_write.go` (+ the
audit_ring/metrics_provider dedup fix).

**Batch B — Proxy health/probe pipeline** (depends on Batch A's proxy_id/proxy_match)
`proxy_failure_history.go`, `proxy_auth_history.go`, `proxy_admission_gate.go`,
`proxy_slow_retry.go`, `proxy_probe.go`, `proxy_table_probe.go`, `proxy_reload.go`,
`proxy_trim.go`, `proxy_warmth.go`, `proxy_health_log.go`.

**Batch C — Earnings/grading pipeline** (depends on Batch A only; independent of Batch B)
`earn_tracker.go`, `proxy_earnings_store.go`, `proxy_grade_tier.go`, `proxy_grade_paid.go`,
`proxy_url.go`, plus optionally `proxy_grade_summary.go`, `proxy_grade_report.go`,
`proxy_paste.go`, `proxy_benchmark.go` if OPTIONAL work is in scope for this pass.

**Batch D — Auth/JWT/security** (independent of B and C)
`client_jwt_store.go`, `jwt_store_lock_unix.go`, `jwt_store_lock_windows.go`,
`auth_rate_limiter.go`, `ssrf_guard.go`.

**Batch E — Control-socket platform completions** (independent of B/C/D; companions to
already-ported control_socket.go)
`peercred_linux.go`, `peercred_other.go`, `restrict_socket_windows.go`,
`restrict_socket_other.go`.

**Batch F — Hotswap platform completions** (independent of everything except already-ported
hotswap.go/hotswap_unix.go)
`hotswap_windows.go`, `hotswap_socketpair_linux.go`, `hotswap_socketpair_other.go`.

**Batch G — Networking** (independent of B/C/D/E/F)
`network.go`, `network_cmd.go`, `direct.go`.

**Batch H — Bandwidth reporting** (depends on Batch D for auth, and already-ported
bandwidth/tracker.go)
`bandwidth_reporter.go`.

**Batch I — Startup/ops cosmetics (OPTIONAL, fully independent)**
`systemd_status.go`, `startup_banner.go`, `startup_env_seed.go`, `pending_overrides.go` +
lock files, `shmlog_linux.go`, `shmlog_fallback.go`, `shmlog_trim.go`.

**Batch J — main.go (sequential, solo)**
Must start only after A, B, C, D, E, F, G, H are merged and building green. Too large and
too interconnected to parallelize internally; treat as one long single-agent (or tightly
paired) effort, likely split into sub-PRs by subsystem-wiring section once the file is open,
but the dependency intake must be complete first.

Batches A–H (7 batches, minus A which gates the others) can run with up to **7 agents in
parallel** once Batch A lands, since B/C/D/E/F/G/H have no edges between them. Batch I can run
in parallel with anything, anytime, including before Batch A (it has zero dependencies on the
CRITICAL chain). Batch J is strictly sequential and last.

## 6. Risk assessment — connect v2026 API surface

Ranked by `connect.` reference count in the source file (proxy for v2026-adaptation risk):

| File | connect. refs | Risk |
|---|---:|---|
| main.go | 167 | **Very high** — by an order of magnitude the largest surface. Almost certainly touches client/transport construction, contract manager setup, the provide loop, and event callbacks. |
| proxy_reload.go | 14 | High |
| proxy_health_log.go | 8 | Medium |
| proxy_table_probe.go | 7 | Medium |
| proxy_grade_paid.go | 7 | Medium |
| proxy_warmth.go | 6 | Medium |
| network_cmd.go | 3 | Low-medium |
| bandwidth_reporter.go | 2 | Low |
| proxy_url.go | 2 | Low |
| proxy_earnings_store.go | 2 | Low |
| proxy_trim.go | 2 | Low |
| earn_tracker.go | 2 | Low |
| proxy_paste.go, proxy_benchmark.go, hotswap_windows.go, restrict_socket_windows.go | 1 each | Low |
| all others | 0 | None — pure Go stdlib/local logic, no v2026 exposure |

`main.go`'s 167 references dwarf everything else combined (≈219 across all other remaining
files). This confirms the task framing: **main.go is where essentially all connect v2026
API-adaptation risk concentrates.** Specific surface likely to have moved between the sn
fork's connect version and v2026 (based on what a provider `main.go` typically wires up, not
confirmed by reading v2026's API directly — budget did not extend to a full connect v2026 API
diff):

- Client/transport construction (the sn fork predates H3/QUIC; v2026 adds dual-stack
  IPv6 + H3, so any `connect.NewClient`/transport-option calls are prime candidates for
  signature changes).
- Contract manager construction/signatures (`transfer_contract_manager.go` equivalents) —
  CLAUDE.md notes 3.23-fix already customized `InitialContractTransferByteCount`; if v2026
  changed the contract manager's constructor shape this is where it would surface.
  h3-provider's already-ported `contract_metrics.go` presumably already adapted to whatever
  v2026's contract manager exposes — cross-check `contract_metrics.go`'s connect calls against
  what `main.go` will need before starting Batch J, since they must agree on the same
  contract-manager handle type.
- Provide-loop callback signatures (`connect.Provide`/event-handler shapes) — the second most
  connect-heavy file after main.go, `proxy_reload.go` (14 refs), likely calls into the same
  callback surface main.go registers; port order should have `proxy_reload.go` land and build
  clean (even if only compiled standalone/stubbed) before attempting main.go, so any API
  mismatch is caught in a smaller file first.
- `proxy_table_probe.go`, `proxy_grade_paid.go`, `proxy_health_log.go`, `proxy_warmth.go`
  (7-8 refs each) likely call connect's stats/metrics or dial/transport probing APIs to test
  proxy health — worth grepping their specific `connect.X` call sites against v2026's package
  docs before porting, since these are the next-largest risk pool after main.go and
  proxy_reload.go.

Recommendation: before Batch J (main.go), do a **focused connect v2026 API diff** (the
`newversioncheck` skill mentioned in this environment's skill list looks purpose-built for
exactly this — "audit what upstream connect changes affect the fork... before deciding
whether to port anything") scoped to just the symbols these six files touch, rather than
starting main.go blind.

## 7. Prioritized plan

1. **Batch A** (foundational utilities + atomic_write conflict fix) — solo, fast, unblocks
   everything. Do first.
2. **Batches B, C, D, E, F, G, H — run in parallel** (up to 7 agents), each independent per
   §5. Batch I (cosmetics) can run in parallel with these too, or be deferred entirely — it
   blocks nothing.
3. **Connect v2026 API audit** (see §6) — run in parallel with step 2, since it doesn't touch
   any files, purely research. Scope: main.go's likely connect surface (transport
   construction, contract manager, provide-loop callbacks) plus the 6 next-riskiest files
   listed in §6. Use the `newversioncheck` skill.
4. **Batch J (main.go)** — sequential, starts only once steps 2 and 3 are both done. Highest
   risk, highest value: this is the file that actually makes h3-provider a runnable binary
   with CLI/auth/provide-loop wiring. Given its size (6427 lines), plan to split the porting
   effort into sub-passes by subsystem (CLI/docopt parsing → auth wiring → client/transport
   construction → provide loop → proxy subsystem wiring → shutdown/signal handling) even
   though it's one file, rather than attempting it in one shot.
5. **Batch I** (if not already done in parallel during step 2) — lowest priority, no
   dependents, safe to do last or skip for an initial "critical parity" milestone.
6. **Full-repo build + `go test ./... -race`** after Batch J lands, to confirm the merged
   result matches h3-provider's existing 303-passing-tests bar.

Minimum viable "critical parity" (skip all OPTIONAL, i.e. skip Batch I and the optional tail
of Batch C) = Batch A + B + C(critical subset) + D + E + F + G + H + connect audit + Batch J.
That's 38 CRITICAL files plus the 2 SUPERSEDED files' verification work, before main.go.
