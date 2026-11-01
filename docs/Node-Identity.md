# Node Identity

Node labels are **not implemented in this line.** Nothing in the tree reads `URNETWORK_NODE_NAME`, and no label reaches the dashboard, the auth handshake or the health heartbeat.

> [!NOTE]
> The miner does not publish a node name. It accepts and persists
> `node_name` (and `urnet-tools rename`) so 3.23-fix tooling keeps working, but
> its client description is `provider <os> <version>` and no label reaches the
> dashboard or the health heartbeat. Neither a live set nor a restart changes
> that (`miner/control_socket.go`, `controlNoEffectKeys`). See
> [Miner Image](/full-bars/sn/wiki/Miner-Image) for the miner.

The `client_id` the platform assigns to each slot is separate from any label. Those ids, and how they are kept stable across restarts by the provider and by the miner, are covered in [Hot-Restart.md](/full-bars/sn/wiki/Hot-Restart).

## Setting the node name

Not implemented in this line. Setting `URNETWORK_NODE_NAME` has no effect on the auth handshake or the health heartbeat, because nothing reads it.

## Default label

Not implemented in this line. There is no label to fall back to. Operators who need to tell nodes apart must do so by container name or client identity, not by this variable.

## Rename at runtime

Not implemented in this line. The `node_name` control key is accepted and persisted, but nothing applies it, so renaming changes nothing (`miner/control_socket.go`, `controlNoEffectKeys`).