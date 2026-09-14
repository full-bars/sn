# FINAL-2 afd7 full gates — canceled evidence

Status: **CLOSED — source-superseded / CANCELED**. This bundle is not a
release-gate pass and does not certify a later source revision.

The two incomplete gates ran on clean published SN
`afd7b26c9c1b2847e8f73648e6a6eac13928453e`. A native pre-transfer
precondition exposed a production defect requiring a source correction, so the
root directed orderly cancellation at `2026-09-14T11:43:53Z`. Neither gate had
an original test failure before that cancellation.

The exact 13-checkout source record is
[source-pair-afd7b26.tsv](source-pair-afd7b26.tsv), SHA-256
`47b08d8d42383a9b2a4d3def6f21ba22b4fef6a8d431de54e13d158e7e751c2e`.
All recorded checkouts are clean.

Producer outer capture PID `2484223` ran from `11:13:17Z` to `11:43:53Z` and
recorded exit `143`. Its 13 admitted children all joined: 10 joined exit 0
(`compile`, `evidence-native`, `runtime`, `seed`, `evm-identity`, `driver`,
`framing`, `sim-seed`, `isolation-regressions`, and `adversarial`) and three
joined exit 143 from directed cancellation (`semantic`, `settlement`, and
`semantic-public-scenario`).

Aggregate outer capture PID `2490766` ran from `11:15:29Z` to `11:43:53Z` and
recorded exit `143`. Its five admitted children all joined: three joined exit
0 (`isolation-regressions`, `sn-go`, and `sn-core-race`) and two joined exit
143 from directed cancellation (`sn-validator-race` and `sn-all-normal`).

Both gates completed source-freeze, source-integrity, binding-toolchain,
runtime-source, and runtime-metadata preflights with exit 0. The complete
started/joined records are in [records](records); the post-closure process
census records no remaining matching gate, test, or service owner.

`raw` holds only the two public-safe gate commands, capture wrappers, and raw
outer stdout, stderr, exit, PID, and timestamps. `preflights` holds the
retained preflight outcomes. `MANIFEST.tsv` maps every copied file to its
original path, SHA-256, and byte count, while `SHA256SUMS` authenticates the
bundle itself.

This bundle intentionally excludes binaries, module caches, private runtime
state, child phase logs, private configuration, plans, credentials, and
secrets. It is staged externally for
`sim-testnet/peerreview/evidence/FINAL-2-afd7-gates-canceled-20260914`; it has
not been copied into a source repository.
