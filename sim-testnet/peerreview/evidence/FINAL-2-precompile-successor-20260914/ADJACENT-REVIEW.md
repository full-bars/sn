# Deterministic root cause and adjacent coverage

The retained fdff diagnostic shows that the archived approval's wire representation was unchanged (`wire_equal=true`) while the in-memory zero amount used `DecimalUint("")` and the persisted archive decoded canonical `DecimalUint("0")`. The final fixture reloads the persisted approval before deriving the successor and adds an amount control that rejects changed signed-repair or native-action amounts.

The original runtime455 pair exposed stale 31-file test expectations while the reviewed checker and manifest already used 33. The final patch centralizes the 33-file expectation, restores exact path/digest census validation, and adds a synthetic drift regression covering omission, duplicate, substitution, malformed, and empty rows.

The independent 4f59 causal patch restores the original CREATE-only successor assumptions. Its four-root control closes with the two named expected failures and two unrelated controls passing. It is a disposable patched source and is not a candidate result.

The original 22 selected new roots (21 successor + one battery) and the final eight-root matrix provide adjacent coverage without relabeling the original 4f59 failure as a pass. Final511 adds `TestPrecompileProbeSuccessorRetainsCanonicalArchivedAmounts` as the 22nd successor root, selected only in that final matrix.
