# Root cause and runtime provenance

The retained live failure was `Blake2b: blake2f failed`. Runtime 455 pinned source `67dcf7f791dc495064c293f080a0702cb433e51e` maps `0x09` to BN128 addition, so it cannot service Ethereum EIP-152 Blake2f input. The corrected custody route calls runtime address-mapping precompile `0x080c`, selector `addressMapping(address)` / `0x0494cd9a`, and requires a successful exact 32-byte response.

The mapping is checked against the retained BlakeTwo256 `"evm:" || H160` known answer. Local `hash256` remains an Ethereum reference for Foundry-only calculations; there is no runtime `0x09` fallback. A failed or zero self mapping leaves custody unstamped and skips the dependent stake read while preserving other precompile diagnostics.

The offline runtime455 source-manifest check matched **33/33** immutable pinned source objects, including dispatcher and address-mapping implementation. `membership/runtime455-source-manifest.tsv` is the copied public-safe row set.

This provenance is diagnostic evidence only. It does not claim a live probe deployment, native write, repair, or release acceptance decision.
