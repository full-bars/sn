# Bittensor Subnet Operations

Operating guide for the subnet (Bittensor) command surface of the provider
binary. `provider/sn.go` implements the Subnet pool bridge (`sn/PLAN.md` 7.3)
backed by `sn/merkle` (inclusion proofs), `sn/stabi` (ABI packing),
`sn/miner/onchain` (go-ethereum submission) and `sn_rpc.go` (stdlib `eth_call`).

## Identity & wallet model

- **Network account token**: the provider authenticates to the control plane
  with a signed JWT at `~/.urnetwork/jwt` (state paths honor the
  `URNETWORK_STATE_DIR` environment variable).
- **Claim coldkey (`ss58`)**: the Bittensor coldkey address (SS58, prefix `42`)
  registered with the platform to receive pool emissions.
- **Mirror account**: EVM smart contract accounts mirrored on Subtensor via
  `evm:<H160 address>` hash derivation for non-custodial claims.

## Commands (provider binary)

### Register / update the claim wallet

```bash
provider wallet set 5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5

# startup flag form:
provider provide --wallet=5FjfHgd4K3H5Vge2igPtBYyWRbRKdgH84roTCnWwwtNgAhU5
```

Registers the coldkey with the platform via the authenticated `POST /sn/wallet`
route (`walletSet` in `provider/sn.go`), printing the derived pubkey on
success. The wallet is only a payment destination — no transaction is signed
or broadcast when linking it.

### Real-time status & telemetry

```bash
provider sn-status            # global rank, bandwidth, coldkey, epoch, payout share
provider sn-status --json     # monitoring / Prometheus-friendly output
```

### Epoch payout claim

```bash
# offline / air-gapped (recommended): prints ready-to-submit calldata for snclaim
provider claim --epoch=1053

# direct on-chain submission with a local key file and RPC endpoint:
provider claim --epoch=1053 --rpc=https://rpc.subtensor.network \
  --key_file=/path/to/coldkey_evm.key

# simulate verification and root matching without broadcasting:
provider claim --epoch=1053 --rpc=https://rpc.subtensor.network --dry-run
```

`claim` fetches the network's pool payout claim for the epoch, recomputes the
Merkle leaf and checks the inclusion proof with `sn/merkle`, cross-checks the
payout root on-chain via `eth_call` when `--rpc` is given, and builds the
`claimMiner` calldata with the `sn/stabi` packer. With `--key_file` it signs
and submits the transaction through `sn/miner/onchain`; without one it prints
the calldata so credentials never touch the provider host.

### Head miner delegation

```bash
provider bind-head --hotkey=0x1234567890abcdef... \
  --registrant=0xYourEvmAddress... --contract=0xContractAddress...
provider unbind-head --hotkey=0x1234567890abcdef... [--contract=0xContractAddress...]
```

> [!IMPORTANT]
> The `--registrant` address MUST equal the EVM transaction sender. The
> head-bind digest binds cryptographically to this sender, and the transaction
> reverts if the sender's Subtensor mirror does not match the hotkey's on-chain
> coldkey.

## Operational security

- Never pass tokens via CLI flags (`-jwt=...`, `-pass=...`) — they are visible
  in `ps aux` / `/proc/<pid>/cmdline`.
- Keep `~/.urnetwork/jwt` and private key files readable only by the service
  account (`0600`).
- For claiming, prefer the air-gapped path: `provider claim` without
  `--key_file` emits calldata and never touches a private key on the provider
  host.