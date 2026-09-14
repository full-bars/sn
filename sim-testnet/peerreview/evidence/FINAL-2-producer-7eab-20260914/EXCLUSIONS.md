# Exclusions

Not copied into this bundle:
- raw outer stdout/stderr and all 36 phase logs;
- the private tmp/urnetwork-release-gate.* state tree;
- service environment/configuration, vault, keys, certificates, container identifiers' contents, readiness output, or database state;
- Foundry artifacts, Go caches, binaries, generated test fixtures, plans, signed payloads, and live-node material;
- records from any later producer, aggregate, native, or live session.

The bundle retains SHA-256 values and exact locators for selected retained raw records. Hash-only service-owner and CID records are included solely to document the bounded evidence available; their contents are not copied.
