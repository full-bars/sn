# Node Identity

Each provider has a label. Use it to tell nodes apart in logs and in your Grafana dashboards.

> [!NOTE]
> The miner does not publish a node name. It accepts and persists
> `node_name` (and `urnet-tools rename`) so 3.23-fix tooling keeps working, but
> its client description is `provider <os> <version>` and no label reaches the
> dashboard or the health heartbeat. Neither a live set nor a restart changes
> that. Everything below applies to the relaying provider; see
> [Miner Image](/full-bars/sn/wiki/Miner-Image) for the miner.

The label is separate from the `client_id` the platform assigns to each slot. Those ids, and how they are kept stable across restarts by the provider and by the miner, are covered in [Hot-Restart.md](/full-bars/sn/wiki/Hot-Restart).

## Setting the node name

Set `URNETWORK_NODE_NAME` before starting the provider:

```sh
export URNETWORK_NODE_NAME=chicago-honk
provider provide ...
```

The name is trimmed and used for:

- the auth handshake identity,
- the health heartbeat label.

The heartbeat also resolves the node name through the runtime override (`urnet-tools rename <name>`), falling back to the startup environment value.

## Default label

When no explicit name is set, the label falls back to the startup value, which by default is the hostname. Operators running containers and native binaries on the same host typically set `URNETWORK_NODE_NAME` explicitly per deployment so the labels stay distinguishable.

## Rename at runtime

Change the label without restarting:

```sh
urnet-tools rename my-new-name
urnet-tools rename off    # revert to the startup default (hostname)
```