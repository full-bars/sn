# Project Structure

`full-bars/sn` is a fork of `urfoundation/sn`, the code for the UR Subnet (Bittensor SN25). It builds one provider program, `./cli/miner`, which is also the subnet miner. It also holds the tools that manage that program (`urnet-tools` and `urnet-docker`) and the subnet code: smart contracts, validator software and chain tools. Most operators only need the first part.

This page lists the top-level folders, grouped by what they are for. The sibling pages [README](https://github.com/full-bars/sn#readme) and [FORK_CHANGES](https://github.com/full-bars/sn/blob/main/FORK_CHANGES.md) explain what the fork adds on top of upstream.

## Provider and operator tools

| Path | What it is |
|---|---|
| `cli/miner/` | Entry point of the provider program (`main.go`). The Docker image and the release archives ship this binary as `provider`. |
| `miner/` | The library behind `cli/miner/`: proxy handling, health checks, hot reload, hot swap, metrics and the control socket. It also holds the subnet miner code. |
| `miner/state_schema.go`, `miner/state_settings.go`, `miner/state_startup.go` | State-store schema guard, private settings migration and startup degradation reporting. |
| `cmd/urnet-tools/` | Entry point of `urnet-tools`, the command line tool that starts, stops, updates and checks a provider. |
| `cmd/urnet-docker/` | Entry point of `urnet-docker`, the same kind of tool for providers that run in Docker containers. |
| `cmd/fake-provider/` | A stand-in provider used by the CI lifecycle tests. |
| `cmd/sp2/` | The SP-2 CRv4 conformance harness: commit-reveal v4 weight checks against a live subtensor chain, plus offline payload tooling. |
| `internal/urnettools/` | The code behind `urnet-tools` and `urnet-docker`. |
| `internal/` (other folders) | Small shared helpers: terminal drawing (`tui`), memory size lookup (`connectx`), crash-safe file writing and paths (`durable*`), and the Windows service and custody code (`winservice`, `wincustody`). |
| `diagnostics/` | Helpers that explain why something failed. |
| `docker/` | Scripts for the container image. `docker/miner/` holds the entrypoint and start scripts that the `Dockerfile` copies in. `docker/scripts/` holds the update, proxy and log helpers, the stable, nightly and JWT start scripts, and their tests. |
| `Dockerfile` | Builds the container image from `cli/miner/`, plus `urnet-tools` and `urnet-docker`. It is published as `:latest`. The `:miner` tag is an alias kept for one release (see [Miner Image](/full-bars/sn/wiki/Miner-Image)). |
| `pelican/` | Egg file for the Pelican panel. |
| `monitoring/` | Prometheus and Grafana bundle for watching a fleet (see [Monitoring](/full-bars/sn/wiki/Monitoring)). |
| `scripts/` | Installers and uninstallers for Linux, macOS, Windows and FreeBSD, plus build and release check scripts. |
| `deploy/` | A systemd unit for the provider, and the policy files for the test and main networks. |
| `fake-release-server/` | A built binary of the fake release server that the installer tests use. Its source is in `scripts/ci/fake-release-server/`. |
| `releases/` | Release notes, one file per release. |

> [!NOTE]
> `cmd/provider/` and the `provider/` package were removed in commit `d79c04bf2`, which archived the relay. The provider entry point is now `cli/miner/` and its library is `miner/`. `Dockerfile.miner` was also removed, in commit `0d8c4604c`. `Dockerfile` is now the only image build. If a script or document still builds `./cmd/provider/` or tests `./provider/...`, switch it to the commands under [Build and test](#build-and-test).

## Subnet code

The subnet miner runs from the same code as the provider, so `miner/` and `cli/miner/` are listed above.

| Path | What it is |
|---|---|
| `evm/` | The Solidity contracts (coordinator, settlement, reserve sink) and their tests. |
| `stabi/` | Go bindings generated from the contracts. |
| `validator/`, `cli/validator/` | The subnet validator and its scoring. |
| `cli/` (other folders) | Command line programs: `snclaim` (claim payouts), `payoutroster`, `commitment-oracle` and `trail-probe`. |
| `chain/`, `crv4/`, `evmrpc/`, `ss58/`, `stctl/` | Talking to the Bittensor chain: calls, commit-reveal weights, RPC access, address encoding and a staking helper. |
| `ledger/`, `merkle/`, `payoutartifact/`, `payoutroster/` | Building and checking the per-epoch payout records. |
| `commitmentoracle/` | The commitment oracle for the mainnet. |
| `protocol/` | Shared data types for fleets, claims and evidence. |
| `clientauth/`, `hotkeyauth/`, `hotkeywallet/` | Sign-in and wallet consent with a Bittensor hotkey. |
| `walletsign/` | The local page behind `provider wallet sign` and the status check behind `provider wallet status`. The miner (`miner/wallet_sign_cmd.go`) uses it to sign the network wallet consent. |
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

`.github/workflows/` holds the build, release, test, security scan and wiki sync workflows. See [CI and Release Process](/full-bars/sn/wiki/CI-and-Release-Process). `.githooks/commit-msg` rejects attribution trailers, so enable it with `git config core.hooksPath .githooks` before you commit here.

## Build and test

Build the provider and the two tools for this machine:

```sh
go build -trimpath -o provider_bin ./cli/miner/
go build -trimpath -o urnet_tools_bin ./cmd/urnet-tools/
go build -trimpath -o urnet_docker_bin ./cmd/urnet-docker/
```

Run the tests with the same scope as CI:

```sh
go test -short -timeout 900s ./miner/... ./internal/urnettools/... ./cli/... ./cmd/...
```

Static checks, with the same scope as CI:

```sh
go vet ./miner/... ./cli/... ./cmd/...
gofmt -l miner/ cli/ cmd/ internal/   # prints nothing when clean
```

Build the container image:

```sh
docker build --build-arg TARGETARCH=amd64 --build-arg VERSION=v<ver> -t ghcr.io/full-bars/sn:latest .
```
