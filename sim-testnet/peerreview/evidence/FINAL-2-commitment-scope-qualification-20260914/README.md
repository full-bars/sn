# FINAL-2 commitment-scope qualification evidence

This public-safe bundle records focused qualification for source `6271dcfceb1af8257b626aba79a6864eb3b7c986`. It does not claim a release lock, native repair, live RPC result, final producer gate, aggregate gate, or campaign result.

## Execution result

| Scope | Result | Boundary |
| --- | --- | --- |
| Normal A33 | 33 PASS | Commitment, refresh, V10 commitment, and lifecycle-renewal roots. |
| Normal B69 | 69 PASS | Retained renewal, generation-one, historical, carried, install, EVM/checkpoint, and independent-reader roots. |
| Race A33 | 33 PASS | Same exact A compiled list on the retained race binary. |
| Race B69 | 69 PASS | Same exact B compiled list on the retained race binary. |
| Constructor root confirmations | p1 + p2 + p3 PASS | `TestFleetCommitmentHistoryScopeRequiresCompletedRenewal` on one normal binary/source snapshot. |
| Causal control | 2 expected FAIL, 2 PASS | Separate disposable restoration of the old consumption predicate. |

`inputs/roots-union.expected.sorted` is the exact 102-root disjoint union. It is formed from 33 A roots and 69 B roots; `inputs/roots-overlap.sorted` is intentionally empty. The normal and race captures retain list, source/dependency, binary, converter, outcome, and post-run fences for every shard.

## Deterministic root cause and adjacent review

`reports/ASTRA-HANDOFF.md` and `reports/ROOT-REVIEW.md` describe the historical-consumer scope defect: an original source plan could not see a later approved descendant where the exact consumer had completed. The new constructor-to-consumer regression exercises that route with synthetic records and no sleep or external node.

`causal/` restores only the old source-plan consumer predicate in a disposable checkout. It produces the expected failures for the descendant consumer and completed renewal while preserving the adjacent-action-family and exact-generation controls. The selected excerpt preserves the exact result literals; the complete raw stream remains restricted.

`reports/adjacent-action-review-1466.json` is the retained static identity/order review of 1,466 historical actions. `reports/generation-consumer-review.json` retains the 802-generation dependency review. Those records are static analysis and do not claim a full historical chain replay. The handoff records the sibling-call-site and family review required by `connect/CODESTYLE.md`.

## Integrity and limits

The candidate was formatter-reconciled before execution. `formatter/` preserves the only formatting delta and the before/after source hashes. This matrix is intentionally disjoint because the unchanged per-process ten-minute deadline applies to each body; no root was dropped and no timeout was increased. `SHA256SUMS` seals the bundle, and `MANIFEST.tsv` maps each copied or derived artifact to the restricted original and its hash.
