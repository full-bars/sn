# Cumulative precompile-successor qualification evidence

This external, public-safe bundle preserves the closed Go qualification trail from the cumulative `4f59d3f30f59ba612c67b266e0d1310b9c155ed3` source through the test-only archived-fixture and runtime455 census correction at `511a09e8c9fad91be09c49ad31327a05c6648568`.

| Closed scope | Result | Evidence retained here |
| --- | --- | --- |
| Original 4f59 normal matrix | Candidate failure: 99 of 100 top-level roots passed; `TestPrecompileProbeSuccessorConstructsOriginalNativeReplay` failed; 29 descendant passes | Exact 100-root membership, expected/actual outcomes, descendant table, result, raw-record hashes |
| Original 4f59 race matrix | Candidate failure: the same 99/1 top-level result and 29 descendant passes | Exact 100-root membership, expected/actual outcomes, descendant table, result, raw-record hashes |
| Original runtime455 pair | Candidate failure: both stale-census roots failed | Exact two-root membership, expected/actual outcomes, result, raw-record hashes |
| fdff archive-wire diagnostic | Closed diagnostic failure: `wire_equal=true` isolated canonical zero representation after persisted decoding | One-root result and observed diagnostic literal; raw event is hash-referenced only |
| Final 511 normal and race matrices | Each closed PASS: 8 exact roots | Compiled memberships, expected/actual outcomes, source/binary/result fences |
| Final 511 confirmations | Closed PASS: normal p1/p2/p3 and constructor race p1/p2/p3 | Exact narrow memberships, expected/actual outcome tables, sequence result |
| 4f59 causal control | Closed expected control: 2 failures and 2 passes | Authorized patch, compiled membership, expected/actual outcomes, closure result |

The original selected set is 21 successor roots plus one battery root; it does not contain final511's `TestPrecompileProbeSuccessorRetainsCanonicalArchivedAmounts`. That test is the 22nd successor root and is selected in the final eight-root matrix.

The final patch changes only `sim-testnet/precompile_probe_successor_test.go` and `sim-testnet/release_gate_test.go`. It preserves production and generated-artifact bytes; this bundle records the bounded test qualification only.

## Scope limit

The original 4f59 100-root bodies remain failures on their own source. This bundle does not claim that all 100 roots passed on 511, does not claim a combined-successor qualification beyond the explicit 8-root final matrix, and does not claim a CLI build, native action, RPC action, publication, producer gate, or aggregate gate.

`CASE-TO-FAILURE-MAPPING.md` connects each retained failure to its deterministic diagnostic, corrected regression, or expected causal control.
