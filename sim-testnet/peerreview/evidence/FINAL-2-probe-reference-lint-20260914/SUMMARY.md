# Probe reference lint qualification

The original producer Solidity phase stopped after compiling 85 files because strict lint rejected `bytes32(referenceOutput)`. No original Solidity test body ran. The correction changes only `evm/test/SP1Probe.t.sol`: it validates the EIP-152 response as exactly two words, decodes both words, and compares the first word to the unchanged known answer.

The corrected strict p1 build passed under the same `--deny warnings --sizes` policy. One full project `forge test --summary` then closed with **215 PASS, 0 FAIL, 0 skipped across 18 suites**, including all 19 `SP1ProbeTest` roots. The 19-name source census exactly matched the compiled list.

The two credited fresh direct lint confirmations used `--no-cache` and each compiled the 30-file target closure. The first cached direct-lint observation is retained but explicitly uncredited. The isolated causal restored only the old assertion and produced the required `unsafe-typecast` refusal; it is expected causal evidence, not a candidate failure.

The generated-contract check and ABI/binding freshness check both passed against the strict p1 artifact. External artifact-sidecar and isolated filesystem-mode admissions are retained separately. The final mode reconciliation changed neither bytes nor git modes and left the candidate clean.
