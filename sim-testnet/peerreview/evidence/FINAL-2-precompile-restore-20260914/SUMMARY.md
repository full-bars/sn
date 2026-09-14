# Restore 52def qualification evidence

This external peer-review bundle covers the bounded historical precompile-restore correction from `afd7b26c9c1b2847e8f73648e6a6eac13928453e` through clean formatter successor `52def6334e72f77a0b2e3b655a37ec2d0df8d3fc`. It records source/patch identity, retained command/evidence refusals, the corrected normal/race matrix, the closure of the affected confirmation obligation, two deterministic causal controls, and a reference to the original closed native refusal.

| Evidence | Result |
| --- | --- |
| Source | `52def6334e72f77a0b2e3b655a37ec2d0df8d3fc`; clean formatter-only successor over `3ff93ee57691138786ef55975a1da69190a574bd` |
| Corrected matrix | normal and race each: 44 compiled top-level roots, 48 terminal pass events, exit 0 |
| Confirmation sequence | p1 full matrix + p2/p3 exact 24-parent / 28-event selections in each mode: PASS |
| Causal control 1 | 3 expected failures / 2 passes |
| Causal control 2 | 1 expected dispatch failure / 2 passes |
| Native refusal reference | closed exit 1; retained separately and never represented as success |

The first source-census attempt, first list validation, repository-root body attempts, and package-pass/no-root-event bodies are retained as command/evidence records only. They are not qualifying passes or source failures.

## Peer-review material

The bundle also includes the two reviewed causal patch files, exact compiled root lists, affected confirmation membership, and expected/actual synthetic per-test outcome tables. These are curated review artifacts, not raw verbose event streams or command output.

## Proof limits

No binary, cache, raw event stream, stdout/stderr, private state, configuration, key, signed receipt material, or RLP is copied here. The bundle does not claim a native repair, submission, probe conformance, combined-successor qualification, or final-gate result.
