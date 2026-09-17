# CI and Release Process

This page describes the automated checks and the release flow for the SN provider repository.

## Workflows

The repository ships sixteen workflows in `.github/workflows/`.

### Push and pull request gates

| Workflow | When it runs | What it does |
|----------|--------------|--------------|
| Build and Push Docker Image (`build.yml`) | main push, pull requests, version tags | Go unit tests with the race detector, go vet, gofmt check, pelican egg validation, shellcheck, then a multi-arch Docker build for amd64 and arm64. Images push to `ghcr.io/full-bars/sn` and `3cape/sn` on GitHub Container Registry and Docker Hub |
| Dash/POSIX Compatibility (`dash-compat.yml`) | main push, pull requests | Validates the installers run under `dash`, not just bash |
| Unix lifecycle verification (`unix-lifecycle.yml`) | main push, pull requests | start/stop/restart/status against a fake provider on systemd |
| Windows lifecycle verification (`windows-lifecycle.yml`) | main push, pull requests | Same lifecycle set against a fake provider under schtasks |
| Tool functional smoke (`tool-functional-smoke.yml`) | main push, pull requests | Builds the real binaries and mints a JWT with the gauntlet account |
| docker-multi-container (`docker-multi-container.yml`) | main push | Targets and modes across three containers on one host |

Docs-only changes skip CI through `paths-ignore`.

### Scheduled and manual

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| CFAA blocklist sync (`cfaa-blocklist-sync.yml`) | cron twice daily, repository dispatch | Syncs the packed IP blocklist from upstream |
| CodeQL (`codeql.yml`) | weekly schedule | Security analysis |
| Upstream monitor (`upstream_monitor.yml`) | schedule | Watches upstream commits and PRs for files shared with this fork |
| Wiki sync (`wiki-sync.yml`) | docs push | Copies `docs/*.md` into the wiki |
| Shakedown (`shakedown.yml`) | `v2026.*` tags, manual | Full pre-release shakedown on a clean runner |
| Docker shakedown (`docker-shakedown.yml`) | `v2026.*` tags, manual | Deep Docker test matrix |
| Functional soak (`functional-soak.yml`) | manual | Three-hour soak |
| Test gauntlet (`test-gauntlet.yml`) | manual | Full gauntlet, manually dispatched |

## Release flow

Releases follow the `v2026.<date>-<unixtime>-meso` tag scheme, for example `v2026.9.17-1789646883-meso`.

### Cut a release

1. Ensure `build.yml` is green on main.
2. Write release notes at `releases/<tag>.md`.
3. Push a signed tag: `git tag -s <tag> && git push origin <tag>`.
4. Release Binaries builds the six-platform matrix, bundles the tarballs including the monitoring bundle and installers, and publishes with the notes from `releases/<tag>.md`. If the file is absent, GitHub auto-generates the notes.
5. The shakedown and docker shakedown fire on the same tag and validate the published artifacts.

### Scanning

VirusTotal and ClamAV scan every binary in a parallel job. The scan job never gates publication. It appends the verdict to the release body and stages flagged files for submission to Microsoft Defender.

### Docker images

- Version tags push `:latest` plus the tag name to both registries.
- Pre-release tags with `-rc`, `-alpha`, or `-beta` never touch `:latest`.
- Main pushes push `:main` and a short-SHA tag so the image is always current.