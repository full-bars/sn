Use the existing qualified CLI with the historical platform-config override. Both config checkouts already satisfy the same qualified release lock; no checkout, link, runtime-manifest, journal, or code edit is needed for this recovery.

The required override is:

```text
--platform-config-repo /home/by/urnetwork/temp/sn-soak-deadline-20260909T221044Z/finalization-20260911T0215/workspace/config
```

All seven `local` file contents are identical. The only observed filesystem difference is mode `0664` at the historical path versus `0644` at the qualified path; both Git indexes record mode `100644`. Both repositories are clean. The independently recomputed digests equal the active release lock:

| Locked input | Digest at both paths |
| --- | --- |
| `platform_config_source_hash` | `sha256:c69caf2830a1ab8cf9b5f884b2351913bbba19620947176d78fd7eb6b676974a` |
| `platform_config_shared_tree_hash` | `sha256:6852f0a67ab5e5cd5e61fbe3d53ac367da7d43c92f766c2de6cff43db40c5252` |

The historical config HEAD is `b5a7b4d8438e80ebc4e8529ba00a872495717109`; the qualified config HEAD is `268f11b7e7d9c43eda3fd6f34447d26c2f0c21dc`. Platform config is locked by the complete `local` path/content digest and reviewed `all` Git tree, rather than by repository HEAD. Source observation requires a clean HEAD that stays fixed during the read. The mode difference does not change either digest. See [config input observation](/home/by/urnetwork/sn/sim-testnet/release_lock.go:920), [path/content hashing](/home/by/urnetwork/sn/sim-testnet/release_lock.go:161), [shared-tree hashing](/home/by/urnetwork/sn/sim-testnet/release_lock.go:252), and [bracketed clean repository observation](/home/by/urnetwork/sn/sim-testnet/release_lock_render.go:251).

Repository paths do not enter [resolved plan inputs](/home/by/urnetwork/sn/sim-testnet/plan.go:673). With every other argument retained, the expected plan remains `0xae15ecdd37cac2a223533b4a1b3d9fa6431da33d78cfd9ccac006a98e9d8f414`. The [overlay verifier](/home/by/urnetwork/sn/sim-testnet/config_overlay.go:79) compares each link target to the invocation's platform-config path. Selecting the historical path makes the existing four links exact while retaining the unchanged manifest and historical receipts.

Temporary API startup uses the existing rendered `testnet-rpc-urls: [http://127.0.0.10:19944]`. [Native server specifications](/home/by/urnetwork/sn/sim-testnet/process.go:1409) route that loopback proxy through the campaign egress proxy to `cfg.OperationalEVM`, which the selected owned authority sets to `http://192.168.1.162:9944`; the Substrate proxy uses the same LAN authority over WebSocket. [Provisioning selects only those proxies and operator APIs](/home/by/urnetwork/sn/sim-testnet/process.go:1510). The historical `testnet-public-rpc-url` is published response metadata, not a server dial target; its only production use is [assigning the epoch response RPC URL](/home/by/urnetwork/temp/sn-warp-admission-integration-20260913/workspace-physical/server/controller/sn_earnings_controller.go:229). [Native launch rerenders](/home/by/urnetwork/sn/sim-testnet/process.go:800) the owned URL and all current configs before starting persistent clients and the complete topology.

`DRY-RUN-REQUEST.json` and `APPLY-PREPARED.json` contain exact argument arrays derived from the failed approved request. Apply changes only the platform-config argument. The native dry run is owned by root and was still running when this handoff was written; actual success must come from its result. After the expected plan identity is confirmed, the existing authorization covers the prepared apply.

Retain the full common arguments from `APPLY-PREPARED.json` for future configured commands: qualified `--config`, actual `--state-dir`, qualified SN/server/operator-proxy/vault overrides, the historical platform-config override above, `--owned-rpc-authority 192.168.1.162:9944`, and `--format json`. This applies to setup, launch, resume, scenario, status, doctor, plan, history-adoption, relay-continuation, fleet-renew, and configured inspect/analyze where those flags are supported. Keep the applicable exact native plan hash for mutations and the authorized scope. For commands such as stop that reject `--owned-rpc-authority`, retain the historical platform-config override and omit only unsupported options.

Detailed per-file SHA-256 values, modes, clean status, repository identities, and prepared request provenance are in `REVIEW.json`. No tests, builds, formatter, native commands, or qualified graph changes were performed by this debugger.
