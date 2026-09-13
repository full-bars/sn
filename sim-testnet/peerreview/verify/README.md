# sim-testnet peer-review verification

Independent re-derivation of the claims in [`sim-testnet/FINAL.md`](../../FINAL.md) from
Bittensor testnet **netuid 521** / EVM chain **945**, and the generator for
[`sim-testnet/final.html`](../../final.html).

Nothing here trusts the report: every value is re-read from a node or recomputed locally.
The cryptography is implemented in this directory (`keccak.py`, `ecrec.py`, `xxh.py`) rather
than imported from the code under test, and is validated against published test vectors.

## Running

Python 3.9+, standard library only. No install step.

```sh
cd sim-testnet/peerreview/verify
python3 verify_all.py     # stage 1 - node identity, contracts, policy, hyperparams, receipts, vault state
python3 verify2.py        # stage 2 - Merkle/settlement chain, native weights, theta, reserve census
```

Both write to `results.json`. Stage 2 appends to stage 1, so run them in order.

Expected: **59 of 62 checks pass.** The three that do not are findings, not errors —
they are reported in §5 of `final.html` and are expected to keep failing until the
deployment changes:

| Check | Finding |
|---|---|
| `wp-cadence` | WHITEPAPER §5's 360/60/180/6 testnet cadence was never scheduled on chain |
| `hp-maxval` | `max_allowed_validators` is 64, above the §15.1 target of ≤ 56 |
| `reserve-target` | reserve share is 61.449%, above the 60% floor but below the 65% target |

## Endpoint

Defaults to **`https://test.finney.opentensor.ai`** — Opentensor-operated, independent of
the subnet owners, and the endpoint the published findings were confirmed on.

```sh
SN_RPC_URL=https://your-archive-node python3 verify_all.py
SN_RPC_BATCH=16 python3 verify2.py      # lower if an endpoint refuses large batches
SN_RPC_TIMEOUT=90 python3 verify_all.py
```

**An archive node is required.** Several checks read contract state and Substrate storage at
historical blocks (7,986,181 – 7,992,355); a pruned node answers those with
`State already discarded`. The owners' own node (`http://192.168.1.162:9944`) was used for
the first pass and is reachable only from that LAN — it is *not* required, and the results
must be identical on any chain-945 archive node.

`rpc2.py` is a second-endpoint helper (`$SN_RPC_URL_2`) for reproducing the published
two-endpoint comparison in the opposite direction.

## What each file does

| File | Role |
|---|---|
| `rpc.py` | JSON-RPC client; chunked batching; endpoint from `$SN_RPC_URL` |
| `rpc2.py` | second endpoint for cross-checking a result on a different node |
| `keccak.py` | Keccak-256 (original padding, not SHA3-256); validated against known vectors |
| `ecrec.py` | secp256k1 ECDSA public-key recovery and Ethereum address derivation |
| `xxh.py` | xxHash64 / `twox128` for building Substrate storage keys |
| `decode.py` | event-topic table and log decoder, built from `evm/abi/*.json` |
| `txcheck.py` | the 41 transactions and 16 claims cited by FINAL.md |
| `verify_all.py` | stage 1 |
| `verify2.py` | stage 2 |
| `events.py` | indexes the full contract log history into `events.json` |
| `content.py` | per-actor whitepaper conformance rows |
| `gen_css.py`, `gen_fig.py`, `build.py` | render `sim-testnet/final.html` |

## Rebuilding the report

```sh
python3 events.py     # refresh events.json from chain (optional)
python3 build.py      # writes sim-testnet/final.html
```

## What this cannot verify

Chain reads cannot establish the truth of off-chain facts. The usage figures inside the
payout artifacts, the release-gate outcomes, supervisor restart counts and service
lifecycle all rest on the run's own receipts and are marked *asserted* or *artifact only*
in the report rather than verified. See §8.2 of `final.html`.
