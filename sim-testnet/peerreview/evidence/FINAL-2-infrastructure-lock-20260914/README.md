# Infrastructure lock integration, 2026-09-14

The read-only native `release-lock` command completed with body and owner exit 0 at 18:38:10.952454483–18:38:14.616939991 UTC. It rendered source `65af88a5d6ce34f63b36de5fbbda98075eac20bf` and xops `d33d4173767ad9dab2f301bc0b0e040579bee896` without RPC, native run state, or `--apply`. The renderer and compiled artifact identities are unchanged from the retained 7eab executable; it reads the current source files from the selected repositories.

The exact YAML SHA-256 is `76cd7fa7031ee5566301173a94e1a4369ccbc871542de05119b46bbcb1461f35`, replacing `7d77c0016491c85e966797f994b6a7c368ad55800c2cb838156439c71b479f84`. The entire diff contains only three fields:

- `repositories.protocol_source_hash`: the corrected producer scheduling script is part of the protocol source digest.
- `infrastructure.gateway_config_hash`: the reviewed gateway playbook/template changes.
- `infrastructure.node_config_hash`: the reviewed node variable changes.

All sixteen contributing repository/library snapshots and the existing lock remained unchanged across rendering. Production Go, contract source, generated artifacts, runtime identities, and all other lock fields retain their exact previous values. This is a local source-lock observation; it is not a complete gate result, deployment result, successor setup-plan approval, reserve transfer, or live soak result.

The preceding refusals are preserved separately: the first native render rejected a symlinked Go module root; the next capture owner exited before the native command because its SHA command named a nonexistent lock path; the following native render rejected the symlinked `evm/lib` directory. The final invocation used physical worktrees for all Go modules and the three pinned Foundry libraries. The attempted extra vault worktree was rejected by the existing git-crypt smudge filter, so the already unlocked physical vault and platform-config overrides were retained. No key or native state was copied.

The earlier review's two-field prediction was corrected before this successful render: the producer script is separately included by `protocolSourceHash`. `RELEASE-INTEGRATION.md` records that source review. Original capture roots and hashes are retained in `RAW-LOCATORS.tsv`; bundled capture files are exact copies except for this explanatory README and the locator list. The source trees were clean during rendering; installing the exact candidate YAML is a subsequent reviewed repository edit. Canonical final-source publication, stamped CLI build, complete producer/aggregate gates, and live RPC deployment remain separate observed steps.

