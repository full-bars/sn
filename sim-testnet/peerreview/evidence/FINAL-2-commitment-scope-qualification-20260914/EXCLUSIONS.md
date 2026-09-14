# Public-safe exclusion policy

This bundle omits test executables, Go/module caches, complete raw test2json streams, converted event JSON, temporary source checkouts, fixture directories, signed RLP, keys, private plans, native/RPC data, live captures, release-lock material, and all chain-operation records.

It retains normalized binary hashes, source/dependency identity fences, compiled test lists, exact terminal outcome TSVs, run/list metadata, selected path-redacted causal excerpts, reviewed static analysis, and formatter evidence. Full raw captures remain restricted. `MANIFEST.tsv` maps each retained evidence file to its logical restricted source and original SHA-256.

The two Markdown reviews and formatter identities redact only local source/capture root prefixes. The causal excerpt removes the test2json framing control byte and redacts the same local prefixes. Terminal outcomes are preserved without converting causal failures into passes or treating this focused matrix as a release-gate result.
