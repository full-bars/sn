# Miner image (sn)

`Dockerfile.miner` builds the sn miner image. It bakes the miner (`cli/miner`)
in as `provider`, the binary whose CLI carries `provide --all-operators`,
`operators`, `auth --operator`, `wallet`, `claim`, `claim-daemon(-v2)` and
`fleet`. It also bakes the relay (`cmd/provider`) as `provider-relay` for the
proxy surface, direct mode and sn-status, plus `urnet-tools`: the Go client
(`urnet-tools-go`) behind a dispatcher that sends `update`/`idle-update` to
the verified shell update wrapper.

The image publishes no ports and runs no status server. The miner dials
outbound only.

Published tags: `ghcr.io/full-bars/sn:miner` (built from main), `:miner-<tag>`
for a version tag, and `:miner-<short-sha>`. The relay image keeps `:latest`
and the Docker Hub mirror, so nothing about the existing relay deployments
changes.

## Quick start

```bash
docker run -d --name sn-miner \
  -v sn_miner_data:/root/.urnetwork \
  -e AUTH=authcode \
  -e AUTHCODE='YOUR_AUTH_CODE_HERE' \
  -e ALL_OPERATORS=Y \
  ghcr.io/full-bars/sn:miner
```

A single-operator node that signs in for one operator and mines the listed
operators:

```bash
docker run -d --name sn-miner \
  -v sn_miner_data:/root/.urnetwork \
  -e AUTH=authcode \
  -e AUTHCODE='YOUR_AUTH_CODE_HERE' \
  -e OPERATOR=example.com \
  -e ALL_OPERATORS=Y \
  ghcr.io/full-bars/sn:miner
```

With a hotkey seed, `--auto-register` signs in operators that have no session
yet (the seed file must be mode 600 and 32 raw bytes or 64 hex characters):

```bash
  -v /path/to/hotkey.seed:/app/hotkey.seed:ro \
  -e HOTKEY_SEED_FILE=/app/hotkey.seed \
  -e AUTO_REGISTER=Y
```

No published ports and no added capabilities are required for relaying. If you
run the baked `provider-relay` in direct mode, give it the relay image's flags
(`--cap-add NET_ADMIN --cap-add NET_RAW`, `--sysctl net.ipv4.ip_forward=1`).

## Environment

| Variable | Meaning |
|---|---|
| `AUTH` | `userpass` (with `USER_AUTH` and `PASSWORD`) or `authcode` (with `AUTHCODE`). |
| `AUTHCODE` | One-time auth code. Alias of `URNETWORK_AUTH_CODE`. |
| `OPERATOR` | Operator domain for `auth --operator=<domain>` and the per-operator session path. |
| `HOTKEY_SEED_FILE` | sr25519 hotkey seed for operator sign-in and `--auto-register`. |
| `ALL_OPERATORS` | `Y` passes `provide --all-operators` (mines every listed operator). |
| `AUTO_REGISTER` | `Y` passes `--auto-register` (requires `ALL_OPERATORS=Y` and a seed). |
| `ALLOW_CLIENT_REGISTRATION` | `auto` (default) omits the flag, `Y` passes `--allow-client-registration`, `N` omits it. |
| `OPERATORS_URL` | Operator list URL for `provide --all-operators` and `auth --operator`. |
| `MAX_MEMORY` | Passed through as `--max-memory` (for example `1500mib`). |
| `PROVIDER_ARGS` | Extra flags appended to `provide` verbatim. |
| `PROXY_FILE` | Proxy file added with `provider proxy add --proxy_file` at startup (default `/app/proxy.txt`). |
| `URNETWORK_STATE_DIR` | State directory override; defaults to `$HOME/.urnetwork`. |

`BUILD` is not supported by this image and logs a warning if set.

## Startup behavior

1. `AUTH` is validated fail-closed: a missing `USER_AUTH`/`PASSWORD` or a
   missing auth code names the variable and exits 1.
2. A stored session is checked locally (`exp`/`nbf`, 5 minute skew). Only a
   definitely expired session is removed, and only when a credential exists to
   replace it; anything unreadable is left for the miner to report.
3. An empty, unmounted state directory logs a new-identity warning, and one
   wallet reminder line is always logged.
4. Authentication runs once when no session exists, then `provide` runs under a
   small supervisor: exit 75 restarts after 5s, exit 78 clears the session and
   re-authenticates (up to 3 times), other crashes exit after 3 attempts so the
   container or systemd restart policy takes over.

## Inside the image

| Path | What it is |
|---|---|
| `/app/urnetwork_<arch>_stable`, `provider` | the miner (the program whose usage is `provider ...`) |
| `/app/urnetwork_<arch>_relay`, `provider-relay` | the relay |
| `/app/urnet-tools.sh`, `urnet-tools-sh` | the verified shell update wrapper (`do_update`: release digest check, `update-pending` marker, respawn verification, Pelican refusal) |
| `urnet-tools` | dispatcher: `update`/`idle-update` go to the shell wrapper, every other command goes to the Go client |
| `urnet-tools-go` | the Go client (the `urnet-tools` of the relay image) |
| `proxy-health`, `proxy-traffic`, `logs` | state-file helpers on PATH, as in the relay image |
| `/app/{entrypoint,start_miner,provider_env,jwt_verify,update_verify}.sh` | startup scripts and their helpers |

## Updating the miner

`urnet-docker update <container>` works against a miner container: it
sed-patches `/app/urnet-tools.sh` from the host and runs `docker exec <c>
urnet-tools update`, which the dispatcher routes to the shell wrapper. The
wrapper refuses to install a release binary that is not the program this
image runs: full-bars/sn releases publish the relay as `provider`, while the
miner ships only inside this image, so an in-place swap would replace the
miner with the relay. It points at the image stream instead.

Update a miner container by pulling and recreating it, or let Watchtower do
it; the Quick start commands are safe to re-run against an existing state
volume. The volume keeps the identity, so no auth code is consumed again.

## Migration from 3.23-fix

The state directory is the same path (`/root/.urnetwork`, or
`$URNETWORK_STATE_DIR`) but the credential files are not. 3.23-fix keeps
`.provider.key` and `.client_jwts.json`; the sn miner keeps `.provider.jwt` and
`.provider-<slot>.jwt` per slot. Mounting a 3.23-fix volume into this image
does not reuse those identities today: treat a copied volume as unverified for
identity reuse, because the import path is still being measured and no claim
that a client id survives has been proven. A fresh volume mints a new identity.

Do not point a production node at a copied 3.23-fix volume expecting the same
client id until the automatic import lands and has been verified against the
real server. Rolling back to 3.23-fix on the original files keeps working,
because nothing in this image writes 3.23-fix state.

## Building locally

```bash
docker build -f Dockerfile.miner -t sn-miner .
docker run --rm --network none -e AUTH=userpass sn-miner    # fails closed, exit 1
```

The entrypoint contract is covered by `docker/miner/test_miner_entrypoint.sh`
(stub provider, offline).
