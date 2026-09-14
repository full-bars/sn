# Census and compiled-test enumeration corrections

The a592 producer and aggregate gates both executed the same stale census failure and were then canceled with all admitted children joined. Neither full gate passed. The closed raw failure logs, initial source bindings and cancellation records are retained here.

The 43cc correction replaces open-family fixed counts with the actual source census while retaining exact single ownership, fixed finite cohorts and selector guards. Adjacent review found the same stale-count issue in the evidence family. It also replaces live-looking oracle fixtures and positive subtests with synthetic inputs and plain cases.

Focused 43cc normal: 20/21 passed, with a separate 120-second nested Go-tool enumeration timeout. Focused race: 21/21 passed. The original census root completed three fresh normal passes on the same source and binary. Disposable stale-cardinality and evidence-only-87 controls preserve expected failures for source growth while their baseline controls pass.

The ff62 correction enumerates the already compiled binary with -test.list, preserving compiled/source census comparison, required and forbidden families, cancellation, empty-selector rejection and proof that no test bodies execute. Searching adjacent call sites found no other executing nested Go test enumeration. Reusing the old 43cc binary with an empty PATH reproduces the old failure immediately, without a timeout-based test.

Focused ff62 normal: exactly 24/24 passed, 09:09:25–09:09:34 UTC. Focused race: exactly 24/24 passed, 09:13:25–09:16:06 UTC. The enumeration root completed three fresh normal passes on one source/binary. Both compiled censuses and all execution, converter, source and binary fences passed. The initial race shell syntax refusal executed no test body and is retained separately; the corrected owned run is the qualifying result.

These are scoped correction and causal results, not a final whole-source release pass. The later mirror-replay production correction requires its own affected qualification and final-source gates. All tests use Terra max; Astra max owns diagnosis and regression implementation under Connect CODESTYLE. The complete adjacent-path notes are in the retained handoffs.

Binaries, caches, build scratch directories, private plans and live runtime state are excluded. Raw selected evidence files are copied byte-for-byte and indexed in STAGING.json; SHA256SUMS covers the complete portable bundle.
