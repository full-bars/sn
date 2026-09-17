# Welcome to the SN Provider Documentation

This documentation covers the SN provider for URNetwork, the production H3/QUIC provider in the [full-bars/sn](https://github.com/full-bars/sn) repository.

The provider serves dual-stack IPv4 and IPv6 traffic, uses HTTP/3 (QUIC) for its client connections, and is built for high-volume professional operation. You can run it as a native binary or in Docker, on Linux, macOS, or Windows.

## Documentation Index

### Getting Started
- [Installation](Installation) — native install on Linux, macOS, and Windows, plus the one-line installer
- [Configuration](Configuration) — environment variables and runtime flags
- [Docker Deployment](Docker-Deployment) — production Docker guide: images, env vars, upgrades
- [Advanced Deployment](Advanced-Deployment) — security hardening and system-level networking
- [Multi-Container Scaling](Multi-Container-Scaling) — multiple provider containers per host
- [urnet-docker](urnet-docker) — the host-side tool for managing provider containers

### Proxies
- [Adding Proxies](Adding-Proxies) — first proxy list, quick start
- [Proxy Management](Proxy-Management) — file lists, add, remove, health, traffic views
- [Proxy URL Sources](Proxy-URL-Sources) — live proxy-list URLs with scheduled fetches
- [Proxy Hot-Reload](Proxy-Hot-Reload) — add and remove proxies without a restart
- [Proxy Admission Pipeline](Proxy-Admission-Pipeline) — probing, verification, and grading before admission

### Operating
- [urnet-tools-go](urnet-tools-go) — the provider-aware fleet ops tool
- [Monitoring](Monitoring) — Prometheus metrics and the Grafana stack
- [Node Identity](Node-Identity) — dashboard labels, naming, and IP privacy
- [Egress Security Policy](Egress-Security-Policy) — packet inspection: CFAA reputation, DMCA BitTorrent, web standards
- [Bittensor Operations](Bittensor-Operations) — Subnet operations and status

### Reference
- [Log Message Reference](Log-Message-Reference) — what provider log lines mean
- [Troubleshooting](Troubleshooting) — common errors and their fixes
- [CI and Release Process](CI-and-Release-Process) — workflows, tags, and cutting a release

## Quickstart

Minimal Docker run:

```bash
docker run -d \
  --name=urnetwork \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e URNETWORK_NODE_NAME=my-provider \
  -v urnetwork_config:/root/.urnetwork \
  -v /path/to/your/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/sn:latest YOUR_AUTH_CODE_HERE
```

The Docker image is multi-arch (linux/amd64 and linux/arm64) and is published to GitHub Container Registry as `ghcr.io/full-bars/sn`, and to Docker Hub as `3cape/sn`.

### Resource Usage Tips
- **Log management**: use `--log-driver=json-file` with `--log-opt max-size=100m` and `--log-opt max-file=3` to keep logs from filling the disk. This is recommended for long-running containers.
- **Monitoring**: turn on the built-in metrics endpoint and point the bundled Grafana stack at your providers. See [Monitoring](Monitoring).

## Support

For troubleshooting input, see the [Troubleshooting](Troubleshooting) guide. For log line meanings, see the [Log Message Reference](Log-Message-Reference).