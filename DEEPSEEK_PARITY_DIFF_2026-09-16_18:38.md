# DeepSeek Parity Diff Review — 2026-09-16_18:38 UTC

## Scope and Method

Only function signatures and stub/TODO counts were provided. No full source bodies were available. This review is therefore a structural and name-level comparison, not a behavioral proof. It identifies likely deviations, adaptation patterns, and areas of risk based on the visible surface.

## Summary Verdict

**Cannot confirm full behavioral parity.**  
NEW appears to be a substantial refactor with new subsystems (proxy health state files, proxy table probing, Eth/SN RPC, resource pressure, bandwidth reporter) and many stubs/TODOs. Several OLD test-covered areas (control socket, lifetime metrics, JWT, proxy reaper/lock) are not visible in the NEW sample signatures; they may be renamed, moved, or still stubbed. The NEW code likely does **not** yet do everything the OLD code does, especially where stub counts are high.

---

## Focus Area Deviations

### 1. provide() flow

- **OLD:** No direct `provide()` signature in sample; tests imply provide flow via control socket, reload triggers, proxy lock, and reaper.
- **NEW:** `chooseNetworkCmd(opts docopt.Opts)` suggests CLI entrypoint selection; `stubs_provide.go` has 1 stub/TODO, indicating the provide flow may be incomplete.
- **Why changed:** Migration to docopt CLI and modular command selection.
- **Matters:** Yes. If the `provide()` entrypoint is stubbed, core provider startup may not work.

### 2. Proxy registration

- **OLD:** `TestFetchAndMergeProxyURLs_MarksApiOKAndSocks5OnlyCorrectly`, `TestRunURLProxyReaperOnce_*`, `TestAcquireProxyLockWithRetry_*` cover proxy URL merge, reaper, and lock retry.
- **NEW:** `parseProxyString`, `capProxyList`, `probeAndGradeProxyURLLines`, `urlProxyPassesAdmission`, `cachedProxyURLState`, `cachedProxyURLScore`, `resetAdmissionStateCache` indicate a new admission/grading layer. `proxy_reload.go` has 12 stubs, `proxy_paste.go` has 21 stubs.
- **Why changed:** Proxy registration now includes probing, scoring, admission cache, and paste/reload paths.
- **Matters:** High stub counts in `proxy_reload` and `proxy_paste` mean registration/reload may be incomplete. Old reaper/lock tests are not visible.

### 3. Bandwidth tracking

- **OLD:** `TestRefreshJWT_TransferStats` and lifetime metrics tests imply bandwidth/transfer stats tracked with JWT refresh.
- **NEW:** `bandwidth_reporter.go` has 4 stubs; `formatTrafficStateFile`, `writeProxyTrafficState` suggest traffic state is persisted. No visible bandwidth reporter tests.
- **Why changed:** Bandwidth reporting separated into its own file with stubs.
- **Matters:** Yes. If bandwidth reporter is stubbed, transfer stats may not be reported.

### 4. Health monitoring

- **OLD:** No explicit health state file functions in sample; tests for reaper and control socket status.
- **NEW:** Many functions for health state: `writeProxyHealthState`, `writeProxyTrafficState`, `writeProxyHealthEvents`, `formatStateFile`, `formatTrafficStateFile`, `formatEventLines`, `rotateIfNeeded`, `startRetentionEventWriter`, `appendRetentionEvent`, `flushRetentionEvents`, `writeRetentionEventLine`.
- **Why changed:** Health monitoring now persists state/events with rotation and retention.
- **Matters:** Likely an improvement, but old reaper tests are not visible. Need to verify the reaper still exists and behaves correctly.

### 5. Error handling

- **OLD:** Tests for control socket persistence failure rollback, unknown key/command, stale socket, permissions, etc.
- **NEW:** No control socket test signatures in sample; `resource_pressure.go` has 20 stubs, `sn_rpc.go` has 10 stubs, `proxy_reload.go` has 12 stubs. Error handling likely incomplete in new subsystems.
- **Why changed:** New subsystems introduce new error paths; old control socket error handling may be moved or stubbed.
- **Matters:** Yes. Missing control socket tests/stubs could regress error handling.

### 6. CLI commands

- **OLD:** No CLI command function in sample; tests for control socket commands (`set/get/clear/status/version`).
- **NEW:** `chooseNetworkCmd(opts docopt.Opts)` is explicit CLI command selection. `snRPCServer` suggests a new RPC command.
- **Why changed:** Migration to docopt CLI and added SN RPC server.
- **Matters:** CLI surface changed. Need to verify old control socket commands are still available.

### 7. Metrics

- **OLD:** `TestLifetimeMetrics_*` (AddAndSnapshot, NegativeDeltasIgnored, PersistenceRoundTrip, MissingFileStartsEmpty, CorruptFileStartsEmpty, ThrottledFlush) and `metrics_listen.go` with 11 stubs.
- **NEW:** `lifetime_metrics.go` has 1 stub; `bandwidth_reporter.go` has 4 stubs; `formatBytes`, `formatAgeDuration` helpers. No lifetime metrics tests in sample.
- **Why changed:** Lifetime metrics file retained but stub count reduced; metrics may be split between lifetime and bandwidth.
- **Matters:** Yes. If old metrics tests are missing, metrics behavior is unverified.

---

## Stub/TODO Count Comparison

| File (OLD) | Stubs | File (NEW) | Stubs |
|---|---|---|---|
| metrics_listen.go | 11 | lifetime_metrics.go | 1 |
| control_state.go | 7 | resource_pressure.go | 20 |
| proxy_probe_adaptive_test.go | 8 | proxy_reload.go | 12 |
| pending_overrides_lock_unix.go | 2 | proxy_paste.go | 21 |
| jwt_store_lock_windows.go | 3 | sn_rpc.go | 10 |
| pending_overrides.go | 1 | bandwidth_reporter.go | 4 |
| … | … | provider_identity.go | 3 |
| | | systemd_status.go | 1 |
| | | peercred_linux.go | 1 |
| | | hotswap_test.go | 4 |
| | | proxy_state.go | 4 |
| | | jwt_utils.go | 1 |
| | | jwt_store_lock_windows.go | 3 |
| | | pending_overrides.go | 1 |
| | | stubs_provide.go | 1 |
| | | atomic_write.go | 1 |
| | | audit_ring.go | 3 |

NEW has more files with stubs and a higher total stub count in the visible list. This suggests an incomplete migration.

---

## Function Signature Mapping (where possible)

| OLD | NEW | Status |
|---|---|---|
| TestFetchAndMergeProxyURLs… | probeAndGradeProxyURLLines, urlProxyPassesAdmission | Refactored/renamed, tests missing |
| TestRunURLProxyReaperOnce… | writeProxyHealthEvents, rotateIfNeeded | Possibly replaced by event retention |
| TestAcquireProxyLockWithRetry… | (not visible) | Missing |
| TestLifetimeMetrics_* | lifetime_metrics.go (1 stub) | Tests missing |
| TestControlSocket_* | (not visible) | Missing |
| TestValidateJWTExpiry, TestJWTContainsClientId, TestJWTNetworkId | jwt_utils.go (1 stub) | Tests missing |
| TestHotRestartEnabled | hotswap_test.go (4 stubs) | Tests missing |
| tlog, captureTlog | (not visible) | Missing |
| createFakeJWT… | (not visible) | Missing |

---

## Conclusion

NEW is **not** a drop-in replacement. It introduces new architecture (docopt CLI, proxy probing/admission, health state files, SN/Eth RPC, resource pressure) but many old tested behaviors (control socket, lifetime metrics, JWT, proxy reaper/lock) are not visible in the provided sample and have high stub counts in new files. Full behavioral parity cannot be confirmed and is unlikely until stubs are implemented and old tests are ported or replaced.