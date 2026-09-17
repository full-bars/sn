# urnet-docker (Host-Side Tool)

`urnet-docker` is the host-side CLI for managing provider Docker containers. It lives on the host, not inside a container. It identifies containers by the JWT inside them, runs commands through `docker exec`, and can update its own binary.

Install with the one-line installer:

```bash
curl -fSsL https://dl.fullbars.xyz/urnet-docker.sh | sh
# GitHub fallback: curl -fSsL https://raw.githubusercontent.com/full-bars/sn/main/scripts/install-urnet-docker.sh | sh
```

## Commands

| Command | What it does |
|---|---|
| `providers` (`list`, `ps`) | List all provider containers, identified by the in-container JWT. |
| `status [target]` | Detailed status of one container. |
| `exec [target flags] [--] <cmd...>` | Run a command inside the container. Target flags must come before the command. Use `--` to forward inner flags verbatim. Example: `urnet-docker exec --unit <name> -- urnet-tools proxy add --proxy_file=/tmp/p.txt`. |
| `proxy add <file>` | Add proxies from a host proxy file. Copies the file into the container, then adds via the in-container tool. Example: `urnet-docker proxy add ~/proxies.txt`. |
| `proxy paste` | Paste raw proxies from stdin, file, or URL. |
| `proxy trim <N\|off>` | Set a persistent hard cap of `<N>` running proxies in the targeted container. Sheds worst-graded (F to A) proxies first. `proxy trim off` clears the cap. |
| `proxy clear` | Remove all proxies for the targeted container. |
| `proxy remove [addresses...] [--all]` | Remove specific proxies, or every proxy with `--all`. |
| `proxy add-source <url>` | Add a URL proxy source. |
| `proxy remove-source <url>` | Remove a URL proxy source. |
| `proxy refresh [--force]` | Re-read the proxy source without restarting. `--force` bypasses warmup lockout. |
| `proxy remove-dead` | Remove dead or degraded proxies. |
| `proxy health` | Show dead/degraded proxies and the live health event log. |
| `proxy traffic` | Real-time bandwidth and client session load. |
| `proxy summary` | Per-proxy activity and performance summary. |
| `restart [target]` | Restart the container. |
| `update` (`self-update`) | Update the tool binary itself. Containers update by image pull, not by this command. Host-side only; never run inside a container. |
| `logs [target] [N]` | Follow the container logs (last N lines, default 250). RAMLOGS-aware: streams `/dev/shm` when RAM logs are enabled, else `docker logs`. Uses an interactive picker when multiple providers exist. |
| `version` | Print the tool version. |

## Targeting

Targeting flags are required when more than one provider container exists (both `--flag value` and `--flag=value` formats supported):

- `--unit <name>`: container name (mapped to the unit).
- `--user <user>`: container identity (docker discovery sets this to `docker:<containerName>`).
- `--network <name>`: JWT network name.
- `--n <id>`: JWT network id, the true unique identity. Use when two providers share the same network name.
- `--state-dir <dir>`: provider state directory.

One container needs no flag. Multiple containers with no target is refused, never silently guessed.

## Relation to urnet-tools

The two tools split the provider world by how the provider runs:

- `urnet-tools` manages **systemd or native** providers on the host.
- `urnet-docker` manages **containerized** providers.

The same targeting concepts and safety model apply to both. See [urnet-tools (Go)](urnet-tools-Go) for the full tool reference.

## Safety

- `exec` refuses to run inside a container. It always operates from the host.
- `proxy` subcommands use the same targeting as `exec`: one container = automatic; multiple containers = interactive picker (requires a TTY) or explicit target flags (`--unit`, `--network`, etc.). Without a TTY and with multiple containers, the command refuses rather than guessing.
- Targeting is explicit. A multi-container host with no target is refused.
- `update` is host-side only. It never touches containers.