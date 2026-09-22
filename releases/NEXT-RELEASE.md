# NEXT-RELEASE

This release hardens the CI pipeline so a build break on a non-Linux platform is caught before merge instead of at release time. It also makes the test suite deterministic.

## What's Changed

### Changed

- **Cross-platform compile gate on every PR** ([#28](https://github.com/full-bars/sn/pull/28)): PR CI now compiles the provider, `urnet-tools`, and `urnet-docker` binaries for Linux, macOS, and Windows across the amd64 and arm64 architectures. A change that breaks a build on any of those platforms fails the PR instead of surfacing only at release time.

### Maintenance

- **Deterministic test suite** ([#27](https://github.com/full-bars/sn/pull/27)): the reload-trigger and health-registry tests no longer depend on timing or shared state.