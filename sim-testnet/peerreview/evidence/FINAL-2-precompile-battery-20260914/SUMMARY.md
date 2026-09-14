# Runtime 455 precompile battery correction

Status: **CLOSED bounded Solidity/artifact qualification**. This is not a combined Go qualification, bootstrap build, release lock, native action, or full release gate.

The initial formatter successor `d7142cc984515d569756a7197ee1588f33a6bddd` established the runtime-455 custody mapping correction and passed all seven new Solidity roots, including three fresh normal executions of the live-failure regression. Its complete Foundry run passed **212 tests across 18 suites**. A real Medium static finding then prevented treating that source as deployable.

The one-line corrected source `bac57484ab290e8f3cc1bb2187a21b99694d7bfe` initializes the caught `selfMapped` local to `false`. It passed three fresh probe static checks, corrected `SP1ProbeTest` 16/16, and a fresh full 85-artifact build. The protected coordinator, vault, reserve, fleet batcher, and validator-evidence creation/runtime bytes are exact in the reviewed comparison. The probe has the intended regenerated payload only.

Generated source successor `2589748336ea52e1654d56b8115d71b108f934b0` is clean and changes only `sim-testnet/contracts_gen.go` (5 additions, 5 deletions). Both causal variants reproduce their old failures as expected-failure controls. The old-probe wrapper refusal is recorded separately from the continuation that ran the previously unstarted zero-self body.

The raw capture remains external. This bundle contains only public-safe result records, source patches, memberships, derived summaries, and hashes.
