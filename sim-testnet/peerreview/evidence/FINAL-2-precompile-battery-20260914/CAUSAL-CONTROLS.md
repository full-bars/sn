# Deterministic causal controls

1. `patches/causal-old-library-and-probe.patch` restores only old `Blake2b.sol` and `STSubnetProbe.sol` on the corrected base. The selected body exited nonzero with exact expected identity `Blake2b: blake2f failed`; this demonstrates the old 0x09 route failure.
2. `patches/causal-old-probe.patch` restores only old probe source on the corrected library. Its first selected body produced exact expected `Blake2b: address mapping failed`. The old zero-self behavior then ran in a separately retained continuation and failed the required assertion after querying zero custody.

The original sequential wrapper re-enabled `set -e` after the first expected nonzero and stopped before launching the zero-self body. `results/causal-old-probe-wrapper-refusal.RESULT` records that operational refusal. It is neither a candidate failure nor evidence that the second body ran; the separate continuation is the evidence for the second outcome.

Both controls are expected-failure controls, never candidate failures.
