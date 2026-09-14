# Configuration-path recovery evidence — 14 September 2026

The first ca42812 native setup apply ran from 02:22:23 to 02:33:52 UTC and exited 1. It adopted approved plan ae15ecdd, completed the 1,000-fleet historical batch, and refused during carried-action verification because the existing operator config links named the historical config checkout. The transaction journal and supervisor files remained byte-identical; no repair transaction was submitted.

The supported recovery changes only --platform-config-repo to that historical path. All seven release-bound local file paths and contents match the qualified checkout exactly; both shared Git trees match. Filesystem modes differ 0664 versus 0644, while Git modes and the release content digests agree. The release binds these config contents and shared tree, and resolved plan identity excludes repository paths. The review records both distinct repository heads and does not relabel the old checkout as the qualified commit.

The native setup preview exited 0 at 05:08:04 UTC and returned the identical installed ae15ecdd plan with all 2,309 actions unchanged. All four observed state files remained byte-identical. The full private preview plan is omitted; its exact digest is retained in the native result and SUMMARY.json.

The current approved repair retry is a separate execution and is not proved complete by this preview. Its eventual native result and on-chain debit/credit belong in a later bundle. No source, checkout, symlink, journal, test or binary change was needed for this path override. Future commands retain the same historical config path and owned LAN RPC route.

Raw native receipts and review bytes are retained unchanged. SHA256SUMS covers this bundle and MANIFEST.json. The result is operational recovery evidence, not live campaign or final testnet acceptance.
