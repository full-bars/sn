# urnet-tools / urnet-docker Porting Analysis

Date: 2026-09-16
Source: `urnetwork-3.23-fix` (module `github.com/urnetwork/connect`)
Target: `h3-provider` (module `github.com/urfoundation/sn`, connect via `replace => github.com/full-bars/connect v0.0.0-20260916141202-065bcdd9b85d`)

## TL;DR

**Recommendation: copy `internal/urnettools` into this repo as `github.com/urfoundation/sn/internal/urnettools`, along with both `cmd/` wrappers.** Don't try to make `internal/` public in `full-bars/connect`. That option doesn't exist: the v2026 connect has no `internal/` tree at all. `urnettools` was only ever in the 3.23-fix fork.

I checked this with a trial port in a throwaway worktree (since removed):

| Check | Result |
|---|---|
| Code changes needed | 1 import-path rewrite (2 files), 1 new 50-line shim file, 1 function body edit, 1 test-file `sed` |
| `go build` linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64, windows/arm64 | all OK (`urnet-tools` and `urnet-docker`) |
| `go test ./internal/urnettools ./cmd/urnet-tools ./cmd/urnet-docker` | **ok** (94s), same as the 3.23-fix baseline (87s) |
| New module dependency | `github.com/spf13/cobra v1.10.2` (+ `pflag` indirect) |

The Go code is the easy part. Real parity is blocked by **release plumbing**, not code. h3-provider's `release.yml` does not publish the assets that `urnet-tools update` and the installer need (see §5). That is the P0 item.

---

## 1. What `internal/urnettools` contains

| | Files | Lines |
|---|---|---|
| Production `.go` | 89 | 16,602 |
| Test `.go` | 58 | 13,230 |
| `cmd/urnet-tools/main.go` + `cmd/urnet-docker/main.go` | 2 | 57 |
| Shell installer `scripts/Provider_Install_Linux.sh` | 1 | 3,158 |

Main areas:

- **CLI surface:** `cli.go`, `cobra.go` (1,206), `cobra_docker.go`, `cli_docker.go` (1,079), `legacy_cmds.go` (1,082), `session_cmds.go`, `usage*.go`
- **Discovery / targeting:** `discover{,_unix,_darwin,_windows}.go`, `target.go`, `select_multi.go`, `default_provider.go`. These find every running provider across all users and identify each one by its JWT network name.
- **Provider IPC:** `control_client.go` (JSON-lines over `control.sock`), `pending_overrides_lock_{unix,windows}.go` (flock file fallback when the socket is down), `hotswap*.go`, `metrics_status.go`
- **Lifecycle:** `lifecycle_{unix,darwin,windows,…}.go`, `restart_escalation.go`, `provider_recover*.go`, `self_heal.go`, `restore_delegate.go`
- **Update:** `update.go` (1,789), `release.go`, `idle_update.go`. Covers provider tarball + tool self-update, sha256 digest verification, and systemd unit reconcile.
- **Docker:** `docker.go`, `docker_actions.go` (`docker exec` delegation), `running_image_*.go`
- **SN:** `sn_status.go` (ranking / epoch / pool claim)

### Dependencies (production code)

| Import | Present in sn `go.mod`? |
|---|---|
| stdlib | n/a |
| `github.com/spf13/cobra` | **No, add it** |
| `golang.org/x/term`, `golang.org/x/sys/{unix,windows}` | Yes |
| `github.com/urfoundation/sn/ss58` | Yes. **It's this repo.** 3.23-fix imports sn as an external module; after the port the import becomes in-module. |
| `github.com/urnetwork/connect` | Yes. Only 7 symbols are used: `NewBringYourApi`, `NewClientStrategyWithDefaults`, `ParseByteCount`, `SnEpochSync`/`SnEpochResult`, `SnPoolClaimSync`/`SnPoolClaimArgs`/`SnPoolClaimResult`, and **`NetworkGetRankingSync`/`NetworkRankingResult`**. |

`urnettools` does **not** import any other `internal/` package (`internal/promtext` is unrelated) or anything from `provider/`. It talks to the provider only through the process boundary: control socket, pending-overrides file, CLI exec, and systemd. This loose coupling is why the port is cheap.

### The one v2026 API gap

`BringYourApi.NetworkGetRankingSync` and `connect.NetworkRankingResult` live in 3.23-fix's `api_ranking.go`, a fork-only extension. Neither is in v2026 connect. `h3-provider/provider/sn.go:228` already hit this and added a local `networkGetRankingSync` built on `connect.HttpGetWithStrategy`. The port does the same thing:

- New `internal/urnettools/sn_ranking.go`: local `NetworkRanking`/`NetworkRankingError`/`NetworkRankingResult` types, plus a `rankingAPI` struct that embeds `*connect.BringYourApi` and adds `NetworkGetRankingSync()` (GET `{apiUrl}/network/ranking` with the JWT).
- `sn_status.go`: change the `snStatusAPI` interface to the local type, and have `newSnStatusAPI` return `&rankingAPI{…}`.
- `sn_status_test.go`: `s/connect\.NetworkRankingResult/NetworkRankingResult/; s/connect\.NetworkRanking{/NetworkRanking{/`

The proposed shim was saved during the trial. It's reproduced in Appendix A.

## 2. Options

### A. Copy `urnettools` into sn as `internal/urnettools` (recommended)

- Visibility works: `internal/` under the `github.com/urfoundation/sn` root is importable from `sn/cmd/*`.
- The measured change is tiny (§TL;DR), and the full test suite passes.
- Keeps `internal/`, so no public API commitment.
- Cost: this becomes a second copy alongside 3.23-fix. That's fine because 3.23-fix is the stable line being migrated *away from*. Once sn is canonical, 3.23-fix's copy freezes and there's only one to maintain.

### B. Make `internal/urnettools` public in `full-bars/connect`

- **Not viable as stated.** `full-bars/connect@065bcdd` (what sn's `replace` points at) and `/home/klets/h3-workspace/connect` have **no `internal/` directory**. You'd first have to add 30k lines of provider-management CLI to the networking library and then export them.
- That makes connect depend on `cobra` and on `github.com/urfoundation/sn/ss58`, while **sn depends on connect**. That's a module cycle. Go tolerates module-level cycles, but it's a maintenance trap, and every `urnet-tools` fix would need a connect bump plus a pseudo-version bump in sn.
- It also puts operator tooling in a package upstream-sync work has to diff around, which makes `newversioncheck` audits noisier.

### C. Keep building urnet-tools from the 3.23-fix repo and ship it next to the sn provider

- Works for now because the wire contract is identical (§3), but it pins tooling to a repo you're retiring. `release.go`/`update.go` would keep pulling **3.23-fix provider tarballs**, so `urnet-tools update` would downgrade an sn node back to the 3.23 provider. Rejected.

### D. Separate `urnet-tools` module/repo

- Clean in theory, but it needs `ss58` + connect as dependencies anyway and adds a third release pipeline. No benefit over A until a second consumer exists.

**Verdict: A.** It's the only option that is true parity (same code, same tests passing) and has a single maintenance home aligned with where the provider now lives.

## 3. Provider-side contract: already in parity

urnet-tools only works if the sn provider still speaks the same protocol. I checked these against `h3-provider/provider/`:

| Surface | 3.23-fix | sn | Status |
|---|---|---|---|
| Control-socket commands (`switch req.Cmd`) | version, get, set, v4, v8, clear, status, history, shutdown, hotswap | identical | ✅ |
| Commands urnet-tools sends | set, clear, get, status, history, hotswap, shutdown, version | all handled | ✅ |
| Docopt usage lines (`provider …`) | 27 | identical (diff empty) | ✅ |
| `control_socket.go` diff (459 lines) | | package rename `main`→`provider`, `tlog`→`controlLog`, comment removal; request struct gains optional `v` (backward-compatible) | ✅ wire-compatible |
| `pending_overrides.go` / `_lock_unix.go` | | 4–5 line diffs (package/comments) | ✅ |
| `.hotswap_declines.json`, `billable_rate`, shmlog `--important` | referenced by urnet-tools | present in sn provider | ✅ |
| Release tarball layout `linux/<arch>/provider` (what `update.go:1114` and installer `:1179` extract) | | local `urnetwork-provider-v2026.9.16-…-meso.tar.gz` matches | ✅ layout, ⚠️ not produced by CI (§5) |

The earlier parity reviews (`OPUS_PARITY_REVIEW.md`, `SONNET_PRODUCTION_READINESS_REVIEW.md`) flagged runtime regressions *inside* `provide()`. Those affect what `urnet-tools status` reports, but they don't block porting the tool.

## 4. Hard-coded 3.23-fix coupling that MUST change in the port

The code compiles and tests pass unchanged, but as-is it would **manage the wrong product**:

| File:line (3.23-fix) | Coupling | Change |
|---|---|---|
| `release.go:64` | `api.github.com/repos/full-bars/urnetwork-3.23-fix/releases/latest` | → sn release repo |
| `release.go:93,129,155` | download + tag URLs | → sn |
| `update.go:692,1352` | provider tarball / tool-asset mirror URLs | → sn |
| `docker.go:19`, `cobra_docker.go` | `ghcr.io/full-bars/urnetwork-3.23-fix` image | → sn image |
| `cobra.go:103`, `cobra_docker.go:81` | help footer URL | → sn |
| installer primary mirror `dl.fullbars.xyz` | serves 3.23-fix releases | needs an sn route, or drop it and use the GitHub mirror only |

**Suggestion:** collect these into one `const releaseRepo = "full-bars/sn"` (plus an image const) in `release.go` rather than doing 9 scattered string edits. The next repo move is then one line. Also add a test asserting that no `urnetwork-3.23-fix` literal remains in non-test code.

**Tag-format check needed:** `update.go` compares running vs. latest versions. sn tags look like `v2026.9.16-1789602650-meso`, while 3.23-fix tags look like `v3.23.0-fix.NN`. Before release, confirm the version-compare and "already on tag" logic in `update.go`/`idle_update.go` orders the new scheme correctly, and that a 3.23 → sn cross-grade isn't rejected as a "downgrade". The trial port did not exercise this, because the tests use fixtures.

## 5. Release pipeline gap (P0, blocks parity)

`h3-provider/.github/workflows/release.yml` currently publishes **loose binaries only**:
`provider-linux-amd64`, `…-arm64`, darwin/windows, and `.asc` signatures.

What the consumers require:

| Asset | Needed by | sn release.yml today |
|---|---|---|
| `urnetwork-provider-<tag>.tar.gz` (with GitHub `sha256:` digest) | `urnet-tools update` (`release.go:100`, refuses without digest), installer `:1166` | ❌ built manually (the untracked tarballs in the repo root), not uploaded by CI |
| `urnet-tools-<os>-<arch>` | installer `:1249`, tool self-update (`update.go` ToolAsset) | ❌ |
| `urnet-docker-<os>-<arch>` | operators | ❌ |

Without the tarball, `urnet-tools update` errors with "has no sha256 digest; refusing unverified download". Without the tool asset, the installer silently falls back to the legacy shell self-copy. Port the `Build urnet-tools` / `Build urnet-docker` / tarball steps from `urnetwork-3.23-fix/.github/workflows/release.yml:59-160`, and GPG-sign the new assets the same way as the provider binaries.

Also re-enable the workflows currently stubbed out with `# DISABLED: requires cmd/urnet-tools/`: `tool-functional-smoke.yml`, `unix-lifecycle.yml`, `functional-soak.yml`, `docker-multi-container.yml`, and `windows-lifecycle.yml`. The last one also needs `cmd/fake-provider/` from 3.23-fix, so port that too.

## 6. urnet-docker has a hidden dependency: the in-container toolkit

`urnet-docker` delegates provider operations with `docker exec <container> urnet-tools …` (`docker_actions.go:14`) and reads in-container files via `docker exec cat` (`docker.go:110`). In 3.23-fix, the container's `urnet-tools` is **`docker/scripts/urnet-tools.sh`** (678 lines), wired up in the `Dockerfile`:

```
COPY docker/scripts/*.sh /app/
RUN ln -sf /app/urnet-tools.sh /usr/local/bin/urnet-tools
RUN ln -sf /app/update_verify.sh /usr/local/bin/update_verify.sh
RUN ln -sf /app/urnetwork_${TARGETARCH}_stable /usr/local/bin/provider
```

sn's new `Dockerfile` (commit `f0190fd0`) only copies `/app/provider` and sets `ENTRYPOINT`. **So a ported `urnet-docker` would build and pass tests, then fail against every sn container.** For parity, also port `docker/scripts/` (~1.7k lines of non-test scripts: `entrypoint.sh`, `start_{stable,nightly,jwt,update}.sh`, `urnet-tools.sh`, `update_verify.sh`, `proxy-health.sh`, `logs.sh`, `stats`, …) and the corresponding Dockerfile lines. The scripts use the same GitHub-repo/ghcr URLs, so they need the same §4 rewrite, and the binary name `urnetwork_${TARGETARCH}_stable` must match what urnettools' discovery expects (`urnetwork_amd`/`urnetwork_arm` patterns appear in urnettools).

## 7. Shell installer (`Provider_Install_Linux.sh`)

**Reusable almost as-is.** Here's what it depends on:

- **Provider CLI:** `provide`, `auth`, `proxy …`. Identical in sn (§3). ✅
- **Install layout:** `~/.local/share/urnetwork-provider/bin/{urnetwork,urnet-tools}`, `~/.urnetwork` state, user unit `urnetwork.service` + `urnetwork-update.timer`. The sn provider still resolves `~/.urnetwork` / `URNETWORK_STATE_DIR`. ✅
- **Tarball layout:** `linux/$arch/provider`. Matches. ✅

**Required edits (all URL/string level, about 6 lines):**

| Line | Current | Change |
|---|---|---|
| 5, 68 | `github.com/full-bars/urnetwork-3.23-fix` (banner/help) | sn repo |
| 102 | `api_base=…/repos/full-bars/urnetwork-3.23-fix` | sn repo |
| 109 | `urnet_install_url` default (raw.githubusercontent …3.23-fix/main) | sn path (keep the `URNET_INSTALL_URL` override) |
| 460, 468 | `releases/latest`, `dl.fullbars.xyz/latest-version` | sn |
| 1166-1167, 1250-1251 | `dl.fullbars.xyz` primary + GitHub mirror downloads | sn |

**Things to reconcile, not just rewrite:**

1. **Unit name mismatch.** The installer and urnettools discovery use `urnetwork.service` (user unit). sn ships `deploy/urnetwork-provider.service` (system unit, `ExecStart=/usr/local/bin/provider provide`, `Type=simple`). Pick one. urnettools' hot-swap needs `Type=notify` (see `provider/hotswap.go:692`), and `urnet-tools update` migrates units to it. Recommend keeping the installer's `urnetwork.service` as canonical, since urnettools discovery is built around it, and either deleting `deploy/urnetwork-provider.service` or making it match.
2. **Cross-grade from 3.23-fix nodes.** Fleet nodes already have the 3.23 layout plus the Go `urnet-tools` that points at the 3.23 repo. Their existing tool would never discover sn releases. Migration needs either one manual `curl …sn/…/Provider_Install_Linux.sh | sh` per node, or a final 3.23-fix release whose `release.go` points at sn. This is an operator decision and should be decided explicitly.
3. **Same tag-ordering check as §4.** The installer's version file (`$install_path/.version`) compares tags too.
4. **Placement:** put it at `h3-provider/scripts/Provider_Install_Linux.sh`, next to the existing release-gate scripts. The directory already exists.

Don't turn the installer into a Go program as part of this work. It's the bootstrap that installs the Go tool (the Go tool then self-updates), so it has to stay POSIX `sh`.

## 8. Porting plan (ordered)

1. **Code port (low risk, verified):**
   - `cp -r urnetwork-3.23-fix/internal/urnettools internal/`, `cp -r cmd/urnet-tools cmd/urnet-docker cmd/`
   - rewrite imports `github.com/urnetwork/connect/internal/urnettools` → `github.com/urfoundation/sn/internal/urnettools`
   - add `sn_ranking.go` shim + `sn_status.go` / test edits (Appendix A)
   - `go get github.com/spf13/cobra@v1.10.2 && go mod tidy`
   - gate: cross-build all 6 targets + `go test ./internal/urnettools ./cmd/...`
2. **De-couple from 3.23-fix (§4):** centralize repo/image constants, add a no-3.23-literal test, verify tag ordering for `v2026.*` tags.
3. **Release pipeline (§5):** tarball + `urnet-tools-*` + `urnet-docker-*` assets with digests and signatures. Port `cmd/fake-provider`, re-enable the 5 disabled workflows.
4. **Installer (§7):** copy, apply the URL edits, reconcile the unit name, decide the 3.23 → sn cross-grade path.
5. **Docker toolkit (§6):** port `docker/scripts/`, extend the Dockerfile, rewrite URLs.
6. **Docs:** `docs/urnet-tools-go.md` (referenced by `wiki-sync.yml`) from 3.23-fix.

Per `CLAUDE.md`, the PRs for steps 2–5 need the `needs-fleet-deploy` label, and step 2/3 (update and digest verification path) warrants `security`.

## Appendix A: `internal/urnettools/sn_ranking.go` (as validated)

```go
package urnettools

import (
	"context"
	"fmt"

	"github.com/urnetwork/connect"
)

// NetworkRanking mirrors the fork-only connect.NetworkRanking, absent in v2026.
type NetworkRanking struct {
	NetMibCount       float64 `json:"net_mib_count"`
	LeaderboardRank   int     `json:"leaderboard_rank"`
	LeaderboardPublic bool    `json:"leaderboard_public"`
}

type NetworkRankingError struct {
	Message string `json:"message"`
}

type NetworkRankingResult struct {
	NetworkRanking *NetworkRanking      `json:"network_ranking,omitempty"`
	Error          *NetworkRankingError `json:"error,omitempty"`
}

// rankingAPI adds GET /network/ranking to a v2026 BringYourApi.
type rankingAPI struct {
	*connect.BringYourApi
	ctx      context.Context
	strategy *connect.ClientStrategy
	apiUrl   string
	byJwt    string
}

func (a *rankingAPI) SetByJwt(jwt string) {
	a.byJwt = jwt
	a.BringYourApi.SetByJwt(jwt)
}

func (a *rankingAPI) NetworkGetRankingSync() (*NetworkRankingResult, error) {
	return connect.HttpGetWithStrategy(
		a.ctx,
		a.strategy,
		fmt.Sprintf("%s/network/ranking", a.apiUrl),
		a.byJwt,
		&NetworkRankingResult{},
		connect.NewNoopApiCallback[*NetworkRankingResult](),
	)
}
```

`sn_status.go` `newSnStatusAPI` body:

```go
strategy := connect.NewClientStrategyWithDefaults(context.Background())
api := &rankingAPI{
	BringYourApi: connect.NewBringYourApi(context.Background(), strategy, apiUrl),
	ctx:          context.Background(),
	strategy:     strategy,
	apiUrl:       apiUrl,
}
api.SetByJwt(byJwt)
return api
```

Possible follow-up: `provider/sn.go` already has an equivalent `NetworkGetRankingSync` + types. If `urnettools` imported them, it would pull the entire `provider` package (and its deps) into the CLI binaries, so the small duplicate is the better trade.
