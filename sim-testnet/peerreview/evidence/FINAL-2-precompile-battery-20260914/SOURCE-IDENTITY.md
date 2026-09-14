# Source identity

| Role | Commit |
| --- | --- |
| Baseline | `afd7b26c9c1b2847e8f73648e6a6eac13928453e` |
| Astra frozen correction | `a2249647f3a4b2a9a479e78a274aaa2a27ac216a` |
| Formatter-only successor | `d7142cc984515d569756a7197ee1588f33a6bddd` |
| Static initialization correction | `bac57484ab290e8f3cc1bb2187a21b99694d7bfe` |
| Generated source successor | `2589748336ea52e1654d56b8115d71b108f934b0` |

`d7142cc` is a clean formatter successor. `bac5748` changes only `STSubnetProbe.sol`, initializing the previously uninitialized caught-local boolean to `false`. `2589748` is a clean generated successor over `bac5748` and changes only `sim-testnet/contracts_gen.go`.

The complete correction, static correction, and generated successor are retained as exact patches. No source checkout is copied.
