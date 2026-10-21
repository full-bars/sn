# NEXT-RELEASE

### Unreleased

- **Go 1.27.3 blocks the eight compromised eMudhra intermediates (`CVE-2026-111943`)**: `go.mod`, the Dockerfile builder and every `setup-go` pin Go 1.27.3, so a binary built from this release carries the denylist. A binary built before it may continue trusting the intermediates, although an updated operating-system trust store may already reject them. The rest of what lands in this release is in the changelog's `[Unreleased]` section.