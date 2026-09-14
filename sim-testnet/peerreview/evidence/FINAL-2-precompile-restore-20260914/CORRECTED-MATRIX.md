# Corrected focused matrix

The fresh package-CWD verbose execution used source `52def6334e72f77a0b2e3b655a37ec2d0df8d3fc`, the exact selector below, `GOMAXPROCS=4`, `-test.parallel=4`, `-test.count=1`, and `-test.timeout=10m`.

```text
^Test(Precompile|IndependentBattery|HistoricalPrecompileProbe|FleetCommitmentHistory|FleetRenewalHistorical|FleetRenewalOriginalOracle|FleetRefreshOracle|FleetRefreshRestore|NativeHistoryCache|CheckpointVisibility|EVMCheckpoint|IndependentReadExecutor)
```

| Mode | Binary SHA-256 | Result |
| --- | --- | --- |
| normal | `3f9e4f49a2c59593a0e73ecb5f20299678385d4c4fee25a745531dc49560f05b` | exit 0; 44 compiled top-level roots; 48 terminal pass events; 4 explicit descendant passes; no unexpected failures |
| race | `258268e06724e0bf4641c163f86058c42bc53bb3be83e84a36adb629aaf6d89d` | exit 0; 44 compiled top-level roots; 48 terminal pass events; 4 explicit descendant passes; no unexpected failures |

- Normal interval: `2026-09-14T12:19:52Z` to `2026-09-14T12:19:59Z`; converter stderr was empty and pre/post binary identity matched.
- Race interval: `2026-09-14T12:19:52Z` to `2026-09-14T12:20:41Z`; converter stderr was empty and pre/post binary identity matched.

The compiled-list census remains 44 top-level roots. Four deliberate cache subtests add four required event identities, so the complete event census is 48. The package source, frozen dependency projection, compiled list, binary identity, and event census were fenced before and after each body.
