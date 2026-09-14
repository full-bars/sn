# Source and patch identity

`source/source-identity.tsv` records the frozen commit, parent, tree, and subject for the original cumulative source, Astra's standalone probe-successor source, and the final fixture/census source. `source/patch-identity.tsv` records the exact final two-test-file patch and the independent causal patch.

The original 22 selected new roots are intentionally explicit: 21 `TestPrecompileProbeSuccessor*` roots declared in Astra's source census plus the deferred `TestPrecompileBatteryRejectsEveryFailedCustodyCheck` root. They are present in the original 4f59 100-root membership. This set does not contain final511's new `TestPrecompileProbeSuccessorRetainsCanonicalArchivedAmounts`; that is the 22nd successor root and is selected in the final eight-root matrix. The original normal/race outcomes are retained in `outcomes/original-4f59-successor-22-normal-race.tsv`; the constructor root is the one retained failure in both modes.

The net 4f59-to-511 patch is test-only. The final source's two changed test file hashes are in `source/final511-two-test-file.sha256`.
