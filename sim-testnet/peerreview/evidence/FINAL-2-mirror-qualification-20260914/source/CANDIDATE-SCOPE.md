# Candidate scope and formatting

- Corrected fixture candidate: `4029396ef34fdea2338152cf70cd69c054f5f484`.
- Parent with retained c50 failure: `c50dbd95649c64887adf372b84441eddfe1d2bbd`.
- Formatter scope: `sim-testnet/fleet_renewal_history_test.go` only.
- Formatter result: no delta. Pre- and post-format SHA-256 are both `9ef5407bbbb72cf82e500ab36f6a1760da3eacd16802587981e10354e82c348e`.
- Candidate production-source digest: `sha256:6215cfe3b055ef3edc453bc688f6abafe4397f8315631883593d7a1ac1131a98`.
- The fixture correction is test-only. `executor.go` remains the reviewed production byte set with SHA-256 `c45f58ccc638ab36f3401bd96cb00d651807466a9db9d25120875c7e15a9f23f`.

The source identities retained with each execution record the candidate head and the twelve pinned dependency heads/index hashes. No workspace, release lock, chain state, or live-node input is included here.
