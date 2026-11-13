# sn (full-bars fork)

`full-bars/sn` is a fork of [`urfoundation/sn`](https://github.com/urfoundation/sn), the code for the UR Subnet (Bittensor Subnet 25, netuid 25), a subnet for a decentralized privacy network.

Besides the subnet code it inherits from upstream, this fork ships a provider that carries traffic over HTTP/3 (QUIC) on IPv4 and IPv6, a miner image, and the tools that install, run, check and update them. Most people who land here want to run a provider or a miner, and the quick start below covers that.

## What you can run from here

- **Container image:** `ghcr.io/full-bars/sn:latest`, with a Docker Hub mirror at `3cape/sn:latest`, built from `Dockerfile` for linux/amd64 and linux/arm64. It bakes in the subnet miner as `provider` and `urnet-tools`. `:miner` is kept as an alias of the same packaging and entrypoint for one release. The image has no vnStat dashboard. The legacy `BUILD=stable|jwt` variable still works (mapped to `AUTH=userpass|authcode`, with a deprecation warning). See [Miner Image](docs/Miner-Image.md) and [Docker Deployment](docs/Docker-Deployment.md).
- **Provider binaries:** `urnetwork-provider-<tag>-<os>-<arch>.tar.gz` on the [releases page](https://github.com/full-bars/sn/releases), for Linux, macOS, Windows, FreeBSD and OpenBSD on amd64 and arm64. The installer scripts in `scripts/` download these for you.
- **`urnet-tools`:** the command line tool that starts, stops, checks and updates a provider on a host. Released as `urnet-tools-<os>-<arch>`.
- **`urnet-docker`:** the same kind of tool for providers that run in Docker containers. It runs on the Docker host. Released as `urnet-docker-<os>-<arch>` for Linux, macOS and Windows.
- **Monitoring bundle:** `urnetwork-monitoring-<tag>.tar.gz` on each release, with Prometheus and Grafana for watching a fleet. See [Monitoring](docs/Monitoring.md).

Release tags look like `v2026.10.29-1084715100-sn`. Releases before the 2026.10.22 tag carried a `-meso` suffix.

## Quick start

### Provider install

Linux:

```sh
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_Linux.sh | sh
```

macOS:

```sh
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_Mac.sh | sh -s -- install
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_Win32.ps1 | iex
```

FreeBSD has its own installer, `scripts/Provider_Install_FreeBSD.sh`, described in [FreeBSD](docs/FreeBSD.md). OpenBSD has `scripts/Provider_Install_OpenBSD.sh`, described in [OpenBSD](docs/OpenBSD.md). Alpine with OpenRC uses the Linux command from a root shell, see [OpenRC](docs/OpenRC.md).

After installing, authenticate and load a proxy list:

```bash
source ~/.bashrc
urnetwork auth
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
urnet-tools status
```

Windows Defender sometimes flags the Windows one-liner and the unsigned binaries. The workaround is in [Installation](docs/Installation.md).

### Container and `urnet-docker`

There is one image, `ghcr.io/full-bars/sn:latest`. Name the container with `urnet` in it so `urnet-docker` can find it. Get an auth code from [ur.io](https://ur.io), then run:

```bash
docker run -d \
  --name=urnet-miner \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e AUTH=authcode \
  -e AUTHCODE='YOUR_AUTH_CODE_HERE' \
  -e HOST_HOSTNAME=$(hostname) \
  -v sn_miner_data:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/sn:latest
```

Auth codes are single-use, and the volume keeps the identity, so later starts reuse the stored session. The image publishes no ports and has no vnStat dashboard. Add `-e ALL_OPERATORS=Y` to mine every listed operator. Options such as `OPERATOR`, `HOTKEY_SEED_FILE` and `AUTO_REGISTER` are in [Miner Image](docs/Miner-Image.md). More run and compose examples are in [Docker Deployment](docs/Docker-Deployment.md).

Install `urnet-docker` once on the Docker host, then manage containers from outside:

```sh
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/install-urnet-docker.sh | sh
```

```bash
urnet-docker providers
urnet-docker status --unit urnet-miner
urnet-docker logs --unit urnet-miner 100
urnet-docker update --unit urnet-miner
```

### Extender setup

On supported hosts, `urnet-tools extender setup` previews the proposed changes and asks before applying them. Use `--dry-run` to inspect the plan. FreeBSD extender setup is untested this release and prints manual commands only; regular providing is unaffected. Container networking must be configured separately. See the [extender guide](docs/Extender.md).

### Uninstall

```sh
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Uninstall_Linux.sh | sh
```

```powershell
irm https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Uninstall_Win32.ps1 | iex
```

## Bittensor payouts

Most operators only need to sign one wallet consent, once, from their own computer, and change nothing on their nodes. Start with the [Bittensor Quick Start](docs/Bittensor-Quick-Start.md). The other Bittensor pages cover optional and advanced setups.

## Documentation

The pages in [`docs/`](docs/) are also published as the [project wiki](https://github.com/full-bars/sn/wiki). The index is [Home](docs/Home.md).

- Setup: [Installation](docs/Installation.md), [Docker Deployment](docs/Docker-Deployment.md), [Miner Image](docs/Miner-Image.md), [Configuration](docs/Configuration.md), [Advanced Deployment](docs/Advanced-Deployment.md), [Multi-Container Scaling](docs/Multi-Container-Scaling.md).
- Platforms: [OpenRC (Alpine)](docs/OpenRC.md), [FreeBSD](docs/FreeBSD.md), [OpenBSD](docs/OpenBSD.md).
- Proxies: [Adding Proxies](docs/Adding-Proxies.md), [Proxy Management](docs/Proxy-Management.md), [Proxy URL Sources](docs/Proxy-URL-Sources.md).
- Tools: [urnet-tools](docs/urnet-tools-go.md), [urnet-docker](docs/urnet-docker.md).
- Operating: [Monitoring](docs/Monitoring.md), [Troubleshooting](docs/Troubleshooting.md), [Log Message Reference](docs/Log-Message-Reference.md), [Provider identity and flags](docs/Provider-Flags.md).
- Bittensor: [Quick Start](docs/Bittensor-Quick-Start.md), [Operations](docs/Bittensor-Operations.md), [Hotkey Delegation](docs/Bittensor-Hotkey-Delegation.md), [Claim Daemons](docs/Bittensor-Claim-Daemons.md), [Fleet](docs/Bittensor-Fleet.md), [Head Tier](docs/Bittensor-Head-Tier.md), [Glossary](docs/Bittensor-Glossary.md).
- Project: [Project Structure](docs/Project-Structure.md), [CI and Release Process](docs/CI-and-Release-Process.md), [Fork Changes](FORK_CHANGES.md), [Changelog](CHANGELOG.md).

## How the subnet works

Network operators run the privacy servers, independent providers carry their traffic, and independent validators measure providers and set weights that Bittensor's consensus turns into emission. Settlement is paid in the subnet's native token, and providers claim their share from an on-chain vault with a Merkle proof. Neither the operators nor the subnet owner hold anyone else's funds.

The mechanism, deposits, emission split, scoring and settlement schedule are specified in [`WHITEPAPER.md`](WHITEPAPER.md), and the validator protocol is in [`VALIDATOR.md`](VALIDATOR.md). For the operator-facing view, read [Bittensor Operations](docs/Bittensor-Operations.md) and the [Bittensor Glossary](docs/Bittensor-Glossary.md). The mainnet launch record is [`mainnet/MAINNET.md`](mainnet/MAINNET.md).

## Repository layout

Provider, miner and tooling:

| Path | What it is |
|---|---|
| `cmd/` | Entry points: `urnet-tools`, `urnet-docker`, `fake-provider` (a test stand-in) and `sp2` (inherited from upstream). |
| `cli/miner/` | The `provider` program the image installs, the miner's command line. Its code lives in `miner/`. |
| `internal/` | Shared code, including `urnettools/` behind `urnet-tools` and `urnet-docker`. |
| `cli/` | Command line programs: `miner`, `validator`, `snclaim`, `payoutroster`, `commitment-oracle`, `trail-probe`. |
| `miner/` | The subnet miner library. |
| `Dockerfile`, `docker/` | The container image and its start scripts. |
| `scripts/` | Installers and uninstallers for each platform, plus build, release and test scripts. |
| `deploy/` | The systemd unit for the provider, and mainnet and testnet deployment files. |
| `monitoring/` | Prometheus and Grafana bundle. |
| `pelican/` | Egg file for the Pelican panel. |
| `releases/` | Release notes, one file per release. |
| `fake-release-server/` | A fake release server for testing the installers. |
| `docs/` | The documentation pages listed above, plus design notes and specs. |

Subnet code:

| Path | What it is |
|---|---|
| `evm/` | Solidity contracts (coordinator, settlement vault, reserve sink) and their tests. |
| `stabi/` | Go bindings for the contracts. |
| `validator/` | The validator and its scoring. |
| `chain/`, `crv4/`, `evmrpc/`, `ss58/`, `stctl/` | Chain access: calls, commit-reveal weights, RPC, address encoding, a diagnostic tool. |
| `ledger/`, `merkle/`, `payoutartifact/`, `payoutroster/`, `commitmentoracle/` | Payout records, Merkle trees and the commitment oracle. |
| `clientauth/`, `hotkeyauth/`, `hotkeywallet/`, `walletsign/`, `nativefee/`, `operatorlist/`, `protocol/` | Client identity, hotkey and wallet handling, wallet consent signing, operator list and claim protocol. |
| `mainnet/`, `sim-testnet/`, `rehearsal/` | Mainnet launch material, the testnet harness and rehearsal tests. |
| `api/`, `iterations/`, `seed/`, `diagnostics/`, `diagrams/`, `testdata/`, `tools/`, `third_party/` | Competition config, design notes, diagnostics helpers, diagrams, test data, probes and vendored dependencies. |

Top-level files: `WHITEPAPER.md`, `VALIDATOR.md`, `SUBTENSOR-COMPATIBILITY.md`, `FORK_CHANGES.md`, `CHANGELOG.md`, `operators.yml` and `price.yml`.

## Upstream and license

This repository tracks [`urfoundation/sn`](https://github.com/urfoundation/sn), and the subnet design, contracts, miner and validator originate there. The changes this fork makes on top are listed in [FORK_CHANGES.md](FORK_CHANGES.md).

Upstream's [`LICENSE`](https://github.com/urfoundation/sn/blob/main/LICENSE) applies to the code inherited from it. Third-party code under `third_party/` keeps its own license.
