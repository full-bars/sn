# Peer-review material

This augmentation adds only synthetic test identifiers and pass/fail outcomes. It contains no verbose test event stream, command output, fixture data, private path, binary, cache, state, plan, key, signed receipt material, RLP, or live record.

- `patches/causal-*.patch` contains the two reviewed causal changes used in disposable controls.
- `membership/` contains the exact normal/race and causal compiled root lists, plus the affected confirmation event membership.
- `outcomes/` contains expected and actual per-test tables for the corrected normal/race matrix, normal/race p2/p3 confirmation processes, and both causal controls.

The original retained records and their hashes remain mapped in `references/RETAINED-RAW-RECORDS.tsv`; the copied review material is independently mapped in `MANIFEST.tsv`.
