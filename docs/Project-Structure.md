# Project Structure

`full-bars/sn` is a fork of `urfoundation/sn`, the code for the UR Subnet (Bittensor SN25). It holds two things. The first is the provider you run to carry traffic, plus the tools that manage it (`urnet-tools` and `urnet-docker`). The second is the subnet code: smart contracts, miner and validator software, and chain tools. Most operators only need the first part.

This page lists the top-level folders, grouped by what they are for. The sibling pages [README](https://github.com/full-bars/sn#readme) and [FORK_CHANGES](https://github.com/full-bars/sn/blob/main/FORK_CHANGES.md) explain what the fork adds on top of upstream.

## Provider and operator tools

| Path | What it is |
|---|---|
| `cmd/provider/` | Entry point of the provider program. |
| `cmd/urnet-tools/` | Entry point of `urnet-tools`, the command line tool that starts, stops, updates and checks a provider. |
| `cmd/urnet-docker/` | Entry point of `urnet-docker`, the same kind of tool for providers that run in Docker containers. |
| `cmd/fake-provider/` | A stand-in provider used by tests. |
| `cmd/sp2/` | Entry point inherited from upstream. |
| `provider/` | The provider itself: proxy handling, health checks, hot reload, hot swap, metrics and the control socket. |
| `internal/urnettools/` | The code behind `urnet-tools` and `urnet-docker`. |
| `internal/` (other folders) | Small shared helpers: terminal drawing (`tui`), memory size lookup (`connectx`) and crash-safe file writing (`durable*`). |
| `diagnostics/` | Helpers that explain why something failed. |
| `docker/` | Start scripts and settings for the provider and miner containers. |
| `Dockerfile`, `Dockerfile.miner` | Build the provider image and the miner image. |
| `pelican/` | Egg file for the Pelican panel. |
| `monitoring/` | Prometheus and Grafana bundle for watching a fleet (see [Monitoring](Monitoring.md)). |
| `scripts/` | Installers and uninstallers for Linux, macOS, Windows and FreeBSD, plus build and release check scripts. |
| `deploy/` | A systemd unit for the provider, and the policy files for the test and main networks. |
| `fake-release-server/` | A fake release server used to test the installer. |
| `releases/` | Release notes, one file per release. |

## Subnet code

| Path | What it is |
|---|---|
| `evm/` | The Solidity contracts (coordinator, settlement, reserve sink) and their tests. |
| `stabi/` | Go bindings generated from the contracts. |
| `miner/`, `cli/miner/` | The subnet miner. |
| `validator/`, `cli/validator/` | The subnet validator and its scoring. |
| `cli/` (other folders) | Command line programs: `snclaim` (claim payouts), `payoutroster`, `commitment-oracle` and `trail-probe`. |
| `chain/`, `crv4/`, `evmrpc/`, `ss58/`, `stctl/` | Talking to the Bittensor chain: calls, commit-reveal weights, RPC access, address encoding and a staking helper. |
| `ledger/`, `merkle/`, `payoutartifact/`, `payoutroster/` | Building and checking the per-epoch payout records. |
| `commitmentoracle/` | The commitment oracle for the mainnet. |
| `protocol/` | Shared data types for fleets, claims and evidence. |
| `clientauth/`, `hotkeyauth/`, `hotkeywallet/` | Sign-in and wallet consent with a Bittensor hotkey. |
| `walletsign/` | `provider wallet sign` and `provider wallet status`, shared by the release provider and the miner so every sn build can sign the network wallet consent. |
| `operatorlist/`, `operators.yml` | The list of network operators and how it is refreshed. |
| `nativefee/` | Checks fee data with an independently pinned verifier. |
| `api/`, `price.yml` | The competition and price settings. |
| `mainnet/` | Mainnet launch code and the launch records. |
| `sim-testnet/`, `rehearsal/` | The simulated test network and the rehearsal tests. |
| `tools/`, `third_party/`, `testdata/` | Small tools, vendored code and test data. |
| `seed/`, `iterations/` | Design notes: incentives and verifier ideas, and a proposal for top-level miners. |

## Documents

| Path | What it is |
|---|---|
| `README.md` | Start here: install and run a provider, and the subnet overview. |
| `CHANGELOG.md` | What changed in each release. |
| `FORK_CHANGES.md` | What this fork changes compared with upstream. |
| `WHITEPAPER.md`, `VALIDATOR.md` | The subnet specification and the validator check protocol. |
| `SUBTENSOR-COMPATIBILITY.md` | Notes on Subtensor compatibility. |
| `docs/` | The operator guides, including this page. A workflow copies `docs/*.md` to the GitHub wiki. |
| `docs/design/` | Design records and plans, such as the implementation plan and the comparison with other subnets. |
| `docs/archive/` | Older plans and status notes kept for history, such as the version 2 documents. |
| `docs/spec/` | Formal specifications and JSON schemas. |
| `diagrams/` | Diagrams used by the README. |

## Automation

`.github/workflows/` holds the build, release, test, security scan and wiki sync workflows. See [CI and Release Process](CI-and-Release-Process.md). `.githooks/commit-msg` rejects attribution trailers, so enable it with `git config core.hooksPath .githooks` before you commit here.

## Build and test

```sh
# Build the three operator binaries for this machine
go build -trimpath -o provider_bin ./cmd/provider/
go build -trimpath -o urnet_tools_bin ./cmd/urnet-tools/
go build -trimpath -o urnet_docker_bin ./cmd/urnet-docker/

# Run the provider tests
go test -short -race -timeout 600s ./provider/...

# Static checks
go vet ./provider/...
gofmt -l provider/            # prints nothing when clean
```

Build the container image:

```sh
docker build --build-arg TARGETARCH=amd64 --build-arg VERSION=v<ver> -t ghcr.io/full-bars/sn:latest .
```
