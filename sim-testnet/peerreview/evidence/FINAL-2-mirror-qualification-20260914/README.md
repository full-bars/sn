# FINAL-2 mirror qualification evidence

This is a compact public-safe handoff bundle for the fixture correction candidate `4029396ef34fdea2338152cf70cd69c054f5f484`. It is evidence for focused qualification only. It does not claim a release lock, native action, live RPC observation, full producer gate, aggregate gate, or campaign result.

## Retained result boundaries

| Record | Terminal result | Scope |
| --- | --- | --- |
| `c50/normal` | 80 PASS, 6 FAIL; body exit 1 | Closed fixture-decoder failure. The failure excerpt preserves unexpected calldata and EOF behavior. |
| `c50/race` | interrupted; exit 143 | Closed after the real c50 normal failure. Its six partial passes are not a qualified race result. |
| `corrected-402/normal` | 87 PASS; body exit 0 | Corrected affected normal matrix. |
| `corrected-402/normal-confirmations` | p2 PASS, p3 PASS | Together with corrected normal p1, three fresh same-binary normal passes for the six c50 failures. |
| `corrected-402/race-original` | timeout; body exit 2 | 85 terminal PASS events, zero terminal FAIL events, and two unresolved roots. It remains a timeout failure and is not renamed a whole-matrix pass. |
| `corrected-402/race-recovery` | p1/P2/P3 PASS | Three fresh retained-binary race processes for only the two roots unresolved by the original timeout. |
| `causal` | 3 expected FAIL, 1 PASS | Disposable two-control causal proof; the outer verification exits cleanly after checking the expected failing roots. |

The original race timeout's `timeout-summary.meta` contains an initial self-matching process observation. `cleanup-check.meta` is the later authoritative direct executable check, recording zero race binaries and zero timeout wrappers. Both are retained so the correction is visible.

## Deterministic root-cause and adjacent coverage

`reports/ASTRA-HANDOFF.md` documents the deterministic direct fake-decoder regression: the serial client sends contract calldata in JSON `input`, while the old fake decoded only `data`. It uses two full synthetic payloads and pinned test blocks, with no sleep, live network, or production capture. The same report records the adjacent decoder/encoder review across sibling test readers and production writers, in accordance with the bug-fix and deterministic-regression requirements in `connect/CODESTYLE.md`.

`causal/` preserves a separate disposable control with exactly two restorations: the historical executor is returned to ff62 behavior, and the fake decoder is returned to `data`. The direct codec root fails on unexpected calldata/empty JSON response, both original-format roots fail on mirror and binding replay-hash mismatches, and the derived-receipt control passes. This separates the fixture issue from the earlier production replay issue without a live RPC call.

`reports/RACE-BUDGET-DIAGNOSIS.md` records why the 87-root race body exhausted its unchanged ten-minute package limit after its long serial prefix. It preserves the original timeout and closes only its two unresolved roots with three fresh processes under unchanged binary, selector, GOMAXPROCS, parallelism, test deadline, and outer deadline.

## Reproduction shape

All executable bodies were run from `sim-testnet` with the retained compiled binary, `-test.v=test2json`, `-test.count=1`, `-test.parallel=4`, and `-test.timeout=10m`. The normal/race 87-root selector and the exact two-root recovery selector are evidenced by the compiled lists in this bundle. Each listed process has pre/post source and dependency identity, binary hash, compiled membership, converter exit, body exit, and terminal outcome files.

The original raw streams and event JSON remain restricted because they are larger incident captures. `MANIFEST.tsv` records their source SHA-256 when a selected excerpt or exact outcome projection was made. `SHA256SUMS` seals every bundled file.
