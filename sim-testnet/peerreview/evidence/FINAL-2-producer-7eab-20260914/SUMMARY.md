# CLOSED producer gate — 7eab

This is a compact, sanitized record of the failed producer-11355 release-gate capture. It is evidence of that closed run only; it does not reclassify either failure as a pass or substitute for later qualification.

| Field | Recorded value |
| --- | --- |
| Outer result | exit 1 |
| Started | 2026-09-14T15:26:05Z |
| Finished | 2026-09-14T17:30:39Z |
| Duration | 2h 04m 34s |
| Admitted / joined phases | 36 / 36 |
| Phase exits | 34 × 0, 2 × 1 |
| Failed phases | solidity; evidence-simulator |
| Initial and final source freeze | same 13-repository source pair; all recorded dirty checks were 0 |
| Source and release-lock fence | recorded successful in the outer transcript |

The Solidity phase compiled 85 files with Solc 0.8.24 and emitted its sizes table before a single unsafe-typecast lint warning ended the phase. It was a lint failure, not a contract-test result or a contract-size failure.

The evidence-simulator phase completed its normal segment (ok github.com/urfoundation/sn/sim-testnet 321.601s), then its race segment timed out at its unchanged 10-minute limit. The four listed test names were active at timeout; this bundle does not call them assertion failures or passes.

All 36 phase owners were joined by the outer launcher. The retained capture has no distinct post-cleanup or container-custody closure artifact. No live process or container query was made while producing this bundle, so it makes no claim about later service removal or custody.

See failures/ for derived public-safe excerpts, phases/ for exact phase accounting, and references/RAW-LOCATORS.tsv for the full retained raw-record hash/locator index.
