# FINAL-2 launch-gate evidence — 14 September 2026

The current ca42812 SN / 0f095a server producer gate passed from 00:22:38 to 02:22:22 UTC. All 36 native phase joins and the outer exit are zero. Initial and final source-freeze stdout is byte-identical, binding the same 13 repository heads. The commands retain the original full scope and RUN_SERVER_DB_TESTS=1 execution.

Raw captures, phase logs, command files and preflight bytes are copied unchanged. The join index and source identity JSON summarize those receipts. BINDING.json retains its original pre-execution observation; the later raw completion receipts establish the gate result. The server source is the qualified published ancestor 0f095a, not a claim about the latest remote main head.

This proves the launch-critical local gate. It does not claim completion of the concurrent aggregate suite, a live campaign, or final testnet acceptance. No on-chain transaction receipt is included in this gate bundle. Native runtime preparation and repair receipts are recorded separately.

MANIFEST.json records each retained file's source, digest, size and role. SHA256SUMS additionally covers the manifest. Test binaries, caches, vault contents, source contents and private service/runtime environments are excluded.
