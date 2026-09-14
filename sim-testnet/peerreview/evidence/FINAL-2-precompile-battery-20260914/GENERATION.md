# Generated source and ABI boundary

`stabi/generate.sh --write` and its artifact check both exited 0. `go run ./sim-testnet/gencontracts` write and check both exited 0. The generated successor is `2589748`, clean, with only `sim-testnet/contracts_gen.go` changed (5 additions, 5 deletions).

The captured generator review confirms the SubnetProbe ABI declaration is byte-identical. Only probe creation/runtime payload, generated hashes, and immutable offsets changed. The complete generated diff is retained in `patches/generated-contracts.patch`; this bundle intentionally does not copy source checkout or Forge artifact files.
