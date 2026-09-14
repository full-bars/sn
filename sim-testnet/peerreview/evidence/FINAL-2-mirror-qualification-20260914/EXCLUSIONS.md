# Public-safe exclusion policy

This bundle intentionally omits test executables, Go caches and module caches, raw test2json streams, converted event JSON, temporary source checkouts, fixture directories, signed RLP, keys, private plans, native/RPC data, live captures, and all release-lock or chain-operation material.

It retains only hashes and normalized binary labels, source/dependency identities, exact compiled-root lists, terminal outcome TSVs, run/list metadata, and selected path-redacted failure excerpts. The full raw capture remains under restricted retention; `MANIFEST.tsv` maps every bundled evidence file to its retained logical source and original SHA-256.

The two report copies redact only local source/capture root prefixes. The selected excerpts remove the test2json framing control character and redact the same local prefixes. Outcome TSVs are preserved as exact terminal outcome projections and are never rewritten to convert a timeout, cancellation, or intentional causal failure into a pass.
