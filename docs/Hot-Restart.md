# ♻️ Hot Restart: Client Identity Reuse

Every connection slot authenticates with the platform once and receives a `client_id`. Hot restart keeps that signed client JWT on disk and reuses it across restarts, so a restart comes back as the same client instead of a brand new one.

This fork carries two identity paths: the relay provider (`provider/`) and the Subnet 25 miner (`miner/`). Both reuse identities by default, and both are controlled by the same `hot_restart` key, but they store the credentials differently. This page covers each.

## Why reuse exists

- The `client_id` is what the platform tracks a connection under. A new id on every restart means a new client row each time, no history, and an auth burst at startup on a box that walks a large proxy list.
- Reuse keeps the identity, and the reliability and location data attached to it, stable across restarts and updates.
- A restart with reuse in place does not call `/network/auth-client` at all while the stored token is still valid.

## Who owns the id

`client_id` and `device_id` are minted by the platform, not by the provider or the miner. Nothing here generates, copies, or transfers an id to another host. The stored token the platform issued is presented again, and when it has expired the platform is asked to re-sign the same client row by sending the existing `ClientId` to `/network/auth-client`. The server updates the existing `network_client` and `device` rows and returns a JWT carrying the same `client_id` and `device_id`.

## The toggle

Reuse is **on by default**. The setting resolves in this order, for the provider and for the miner:

1. Control state key `hot_restart`, when set (`on` or `off`).
2. Environment variable `URNETWORK_HOT_RESTART`, where any value other than `0` means on.
3. Default: on.

An unknown stored value falls back to on rather than silently disabling reuse after a bad write.

## Checking the current state

```bash
# The effective override, and whether it is set at all
urnet-tools set hot-restart
# hot_restart: not set for urnetwork.service (using startup default)

# Client ids held in the provider's identity store
urnet-tools proxy ids
# PROXY            CLIENT_ID                           NETWORK_ID  AGE
# (direct)         4f2c8d1a9b7e3c5061d2e8f4a7b9c013    9a1b3c7e…   6h12m
# 10.0.0.4:8080    91bd0e5f2a6c48d73b19e0a4c8f72d55    9a1b3c7e…   6h12m
```

`proxy ids` reads `.client_jwts.json` directly, so it works while the provider is stopped. Bearer tokens are never printed, only the assigned ids. It does not enumerate the miner's per-slot credential files; see [The miner line](#the-miner-line).

Reuse decisions are logged:

```
🔥 [hot-restart] loaded 12 stored identities (0 pruned as stale >30d); carrying forward 12
♻️ client_id: 4f2c... (reused)
✨ client_id: 91bd... (new)
```

## Turning it off or on

`hot-restart` is a `set` key, not a standalone subcommand. Calling it without `set` fails with `unknown command`, so use the `set` form shown below.

```bash
# Reuse off: the next start mints fresh identities
urnet-tools set hot-restart off

# Reuse on again (explicit on is equivalent to the default)
urnet-tools set hot-restart on
```

`off` is stored as a value for this key, it does not clear the override. There is no separate reset to the startup default: turn reuse on explicitly instead.

The override applies to the next time a slot authenticates. Connections that are already up keep their identity until they reconnect. If the process is stopped, the change is queued and applied on the next start.

The environment equivalent, for a systemd user unit:

```bash
systemctl --user edit urnetwork.service
```

```ini
[Service]
Environment=URNETWORK_HOT_RESTART=0
```

On the miner, the same variable is read from its own state directory environment.

## Docker

Pass the variable at container creation. There is no default value inside the image, so leaving it unset keeps reuse on:

```bash
docker run -e URNETWORK_HOT_RESTART=0 ...
```

```bash
# Check what a running container sees
docker exec urfix sh -c 'echo ${URNETWORK_HOT_RESTART:-1}'
```

## The provider store

`$HOME/.urnetwork/.client_jwts.json`, or `$URNETWORK_STATE_DIR/.client_jwts.json` when that override is set. One entry per identity slot:

| Field | Meaning |
| :--- | :--- |
| `by_client_jwt` | The signed client token the platform issued |
| `client_id` | The id carried in that token |
| `network_id` | The network that minted it, used as a credential-swap guard |
| `minted_at` | When the entry was written, used for the 30 day prune |

On this fork the slot key is the proxy address plus the proxy auth user (`ProxySettings.Key()`), so rotating a proxy username selects a new identity slot even when the address is unchanged. The 3.23-fix and meso-miner forks key by address alone.

The store keeps up to 10 timestamped snapshots taken before an overwrite, so a corrupted or truncated file can be inspected. Snapshots of identical content are at most one every 5 minutes, which stops a crash loop from cycling through every backup slot. A file that fails to parse is reported and the provider starts fresh instead of refusing to run.

Entries whose `minted_at` is older than 30 days are pruned at load time.

## The miner line

The miner does not use one shared store. Its identity unit is a durable registration scope, and each slot keeps its own credential:

| Slot | Credential file |
| :--- | :--- |
| Native provider | `.provider.jwt` |
| One proxy | `.provider-<hash>.jwt`, where the hash is derived from the network and proxy address |

- The slot is `direct` for the native provider, otherwise `proxy-sha256:<sha256(network, address)>`. Proxy credentials and proxy order do not select identity.
- Every slot in one process shares a single Ed25519 seed (`.provider.key`), while holding its own client id. One id per slot is not one key per slot.
- Overlapping slots, or two slots pointing at the same credential path, are refused before any worker starts.
- The registration record is written before the credential, so a crash between the two replays the original request instead of allocating a second client. The platform's own scope uniqueness is what stops a duplicate allocation.
- With `URNETWORK_HOT_RESTART=0` this fork takes a fresh-mint path: it registers a new client for the slot, overwrites the credential file, and publishes custody for the new identity. Upstream has no such path.
- A refused identity is tombstoned in a `.rejected` file beside its credential and is not reused.
- A renewal that fails before the platform gives a verdict (dropped connection, timeout, 5xx, 408, 429) keeps the identity and retries. This fork does not throw the identity away on a transient error; the relay provider forks mint fresh in that situation.
- Starting the miner on a state directory written by 3.23-fix triggers an automatic import: the retained seed is proved against the platform for each saved client id, and only then are the per-slot credential files written. Decisions land in `.provider.legacy-import.json`, and the old files are never modified, so going back to the relay provider still finds its identities.

Because the seed is shared per process, two slots from one process cannot both be members of the same fleet manifest: the manifest validation refuses a repeated client key. Swarm mode gives every member its own state directory and its own seed.

## What keeps an id and what mints a new one

| Change | Result |
| :--- | :--- |
| Restart, same state dir, same proxy target | Same `client_id` (reused) |
| Stored token expired, server row still present | Renewed with the same `client_id` and `device_id` |
| New proxy address | New slot, new `client_id` |
| New proxy auth user at the same address | New slot on this fork's provider; same miner slot, because the miner derives its slot from the network and proxy address only; same slot on 3.23-fix and meso-miner |
| Different account or network (`~/.urnetwork/jwt` replaced) | Fresh mint, logged as `network_id mismatch` |
| State dir wiped, new `HOME`, container recreated without its volume | Fresh mint |
| `minted_at` older than 30 days | Entry pruned, fresh mint on return |
| Reuse turned off | Fresh mint on every start |

Identities are per slot, never shared across hosts. A cloned state directory is not detected locally: the platform resolves it to the same scope, so the second host silently shares the identity instead of failing loudly.

## Forcing a fresh identity

```bash
# Fresh ids for exactly one restart, then reuse again
urnet-tools set hot-restart off
urnet-tools restart -y
urnet-tools set hot-restart on
```

```bash
# Provider: reset the whole store, then start
urnet-tools stop
mv ~/.urnetwork/.client_jwts.json ~/.urnetwork/.client_jwts.json.bak
urnet-tools start
```

```bash
# Miner: one run with fresh registration for every slot
docker run -d --name urnet-miner -e URNETWORK_HOT_RESTART=0 ...
```

A direct `provider` invocation takes the same variable in its environment.

Setting `URNETWORK_HOT_RESTART=0` in front of `urnet-tools restart` does nothing: the unit and the container start from their own environment, not from the caller's shell.

These routes replace the identity for every slot that connects. Turning reuse back on afterwards reuses whatever the last start wrote.

## Interaction with the rest of the tool

- **Proxy audit.** Parking a proxy that grades as junk requires reuse. With hot restart off the audit still runs, but it observes and logs `would-park` instead of parking, because every relaunch would otherwise mint a new identity.
- **HotSwap.** A handover runs the replacement against the same state dir, so the incoming process presents the same identities as the outgoing one. The brief overlap where both processes hold connections is documented on the [HotSwap](/full-bars/sn/wiki/HotSwap) page.
- **Miner restart machinery.** On the miner the same key also gates the hotswap trigger: a hotswap request is refused with `hot restart is disabled` when the key is off.
- **Session save and load.** `urnet-tools session save` bundles `.client_jwts.json` together with the account JWT and the proxy state, so loading that bundle on another box carries the same client ids with it. The backend keeps one resident per client, so two hosts running the same bundle displace each other rather than doubling throughput. It is a way to move a provider, not to clone one.

## Troubleshooting

**Ids churn on every restart.** Check `urnet-tools set hot-restart` first, then `URNETWORK_HOT_RESTART` in the unit environment or the container spec. A container recreated without a persistent state volume also churns, because the credentials are gone each time.

**`🔥 [hot-restart] network_id mismatch` in the log.** The account token now carries a different `network_id` than the stored entry, usually after switching accounts or networks. The old entries cannot be reused and are replaced on the next auth.

**`client limit exceeded`.** The platform refused a new client because the account is at its concurrent client limit. Churn makes this more likely, since every fresh mint adds a row.

**A slot comes back with a new id even though reuse is on.** Either the entry aged past the 30 day prune, the token could not be renewed, the identity was tombstoned after a refusal, or the state dir changed. The log line for the slot says which path was taken.

**Store reported as corrupt.** The provider logs `⚠️ [jwt-store] corrupt ... starting fresh` and mints new identities. The previous bytes are kept in a snapshot beside the store for inspection.

## Verifying reuse after a restart

```bash
ids() { urnet-tools proxy ids 2>/dev/null | awk '$1 != "PROXY" && NF >= 2 {print $1" "$2}' | sort; }
ids > /tmp/before.txt
urnet-tools restart -y
ids > /tmp/after.txt
diff /tmp/before.txt /tmp/after.txt
```

An empty diff, plus `♻️ client_id: ... (reused)` lines in the startup log, means every slot kept its identity. A `✨ ... (new)` line for a slot you did not change is the one worth investigating.

On the miner, `proxy ids` does not enumerate the per-slot credential files, so use the `🔥 [hot-restart]` lines in the miner log for the same check.
