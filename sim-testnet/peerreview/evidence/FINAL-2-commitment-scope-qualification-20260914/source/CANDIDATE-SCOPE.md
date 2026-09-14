# Candidate source and formatting scope

- Qualification candidate: `6271dcfceb1af8257b626aba79a6864eb3b7c986`.
- Parent production revision: `abae9a6a8410a54a560335b16888d2ad0d4fc51f`.
- Production files reviewed and qualified:
  - `sim-testnet/executor.go` SHA-256 `dafd0d953e65d91186712a54524630b3e2f0997390dfb9af12ee88f133458a68`
  - `sim-testnet/fleet_commitment_recovery.go` SHA-256 `80423ceabcb443a6a461f191fcea400e1cce643d347f1d7fd4ba1914705d0e3b`
  - `sim-testnet/fleet_renewal_history.go` SHA-256 `a1f9fbb4114542c022ac66f3bf4b60f1764360216858c71c21b57732bb11db71`
- Regression file: `sim-testnet/fleet_commitment_history_test.go` SHA-256 `a885196dfde1ab8c6b75edfa1c504c87bca5ef2de62e89478d28ff41161e5354`.

Terra's first format pass changed only four table-literal wraps in the regression file. Astra committed that exact formatter-only reconciliation as `6271dcf`; the qualification source was then clean. The formatter evidence is retained in `formatter/`.
