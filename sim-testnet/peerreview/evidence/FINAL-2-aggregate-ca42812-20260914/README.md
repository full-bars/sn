# FINAL-2 aggregate-gate evidence — 14 September 2026

The ca42812 SN / 0f095a server aggregate gate passed from 00:26:03 to 04:28:36 UTC. All 25 native phase joins and the outer exit are zero. All seven preflights, including the final source freeze, exited zero. Initial and final source-freeze stdout is byte-identical across 13 repository heads and matches the separately completed producer gate.

The gate retains the full test populations, race checks, database checks and original deadlines. Commands and complete native phase logs are copied unchanged. The server is the qualified published ancestor 0f095a; this does not claim qualification of unrelated newer server changes.

The original concurrent startup exited 1 before a test body ran because Git fetches collided on a moving server remote-tracking reference. Its capture and command remain under original-startup-refusal and receive no PASS credit. The successful retry uses the same source and workload with separate output paths.

This proves the complete local aggregate gate. It does not prove deployment, live epochs, on-chain payments or final testnet acceptance. The native setup command refused a stale runtime config link before any transaction, recorded separately.

MANIFEST.json records retained sources, sizes, digests and roles; SHA256SUMS also covers the manifest. Raw bytes are preserved. Test binaries, caches, source contents, vault contents and private service environments are excluded.
