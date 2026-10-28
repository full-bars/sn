# Operator task map

This maps the 49 tasks in `.worktree-docs/operator-tasks.md` to pages in this tree. A page is the nearest existing landing point, not necessarily a good answer. `runnable` means the page contains a complete-looking sequence consistent with the checked help and source; it is not a claim of a live deployment test. `reference` has commands or descriptions without a task sequence; `partial` has a sequence with a gap; `missing` has no answer; `stale` makes a concrete claim contradicted by the checked binary or source. A stale statement takes precedence over an otherwise useful sequence.

Audit basis: tree `92cad8ba6cab42be2bc2db043ead0ca90f13611a`, `internal/urnettools/` and relevant `miner/` code, plus help from `/home/klets/.local/bin/urnet-tools` v2026.10.22-1078667780. There was no `./urnet-tools` in this worktree. The installed `urnetwork --help` is from the older relay line (it lists `bind-head`); this tree builds the miner-backed provider, so installed help and current source are not the same release. Live operation, installer downloads, third-party endpoints, OS-specific execution and published wiki search results are **unverified**. Rows describe what a reader can find in this checkout, not search-engine ranking.

## Tasks

| Task | Page today | State | What is missing |
|---|---|---|---|
| Install on systemd, launchd, OpenRC and FreeBSD rc | [Installation](Installation) | partial | Links to [OpenRC](OpenRC) and [FreeBSD](FreeBSD) exist; end-to-end installation on each OS is unverified. |
| Install in Docker | [Docker Deployment](Docker-Deployment) | partial | Run and Compose examples lack a common post-start verification sequence; image and deployment unverified. |
| Sign in with an auth code (`auth`, interactive paste) | [Installation](Installation) | partial | It says `urnetwork auth` but does not explain the prompt or verify sign-in; top-level and `auth --help` disagree about no-code behavior. |
| Choose API and connect endpoints (`choose-network`) | [urnet-tools-go](urnet-tools-go) | reference | No sequence to choose main/beta or custom endpoints, reset and check the result. |
| Name the node for the dashboard (`rename`) | [Node Identity](Node-Identity) | stale | The miner persists the setting but does not publish a dashboard label; the page's general rename instructions promise one. |
| Export and import an identity plus proxy state (`session save`, `session load`) | [urnet-tools-go](urnet-tools-go) | partial | The bundle file list omits miner custody files including `.provider.key.identity`; complete miner identity migration is unverified. |
| Keep several providers straight (`--unit`, `--user`, `--network`, `--state-dir`, `default`, `providers`) | [urnet-tools-go](urnet-tools-go) | stale | It says root with multiple providers must name a target or pass `--all`; `update` can offer an interactive picker. |
| Start, stop, restart (`start`, `stop`, `restart`, `-y`/`-f`) | [Installation](Installation) | partial | Commands are shown but the restart confirmation, noninteractive flags and how to check the result are not a single sequence. |
| Update provider and tool (`update`, `self-update`, `auto-update --interval`) | [urnet-tools-go](urnet-tools-go) | reference | Lifecycle table lacks a complete update, tool-update, schedule and verification workflow. |
| Upgrade with no gap (`hotswap`) | [HotSwap](HotSwap) | partial | The page describes `update` rather than the explicit `hotswap` command, and its v3.23 eligibility/version rules are not verified for this miner tree. |
| Read status at a glance (`status`, `dashboard`, `top`) | [urnet-tools-go](urnet-tools-go) | partial | `status` and `dashboard` are described, but there is no `top` walkthrough or interpretation of the three views. |
| Read and follow logs (`logs [target] [N]`) | [urnet-tools-go](urnet-tools-go) | reference | It describes the follower but provides no example selecting a provider and line count. |
| Find what changed (`history`, `autopilot log`, `enforcement`) | [urnet-tools-go](urnet-tools-go) | partial | `history` is described; `autopilot log` and `enforcement` are absent from the task flow. |
| Record before an upgrade and compare after (`baseline show\|mark\|compare`) | [Baseline](Baseline) | reference | The command table describes each operation, but does not present the before/update/after sequence as a walkthrough. |
| Uninstall and reinstall cleanly (`uninstall`, `reinstall`) | [Installation](Installation) | partial | Uninstall paths are shown, but no joined backup, uninstall, reinstall and verification sequence explains state loss. |
| Turn auto-start on or off (`auto-start`) | [FreeBSD](FreeBSD) | reference | FreeBSD names on/off; no cross-platform sequence checks the resulting boot behavior. |
| Bulk add from a file (`proxy add <file>`) | [Adding Proxies](Adding-Proxies) | runnable | — |
| Paste proxies from stdin, a file or a URL (`proxy paste`) | [Adding Proxies](Adding-Proxies) | partial | Interactive paste and a pipe are shown; file and URL input forms are not a complete walkthrough. |
| Add and remove a URL source (`proxy add-source`, `proxy remove-source`) | [Proxy URL Sources](Proxy-URL-Sources) | partial | Both commands are shown, but the page does not provide a complete add, inspect and remove verification sequence. |
| Refresh and force a refresh (`proxy refresh [--force]`) | [Adding Proxies](Adding-Proxies) | runnable | — |
| See dead or degraded proxies (`proxy health`) | [Proxy Management](Proxy-Management) | partial | Shows the report and states but does not walk from a degraded result to diagnosis and verification. |
| Hold the running count at N (`proxy trim <N>`, `proxy trim off`) | [Proxy Management](Proxy-Management) | runnable | — |
| Remove one, many or all proxies (`proxy remove`, `proxy clear`) | [Proxy Management](Proxy-Management) | partial | Pattern removal is shown; explicit single-address removal and complete clear/verification are not joined into a sequence. |
| Understand a rotation on a live node (proxy hot reload) | [Proxy Hot-Reload](Proxy-Hot-Reload) | runnable | — |
| Raise throughput on a RAM-rich box (`turbo v4\|v8\|off`) | [urnet-tools-go](urnet-tools-go) | reference | No choose, apply, restart-impact and verify sequence. |
| Pick a profile from hardware (`auto on\|off`) | [urnet-tools-go](urnet-tools-go) | reference | No on/off and verification sequence or restart-impact warning alongside the commands. |
| Cut memory on a small box (`eco`, `lowmode`, `profile <name>`) | [Configuration](Configuration) | partial | Profile choices are described but the CLI sequence, restart impact and verification are split or absent. |
| Apply golden-fleet OS and kernel limits (`optimize`) | [FreeBSD](FreeBSD) | partial | A FreeBSD sequence exists; a single operator path for other supported OSes and checking the applied limits is missing. |
| Show or change a runtime override (`set`, `get`, `config`) | [Configuration](Configuration) | partial | Shows set/clear and log verification but does not show `get` and `config` in the same workflow. |
| Keep logs off disk (`ramlogs on\|off`) | [Configuration](Configuration) | reference | The setting is listed but an on/off, restart and log-follow verification sequence is missing. |
| Bypass the auth rate limiter (`fast-auth`) | [urnet-tools-go](urnet-tools-go) | reference | The flag is listed without a status/on/off sequence or warning about the bypass. |
| Expose Prometheus metrics (`metrics on\|off\|listen`) | [Monitoring](Monitoring) | partial | Shows controls and stack setup but not a safe end-to-end scrape verification for the chosen address. |
| Adapt transport choice (`smart-dialer on\|off`) | [Configuration](Configuration) | reference | Explains live application but gives no on/off/status and verification sequence. |
| Auto-regulate proxies under load (`self-heal on\|off`) | [Proxy Management](Proxy-Management) | runnable | — |
| Show the public IP on the dashboard label (`show-ip`) | [urnet-tools-go](urnet-tools-go) | stale | It promises a dashboard label, but the miner does not publish node labels; a dashboard result is not established. |
| Provide on the direct/local IP (`direct on\|off`) | [urnet-tools-go](urnet-tools-go) | reference | The toggle is listed without a sequence to change and verify the active direct slot. |
| Set the report URL (`report`) | — | missing | Installed `urnet-tools report --help` returns unknown command; current miner tests say old report settings are ignored. |
| Read traffic accounting (`usage`, `usage graphs <view>`) | [urnet-tools-go](urnet-tools-go) | reference | Lists usage but does not show how to select and interpret a graph view. |
| Something is wrong and I do not know where to start | [Troubleshooting](Troubleshooting) | partial | Symptom table lacks a first-pass status/log collection sequence and branching from unknown symptoms. |
| Provider will not stay up | [Troubleshooting](Troubleshooting) | partial | Container exit-code steps exist, but there is no equivalent end-to-end native-service workflow. |
| Proxies are all dead | [Troubleshooting](Troubleshooting) | partial | Health and pruning are named, but box-wide/network failure is not separated from bad individual proxies before removal. |
| Activation is refused or held | [Troubleshooting](Troubleshooting) | partial | Auth failures are covered; activation refusal or held status has no dedicated diagnostic path. |
| Node is running but not being used | [Troubleshooting](Troubleshooting) | missing | No path distinguishes running, control-connected, idle and actually serving traffic. |
| State file is damaged and what the provider does | [Hot-Restart](Hot-Restart) | partial | Explains a corrupt client JWT store, not a file-by-file recovery and preservation path for other state. |
| Collect an incident bundle for support | [Troubleshooting](Troubleshooting) | missing | No collection, redaction and secure transfer procedure or bundle command is documented. |
| Subnet 25 status (`sn-status`) | [Bittensor Operations](Bittensor-Operations) | partial | Shows status and JSON and warns about consent, but does not walk through interpreting a result or verifying consent. |
| What `-n/--dry-run` guarantees | [urnet-tools-go](urnet-tools-go) | partial | Mentions individual dry runs but lacks command-specific scope and limitations; update planning can omit the restart/hotswap decision. |
| What `-f/--force` does and does not do | [urnet-tools-go](urnet-tools-go) | stale | It says force only skips confirmations; `proxy refresh --force` also bypasses the warmup gate. |
| No target on a box with multiple providers | [urnet-tools-go](urnet-tools-go) | stale | Its unconditional root refusal rule omits the interactive `update` picker; a noninteractive run can use a persisted default. |

## Three entry paths

First install should land on [Installation](Installation), with explicit links to [OpenRC](OpenRC), [FreeBSD](FreeBSD) and [Docker Deployment](Docker-Deployment). Today [Home](Home) links to Installation and Docker, but leads with a Docker quickstart and does not link the two native platform guides. The missing bridge is a platform choice, authentication and a shared working-result check.

Operating a live node should land on an operations task index rooted in [urnet-tools-go](urnet-tools-go). Today Home links the command reference, but a reader must find the relevant command in a long table and separately discover [Baseline](Baseline) and [HotSwap](HotSwap), neither linked from Home. The missing bridge is a task-first route from status to maintenance, proxies and tuning with verification.

Troubleshooting a live node should land on [Troubleshooting](Troubleshooting). Today it is under Home's Reference section after the Docker quickstart, not an incident entry path. The missing bridge is an unknown-symptom starting sequence, followed by native/container branches and safe support collection.

## Five holes that matter most

1. **“How do I back up and move my miner identity?”** Today the session examples claim identity export, but the tool's explicit bundle list omits miner custody files; a complete migration is unverified and should not be promised.
2. **“Why is my provider running but not getting traffic?”** Today Troubleshooting has no flow distinguishing process liveness, control connection, idle capacity and actual traffic.
3. **“How do I change my node name on the dashboard?”** Today Node Identity shows `rename`, while current miner code says the setting is persisted but not sent to the dashboard.
4. **“Why are all my proxies dead?”** Today Troubleshooting points at health and removal without a box-wide/network-failure branch before pruning.
5. **“How do I collect a support bundle without leaking credentials?”** Today Troubleshooting has no collection or redaction procedure and the checked help exposes no bundle command.

## Pages to merge or delete

- **Merge `urnet-tools-Go.md` into `urnet-tools-go.md`, then delete the uppercase duplicate.** The two 245-line pages differ in the trim reference and can drift; Home and Sidebar use the lowercase name. Review links and case-sensitive wiki behavior before deletion.
- **Merge `urnet-docker.md` into `Docker-Deployment.md`, then delete the separate reference after preserving unique command details.** Both cover the host tool; one maintained entry avoids conflicting update and target instructions.
- **Merge `Adding-Proxies.md` into `Proxy-Management.md`, retaining the platform-specific first-add and paste examples.** Both cover add, refresh, removal and health; one task-oriented page can own the sequence. Keep [Proxy URL Sources](Proxy-URL-Sources) and [Proxy Hot-Reload](Proxy-Hot-Reload) as linked deep dives.
- **Remove or rewrite the relay-only dashboard promise in `Node-Identity.md` before using it as the miner's naming guide.** Current miner code persists the name without publishing it; the warning alone does not make the subsequent promise true.

These are recommendations only; no existing pages are changed or deleted in this mapping job.

## Landing page outline

Current [Home](Home) has an index and Docker quickstart, but does not organize the three paths in this order or provide the bridges above.

- First install: choose systemd, launchd, OpenRC, FreeBSD rc or Docker and link the matching install guide.
- First install: link authentication, proxy setup and a first-result check.
- Live node: link a task-first operations index for status, logs, updates, proxies and tuning.
- Live node: link baseline and hotswap with their operational caveats.
- Troubleshooting: link an unknown-symptom starting point and native/container branches.
- Troubleshooting: link support and a credential-safe incident collection procedure once one exists.

## Evidence and limits

- Help was captured read-only for all named command families. Some nested `--help` calls return the parent help, not dedicated subcommand detail; `auth --help` says no code hashes stored identity while top-level help says interactive paste. The actual no-code interactive path is **unverified**; no authentication was attempted.
- `internal/urnettools/session_cmds.go` enumerates the archived files; `miner/provider_registration_cause_test.go` names retained custody files not present in that enumeration. Whether a particular migrated miner can recover its full identity from this bundle is **unverified**.
- `internal/urnettools/cobra.go` states the miner does not send its node name; `internal/urnettools/proxy.go` passes force through to refresh, and `miner/proxy_manager_commands.go` implements the refresh gate. `miner/hub_report_removed_test.go` checks that old report settings are ignored.
- No live installs, provider changes, destructive commands, authentication, external service tests or support submissions were performed. Platform parity, actual runtime outcomes and current published wiki/search behavior remain **unverified**.
