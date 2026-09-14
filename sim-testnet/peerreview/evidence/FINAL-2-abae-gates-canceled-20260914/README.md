# FINAL-2 abae gates — canceled evidence

Status: **CLOSED — source-superseded / CANCELED**. This is not a release-gate pass.

This compact bundle preserves the two incomplete full-gate invocations on the
published `abae9a6a8410a54a560335b16888d2ad0d4fc51f` source after the root
directed orderly cancellation at `2026-09-14T10:35:29Z`. A production source
correction was required, so these invocations cannot certify a successor.

The exact 13-checkout source record is [source-pair-abae9a6.tsv](source-pair-abae9a6.tsv),
whose SHA-256 is
`f7871bf5e6b53a939b8ea8add2b2b3abbbfd0bf58f9c1bdc3474fd42248b0089`.
Every recorded checkout is clean; the SN row is `abae9a6a8410a54a560335b16888d2ad0d4fc51f`.

Producer capture: outer PID `2358555`, `10:20:36Z`–`10:35:29Z`, outer exit
`143`. All 13 admitted child owners joined: 10 had already joined exit 0 and
three were terminated by the directed cancellation (`settlement`, `semantic`,
and `semantic-public-scenario`, each exit 143).

Aggregate capture: outer PID `2363560`, `10:23:35Z`–`10:35:29Z`, outer exit
`143`. All three admitted child owners joined: `isolation-regressions` joined
exit 0; `sn-go` and `sn-all-normal` joined exit 143 from cancellation.

Both invocations completed all five initial preflights with exit 0:
source-freeze, source-integrity, binding-toolchain, runtime-source, and
runtime-metadata. There was no original nonzero phase outcome before the
directed cancellation. The complete started/joined records and a later
no-live-owner census are in [records](records).

The `raw` directory contains the capture wrapper and gate command plus the
raw outer stdout, stderr, exit, PID, and timestamps. `preflights` contains
their retained output and exit records. `ORIGINAL-MANIFEST.tsv` maps every
copied raw file to its original path, source SHA-256, and byte size;
`SHA256SUMS` authenticates the bundle files.

This bundle intentionally excludes child phase logs, duplicate full source
inventories beyond the required exact pair, binaries, module caches, and live
runtime state. It is staged for
`sim-testnet/peerreview/evidence/FINAL-2-abae-gates-canceled-20260914` only;
it has not been copied into a source repository.
