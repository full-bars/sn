# Welcome to the SN Provider Documentation

This documentation covers the provider and its tools in the [full-bars/sn](https://github.com/full-bars/sn) repository, a fork of `urfoundation/sn` (the UR Subnet, Bittensor SN25).

The provider serves dual-stack IPv4 and IPv6 traffic, uses HTTP/3 (QUIC) for its client connections, and is built for high-volume professional operation. You can run it as a native binary or in Docker, on Linux, macOS, or Windows.

## Documentation Index

### Getting Started
- [Installation](/full-bars/sn/wiki/Installation) — native install
- [Configuration](/full-bars/sn/wiki/Configuration) — environment variables and runtime settings
- [Docker Deployment](/full-bars/sn/wiki/Docker-Deployment) — images and upgrades
- [Advanced Deployment](/full-bars/sn/wiki/Advanced-Deployment) — security and networking
- [Multi-Container Scaling](/full-bars/sn/wiki/Multi-Container-Scaling) — multiple containers per host
- [urnet-docker](/full-bars/sn/wiki/urnet-docker) — container management

### Proxies
- [Adding Proxies](/full-bars/sn/wiki/Adding-Proxies) — first proxy list
- [Proxy Management](/full-bars/sn/wiki/Proxy-Management) — lists, health and traffic
- [Proxy URL Sources](/full-bars/sn/wiki/Proxy-URL-Sources) — scheduled URL fetches
- [Proxy Hot-Reload](/full-bars/sn/wiki/Proxy-Hot-Reload) — update a running pool
- [Proxy Admission Pipeline](/full-bars/sn/wiki/Proxy-Admission-Pipeline) — probing and grading

### Operating
- [urnet-tools-go](/full-bars/sn/wiki/urnet-tools-go) — provider management
- [Monitoring](/full-bars/sn/wiki/Monitoring) — metrics and dashboards
- [Extender](/full-bars/sn/wiki/Extender) — direct-slot activation
- [Hot Restart](/full-bars/sn/wiki/Hot-Restart) — client identity reuse
- [Node Identity](/full-bars/sn/wiki/Node-Identity) — node labels
- [Egress Security Policy](/full-bars/sn/wiki/Egress-Security-Policy) — packet inspection
- [Bittensor Quick Start](/full-bars/sn/wiki/Bittensor-Quick-Start) — start here
- [Bittensor Operations](/full-bars/sn/wiki/Bittensor-Operations) — subnet overview
- [Bittensor Head Tier](/full-bars/sn/wiki/Bittensor-Head-Tier) — optional advanced setup
- [Bittensor Glossary](/full-bars/sn/wiki/Bittensor-Glossary) — terms

### Reference
- [Provider Flags](/full-bars/sn/wiki/Provider-Flags) — provider identity and registration
- [Project Structure](/full-bars/sn/wiki/Project-Structure) — repository layout
- [Log Message Reference](/full-bars/sn/wiki/Log-Message-Reference) — log lines
- [Troubleshooting](/full-bars/sn/wiki/Troubleshooting) — common failures
- [CI and Release Process](/full-bars/sn/wiki/CI-and-Release-Process) — workflows and releases

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
  -e URNETWORK_AUTH_CODE=YOUR_AUTH_CODE_HERE \
  -v urnetwork_config:/root/.urnetwork \
  -v /path/to/your/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/sn:latest
```

The auth code is single-use. Keep the state volume to retain the sign-in and provider identity. See [Node Identity](/full-bars/sn/wiki/Node-Identity) for node labels.

The Docker image is multi-arch (linux/amd64 and linux/arm64) and is published to GitHub Container Registry as `ghcr.io/full-bars/sn`, and to Docker Hub as `3cape/sn`.

### Resource Usage Tips
- **Log management**: use `--log-driver=json-file` with `--log-opt max-size=100m` and `--log-opt max-file=3` to keep logs from filling the disk. This is recommended for long-running containers.
- **Monitoring**: use the built-in metrics endpoint and point the bundled Grafana stack at your providers. See [Monitoring](/full-bars/sn/wiki/Monitoring).

## Support

For troubleshooting input, see the [Troubleshooting](/full-bars/sn/wiki/Troubleshooting) guide. For log line meanings, see the [Log Message Reference](/full-bars/sn/wiki/Log-Message-Reference).