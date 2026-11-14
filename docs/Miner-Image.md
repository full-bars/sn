# Miner image (sn)

> [!TIP]
> **Most operators can stop here.** You do not need the miner image to get paid. Sign the one-time wallet consent from your own computer (see the [Bittensor Quick Start](/full-bars/sn/wiki/Bittensor-Quick-Start)). This page is for operators who want hotkey delegation, multi-operator mode, claim daemons or the head-tier fleet commands.

`Dockerfile` builds the sn container image, published as `:latest` and, for one release, as the `:miner` alias. Both tags use the same packaging and the same entrypoint; they are built by separate workflows, so `:miner` (moved on pushes to `main`) can point at a different commit than `:latest` (moved on stable tags). It bakes the miner (`cli/miner`)
in as `provider`, the binary whose CLI carries `provide --all-operators`,
`operators`, `auth --operator`, `wallet`, `claim`, `claim-daemon(-v2)` and
`fleet`, plus `urnet-tools`: the Go client
(`urnet-tools-go`) behind a dispatcher that sends `update`/`idle-update` to
the verified shell update wrapper.

The image publishes no ports. The miner dials outbound only. It also serves
Prometheus `/metrics` by default. Inside a container the listener binds all
interfaces, and only a port you publish is reachable from the host.

Published tags: `ghcr.io/full-bars/sn:latest` (and the Docker Hub mirror) is
this image. `:miner` (built from main), `:miner-<tag>` and `:miner-<short-sha>`
are kept as aliases for one release.

> [!WARNING]
> **Upgrading an existing `:latest` container loses the vnStat dashboard.** `:latest` used to be the 3.23-style provider image. Moving to this image
> drops the vnStat dashboard on port 8080 (`ENABLE_VNSTAT`), `TURBO`,
> `ENABLE_IP_CHECKER`, `PELICAN=yes` panel mode and `BUILD=nightly`. The legacy
> `BUILD=stable` and `BUILD=jwt` are still accepted and mapped to
> `AUTH=userpass` and `AUTH=authcode` with a deprecation warning; an explicit
> `AUTH` wins over `BUILD`.

## Quick start

> **Discovery uses the image repository.** The sn `urnet-docker` client finds provider containers by the repository of their image. The container name does not matter. `ghcr.io/full-bars/sn` and `docker.io/3cape/sn` are on the default list. For a locally built image, add its repository to `URNET_DOCKER_IMAGE_REPOS` (a comma-separated list) before you run the tool. `docker start|stop|logs` work without discovery.

```bash
docker run -d --name urnet-miner \
  -v sn_miner_data:/root/.urnetwork \
  -e AUTH=authcode \
  -e AUTHCODE='YOUR_AUTH_CODE_HERE' \
  -e ALL_OPERATORS=Y \
  -e ALLOW_CLIENT_REGISTRATION=Y \
  ghcr.io/full-bars/sn:miner
```

A single-operator node that signs in for one operator and mines the listed
operators:

```bash
docker run -d --name urnet-miner \
  -v sn_miner_data:/root/.urnetwork \
  -e AUTH=authcode \
  -e AUTHCODE='YOUR_AUTH_CODE_HERE' \
  -e OPERATOR=example.com \
  -e ALL_OPERATORS=Y \
  -e ALLOW_CLIENT_REGISTRATION=Y \
  ghcr.io/full-bars/sn:miner
```

With a hotkey seed, `--auto-register` signs in operators that have no session
yet (the seed file must be mode 600 and 32 raw bytes or 64 hex characters):

```bash
  -v /path/to/hotkey.seed:/app/hotkey.seed:ro \
  -e HOTKEY_SEED_FILE=/app/hotkey.seed \
  -e AUTO_REGISTER=Y
```

The miner publishes no ports. Added capabilities are only needed if you want
the provider to forward client traffic, in which case add `--cap-add NET_ADMIN`,
`--cap-add NET_RAW` and `--sysctl net.ipv4.ip_forward=1`.

## Environment

| Variable | Meaning |
|---|---|
| `AUTH` | `userpass` (with `USER_AUTH` and `PASSWORD`) or `authcode` (with `AUTHCODE`). |
| `AUTHCODE` | One-time auth code. Alias of `URNETWORK_AUTH_CODE`. |
| `OPERATOR` | Operator domain for `auth --operator=<domain>` and the per-operator session path. |
| `HOTKEY_SEED_FILE` | sr25519 hotkey seed for operator sign-in and `--auto-register`. |
| `ALL_OPERATORS` | `Y` passes `provide --all-operators` (mines every listed operator). |
| `AUTO_REGISTER` | `Y` passes `--auto-register` (requires `ALL_OPERATORS=Y` and a seed). |
| `ALLOW_CLIENT_REGISTRATION` | Unset or empty defaults to `auto`, which permits unattended `provide` to register a missing identity. `Y` also permits registration; `N` withholds permission unless an explicit flag grants it. Preserve and restore existing state instead of registering again when an existing identity is missing. |
| `OPERATORS_URL` | Operator list URL for `provide --all-operators` and `auth --operator`. |
| `MAX_MEMORY` | Passed through as `--max-memory` (for example `1500mib`). |
| `PROVIDER_ARGS` | Extra flags appended to `provide` verbatim. |
| `PROXY_FILE` | Proxy file added with `provider proxy add --proxy_file` at startup (default `/app/proxy.txt`). |
| `URNETWORK_STATE_DIR` | State directory override; defaults to `$HOME/.urnetwork`. |

`BUILD` is deprecated but still accepted. `BUILD=stable` maps to
`AUTH=userpass` and `BUILD=jwt` to `AUTH=authcode`, each with a deprecation
warning; `BUILD=nightly` has no equivalent and is ignored. When `BUILD` and
`AUTH` are both set and would select different paths, the conflict is logged
and `AUTH` wins.

## Startup behavior

1. `AUTH` is validated fail-closed: a missing `USER_AUTH`/`PASSWORD` or a
   missing auth code names the variable and exits 1.
2. A stored session is checked locally (`exp`/`nbf`, 5 minute skew). Only a
   definitely expired session is removed, and only when a credential exists to
   replace it; anything unreadable is left for the miner to report.
3. The state directory must be owned by the container user (root with the
   default image): a bind mount created by another host UID is refused by the
   custody checks with a bare `startup_recovery_required` diagnostic. An empty,
   unmounted state directory logs a new-identity warning, and one wallet
   reminder line is always logged.
4. Authentication runs once when no session exists. The default `auto` permits `provide` to register a missing identity without a prompt. If an existing identity is missing, stop and restore its state volume instead of registering a replacement. Then `provide` runs under a
   small supervisor: exit 75 restarts after 5s, exit 78 clears the session and
   re-authenticates (up to 3 times), exit 79 parks the container (it stays up
   but makes no further login attempt) until the credentials are fixed, and
   other crashes exit after 3 attempts so the container or systemd restart
   policy takes over.

## Inside the image

| Path | What it is |
|---|---|
| `/app/urnetwork_<arch>_stable`, `provider` | the miner (the program whose usage is `provider ...`) |
| `/app/urnet-tools.sh`, `urnet-tools-sh` | the verified shell update wrapper (`do_update`: release digest check, `update-pending` marker, respawn verification, Pelican refusal) |
| `urnet-tools` | dispatcher: `update`/`idle-update` go to the shell wrapper, every other command goes to the Go client |
| `urnet-tools-go` | the Go client |
| `proxy-health`, `proxy-traffic`, `logs` | state-file helpers on PATH |
| `/app/{entrypoint,start_miner,provider_env,jwt_verify,update_verify}.sh` | startup scripts and their helpers |

## Updating the miner

`urnet-docker update <container>` works against a miner container: it
sed-patches `/app/urnet-tools.sh` from the host and runs `docker exec <c>
urnet-tools update`, which the dispatcher routes to the shell wrapper. The
wrapper refuses to install a release binary that is not the program this image
runs. This fork ships one program: the platform archives carry it as `provider`
and the miner archives carry the same binary as their only member, so the check
is a guard against a future archive holding something else rather than a
restriction on today's releases.

Update a miner container by pulling and recreating it, or let Watchtower do
it; the Quick start commands are safe to re-run against an existing state
volume. The volume keeps the identity, so no auth code is consumed again.

## Migration from 3.23-fix

The state directory is the same path (`/root/.urnetwork`, or
`$URNETWORK_STATE_DIR`) but the credential files are not. 3.23-fix keeps
`.provider.key` and `.client_jwts.json`; the sn miner keeps `.provider.jwt` and
`.provider-<slot>.jwt` per slot. Mounting a 3.23-fix volume into this image
has an automatic import path for those identities. The miner proves the retained key against the server before it writes the per-slot credentials. If the proof fails for a slot, the miner registers a new client for that slot. Treat a copied volume as unverified for identity reuse until you check the client ids after the first start. Rolling back to 3.23-fix on the original files keeps working, because nothing in this image writes 3.23-fix state.

## Building locally

```bash
docker build -t urnetwork-miner:local .
docker run --rm --network none -e AUTH=userpass urnetwork-miner:local    # fails closed, exit 1
```

The sn `urnet-docker` client discovers a container by the repository of its
image, so a local image such as `urnetwork-miner` is discovered only when you
add its repository to `URNET_DOCKER_IMAGE_REPOS` (see Quick start).

The entrypoint contract is covered by `docker/miner/test_miner_entrypoint.sh`
(stub provider, offline).
