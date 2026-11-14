# CI and Release Process

This page describes the automated checks and the release flow for the SN provider repository.

## Workflows

The repository ships 23 workflows in `.github/workflows/`.

### Push and pull request gates

| Workflow | When it runs | What it does |
|----------|--------------|--------------|
| Build and Push Docker Image (`build.yml`) | main push, pull requests, version tags | Go unit tests with the race detector, go vet, gofmt check, pelican egg validation, shellcheck, the ClamAV verdict harness (`.github/scripts/test-clamav-scan.sh`, ten cases against a mock `clamscan`), then a multi-arch Docker build for amd64 and arm64. The builder stage asserts the machine type of `provider`, `urnet-tools` and `urnet-docker` for the target architecture (`docker/check-elf-arch.sh`) and fails the build on a mismatch. Images push to `ghcr.io/full-bars/sn` and `3cape/sn` on GitHub Container Registry and Docker Hub |
| Dash/POSIX Compatibility (`dash-compat.yml`) | main push, pull requests | Validates the installers run under `dash`, not just bash |
| Unix lifecycle verification (`unix-lifecycle.yml`) | main push, pull requests | start/stop/restart/status against a fake provider on systemd |
| Windows lifecycle verification (`windows-lifecycle.yml`) | main push, pull requests | Same lifecycle set against a fake provider under schtasks |
| Tool functional smoke (`tool-functional-smoke.yml`) | main push, pull requests | Builds the real binaries and mints a JWT with the gauntlet account |
| PR soak churn (`pr-soak-churn.yml`) | pull requests, manual | Adds and removes one proxy while the provider runs, and bounds the growth of goroutines, memory and file descriptors. See below |
| docker-multi-container (`docker-multi-container.yml`) | main push | Targets and modes across three containers on one host, each with its own state volume |

Docs-only changes skip CI through `paths-ignore`.

### The churn soak

`pr-soak-churn.yml` starts a provider with three anchor proxies and a test account. It waits two minutes. Then it adds and removes one more proxy three times each, one minute apart.

`.github/scripts/soak_settled.py` takes the measurements. Both the baseline and the final check use the lowest of five samples taken ten seconds apart. The final check waits 60 seconds first. The bounds are 5 percent for goroutines, 10 percent for memory and 1000 file descriptors.

The goroutine bound counts only goroutines that a proxy slot can own. The libp2p gossip peers and the UDP read pollers start and stop on their own, so the helper leaves them out. It reads the number of running slots from the metrics page. The provider also adds proxies from a public list during the run. If the slot count rises, the bound applies per slot.

When the goroutine bound fails, the log lists the stack groups that grew. The job keeps both profiles as the `churn-goroutines` artifact. Run `python3 .github/scripts/test-soak-settled.py` to check the helper itself.

### Scheduled and manual

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| CodeQL (`codeql.yml`) | weekly schedule | Security analysis |
| Upstream monitor (`upstream_monitor.yml`) | schedule | Watches upstream commits and PRs for files shared with this fork |
| Wiki sync (`wiki-sync.yml`) | docs push | Copies `docs/*.md` into the wiki |
| Shakedown (`shakedown.yml`) | `v2026.*` tags, manual | Full pre-release shakedown on a clean runner |
| Docker shakedown (`docker-shakedown.yml`) | `v2026.*` tags, manual | Deep Docker test matrix |
| Docker PR build (`docker-pr-build.yml`) | pull requests touching `docker/` or the Dockerfile | Builds the image from the checkout and runs the credential-free checks, including a `linux/arm64` build of the builder stage so the architecture assertion runs on every image change |
| Functional soak (`functional-soak.yml`) | manual | Three-hour soak |
| Test gauntlet (`test-gauntlet.yml`) | manual | Full gauntlet, manually dispatched |

## Release flow

Releases follow the `v<date>-<build>-sn` tag scheme, where the build number is the seconds since the 2023-05-23 founding epoch times ten — not a raw unix stamp. For example `v2026.10.29-1084766765-sn`. `scripts/compute-sn-tag.sh` prints it.

### Cut a release

1. Ensure `build.yml` is green on main.
2. Compute the tag once with `tag=$(scripts/compute-sn-tag.sh)` and keep that value for every remaining step. The value changes with time.
3. Copy the prepared `releases/next-sn.md` to `releases/<tag>.md`, replacing `<tag>` in the filename with that exact value. Update its heading and asset names using the same value. Commit the final notes to main before tagging.
4. Push a signed tag of that commit with the same value: `git tag -s "$tag" -m "$tag" && git push origin "$tag"`.
5. Release Binaries builds five operating systems on amd64 and arm64, bundles the tarballs including the monitoring bundle and installers, and publishes with the notes from `releases/<tag>.md`. If the file is absent, GitHub auto-generates the notes.
6. The shakedown and docker shakedown fire on the same tag and validate the published artifacts.

### Scanning

ClamAV gates publication. The `create-release` job runs `.github/scripts/clamav-scan.sh` on the exact files that the release step uploads, before it publishes anything. A detection or a scanner failure fails the job, and the release is not created.

VirusTotal runs afterwards in the `scan` job and never gates publication. It scans the `provider`, `urnet-tools` and `urnet-docker` binaries and the `miner` binary from each `miner-*` artifact. It appends its verdict to the release body and stages flagged files for submission to Microsoft Defender. The ClamAV report is appended to the release body by the `create-release` job.

The script fetches fresh definitions with `freshclam` before every scan and tries three times. If the refresh fails, it fails closed and does not scan with old signatures. It reports `clean` only when the definitions were refreshed, the EICAR self-test detected the sample first, and the scan counted files and exited 0 with nothing found. A detection, a scanner error, a stale database, a file over the size limits, an inconsistent exit code and an empty target each fail the step with their own line in the report.

The ten-case harness `.github/scripts/test-clamav-scan.sh` tests the script. `build.yml` runs it on pull requests, and the release job runs it again before the scan.

If the gate fails, read the job log. A detection means you investigate, fix the cause and cut a new tag. A refresh or network failure means you re-run the failed job.

### Docker images

- Version tags push `:latest` plus the tag name to both registries.
- Pre-release tags with `-rc`, `-alpha`, or `-beta` never touch `:latest`.
- Main pushes push `:main` and a short-SHA tag so the image is always current.
- ClamAV gates every push. Before it pushes, the job builds the root filesystem of each platform and scans it with `.github/scripts/clamav-scan.sh`, with fresh definitions. A detection or a scanner failure stops the push, so no tag moves. The same gate runs in the miner image workflow. The push step reuses the layers from the build cache. If the cache has been evicted, the push step builds again from scratch and takes longer.