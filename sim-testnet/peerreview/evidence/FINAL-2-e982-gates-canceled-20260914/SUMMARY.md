# FINAL-2 e982 full gates — canceled evidence

Status: **CLOSED — source-superseded / CANCELED**. This bundle is not a
release-gate pass and does not certify a later source revision.

The two incomplete gates ran on clean published SN
`e982b3fbd74f76c8afe79cd8ef7067b19b044238`. A native plan mismatch
established a production defect requiring a source correction, so the root
directed orderly cancellation at `2026-09-14T14:42:31Z`. Neither gate had
an original test failure before that cancellation.

The exact 13-checkout source record is
[source-pair-e982b3f.tsv](source-pair-e982b3f.tsv), SHA-256
`519744ac4ad704e69a8f7e5500f4dddfbb479ca9a8021cea6b66c60d6000ade7`. All recorded checkouts are clean.

Producer outer capture PID `2841851` ran from
`2026-09-14T14:21:59Z` to
`2026-09-14T14:42:32Z` and recorded exit `143`.
Its 13 admitted children all joined: 10 joined exit 0
(`compile`, `evidence-native`, `runtime`, `seed`, `evm-identity`, `driver`,
`framing`, `sim-seed`, `isolation-regressions`, and `adversarial`) and
3 joined exit 143 from directed cancellation (`settlement`, `semantic`,
and `semantic-public-scenario`).

Aggregate outer capture PID `2844375` ran from
`2026-09-14T14:23:00Z` to
`2026-09-14T14:42:33Z` and recorded exit `143`.
Its 5 admitted children all joined: 3 joined exit 0
(`isolation-regressions`, `sn-go`, and `sn-core-race`) and 2 joined
exit 143 from directed cancellation (`sn-validator-race` and `sn-all-normal`).

Both gates completed source-freeze, source-integrity, binding-toolchain,
runtime-source, and runtime-metadata preflights with exit 0. The complete
started/joined records are in [records](records); the post-closure process
census records no remaining matching gate, test, or service owner.

`raw` holds only the public-safe gate commands, capture wrappers, cancellation
requests, and raw outer stdout, stderr, exit, PID, and timestamps. `preflights`
holds the retained preflight outcomes. `MANIFEST.tsv` maps every copied or
derived evidence record to its original path or derivation, SHA-256, and byte
count, while `SHA256SUMS` authenticates the bundle itself.

This bundle intentionally excludes binaries, module caches, private runtime
state, child phase logs, private configuration, native plans, credentials, and
secrets. It is staged externally for
`sim-testnet/peerreview/evidence/FINAL-2-e982-gates-canceled-20260914`; it
has not been copied into a source repository.
