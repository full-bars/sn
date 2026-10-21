# NEXT-RELEASE

### Unreleased

- **Go 1.27.3 blocks the eight compromised eMudhra intermediates (`CVE-2026-111943`)**: `go.mod`, the Dockerfile builder and every `setup-go` pin Go 1.27.3, so a binary built from this release carries the denylist. A binary built before it may continue trusting the intermediates, although an updated operating-system trust store may already reject them. The rest of what lands in this release is in the changelog's `[Unreleased]` section.
- **`provider wallet sign` and `provider wallet status`**: sign the network wallet consent with this fork's own `provider`, then check whether it is in effect. `wallet sign` runs a local page on 127.0.0.1 where a browser wallet extension signs; the provider never sees a seed or key. Older builds do not have these commands, so the [Bittensor Quick Start](../docs/Bittensor-Quick-Start.md) shows how to check yours (`provider --help`) and keeps the 3.23-fix fallback.
- **The retired fleet hub's report commands and settings are removed**: `urnet-tools report`, `urnet-docker report`, `report-url`, `report-interval`, `URNETWORK_REPORT_URL`, `URNETWORK_REPORT_INTERVAL` and `URNETWORK_HEARTBEAT_INTERVAL` are gone, with the reporters behind them. A script or cron job that still calls them now fails or has the variable ignored; remove the call. A saved `report_url` is ignored, so nodes that had one still start. Use `urnet-tools metrics on` and the monitoring bundle for fleet visibility.
- **The `provider` now carries the 3.23-fix operator surface** (the one-program port): hot-swap, the client-JWT store, Prometheus metrics, h3 limit modes, proxy grading and earnings-ranked trims, and the `provider proxy`, `direct`, `logs`, `print-network-id`, `sn-status` and `claim --store-client` commands, so unmodified `urnet-tools` and `urnet-docker` drive it.
- **Signed hotkey wallet delegation**: `provider wallet hotkey challenge|set|status`. `provider wallet set` and `provide --wallet` now refuse without `--legacy-network-wallet`, because mainnet no longer accepts the unsigned request.
- **OpenRC and FreeBSD are supported**: lifecycle, discovery, installers, auto-update, release artifacts and CI. See the [OpenRC](../docs/OpenRC.md) and [FreeBSD](../docs/FreeBSD.md) guides.
- **Reliability fixes**: a stalled proxy reload can no longer freeze the pool, the startup disk benchmark is bounded and no longer prints a false low-disk warning, a failed renewal no longer replaces a client identity, a stalled provider is restarted (systemd and container), and a false "wrong network" warning no longer costs an auth code.
- **The upstream mainline is merged through `6608298e`**: settlement v2, validator, mainnet and EVM work, and the `provider claim-daemon-v2`, `operators`, `provide --all-operators`, `auth --operator` and `fleet sign` commands.
- **CI**: the VirusTotal scan covers every artifact, and the installer matrix gates publication.

The full list, with PR numbers, is in the changelog's `[Unreleased]` section.
