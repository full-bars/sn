# HotSwap make-before-break handover (design proposal)

> [!NOTE]
> **Status: proposal, not scheduled.** This needs research (see [Open questions](#open-questions)) and development before it can ship. It is a plan for a future point release, not a commitment. It will ship behind an opt-in flag as an ordinary release, not as a release candidate.

## Problem

HotSwap today removes the 2 to 3 second window in which no provider process is running. It does not keep the provider's proxy connections up. Measured on a node with about 1,300 established proxy connections (sampled every second per process, no tracing), swapping 32.0 to 31.2:

| Time after the handover signal | Old process | New process | Total |
|---|---|---|---|
| before | 1,296 | 0 | 1,296 |
| +0.6 s | 51 | 2 | 53 |
| +4 s | 48 | 143 | 191 |
| +10 s | 50 | 395 | 445 |
| +16 s | 46 | 647 | 693 |
| +29 s | 42 | 1,068 | 1,110 |
| +32 s | 0 (exited after its 30 s drain) | 1,133 | 1,133 |
| +54 s | 0 | about 1,190 | about 1,190 |

A plain restart of the same node reached the same connection level in about 28 s. So for the platform-facing capacity, a hotswap behaves like a restart. Measured numbers and method are in `docs/HotSwap.md` and progress entry 1531.

A second direction (31.2 to 32.0) showed an additional effect: the old process re-established about 1,000 connections of its own during its drain, so about 2,100 connections were established for about 30 s. In that run the replaced process was itself a previous HotSwap candidate.

## How the handover works today

1. `urnet-tools update` sends SIGUSR2 to the running provider (the parent).
2. The parent starts the new binary (the candidate) as a child and talks to it over a private socketpair with three messages: READY, TAKEOVER, ACK.
3. The candidate passes its pre-flight checks, announces READY, receives TAKEOVER, and starts bringing up its own proxy clients, reusing the same client ids with new instance ids.
4. After the candidate's ACK, the parent calls `yieldCoordinatorSession()` (`provider/hotswap.go`), which runs every registered closer at once. Each proxy client registers `platformTransport.Close()` as its closer (`provider/provide.go`). This is the mass drop.
5. The parent then drains in-flight streams for up to 30 s and exits.

The process-level handover (MAINPID, control socket, metrics) is a single global switch. That part is fine. The problem is that the per-proxy client sessions are switched by the same global event.

## What the backend allows

Read from the upstream server code (`model/network_client_resident_model.go`, `connect/resident.go`):

- The backend keeps one resident per client id. A connection with a different instance id for the same client replaces the current resident (the newest writer wins).
- The replaced resident polls every `ExchangeResidentTtl / 4`, finds it is no longer current, and shuts itself down.
- The connection limit check treats a re-nomination of an already-connected client as not a new connection.

So the backend already supports switching one client from one instance to another cleanly. It does not need a global cutover.

## Proposal: hand over one client at a time

Replace the single global yield with a per-client release:

1. The candidate brings up client X and confirms it is live (transport connected and past a health check).
2. The candidate sends the parent `CLIENT_LIVE(client_id)` over the existing socketpair.
3. The parent closes only client X's transport and marks X released so its supervisor does not reconnect it.
4. Repeat for every client, in the candidate's normal start-up order (earnings ranking first).
5. Global pieces (MAINPID, control socket, metrics) switch once, as today.
6. Safety fallbacks: if the candidate never confirms a client within a deadline, the parent keeps that client. If the candidate dies before it has connected any client, the parent keeps everything (already true today). After the candidate has connected client X, the backend has already replaced the old resident for X (newest instance wins), so the parent can no longer keep its old connection; recovery has to reconnect X from the parent, which is why the handover needs the commit step described in open question 8. An overall deadline ends the drain exactly as today.

Result: a client never has zero connections, so the connection total should stay near its pre-swap level instead of dropping to about 4%.

### Not applicable

- **Docker PID-1 path** replaces the process image in place with `execve`. There is no second process, so there is nothing to hand over gradually. It keeps today's behavior.
- **Moving a live connection between processes** is not possible. Each is an encrypted session held inside the process. Every connection is re-established; this design only changes the order.

## Open questions

These need answers before implementation starts:

1. **Streams at the switch moment.** When the backend replaces client X's resident, what happens to a stream in flight on X? Does the platform re-route it, or does it drop? Test with a long transfer across a swap.
2. **Contracts and earnings.** Are they keyed by client id or by instance id? A per-client switch must not lose or double-count a contract.
3. **Old process reconnecting.** The second measured swap suggests the old process can re-establish clients after yielding. Find the cause (a promoted candidate acting as parent) and fix it first; without that, per-client handover would let the two processes displace each other.
4. **Proxy vendor concurrency.** Both processes will hold a session to the same proxy at the same time. Measure this on a vendor that limits concurrent sessions per credential or per source IP.
5. **API load.** Every client authenticates twice (old and new). Check that the shared auth rate limiter stays healthy and stagger start-up if not.
6. **Memory.** Two full client sets are alive together. Quantify on the lowmem profile and decide whether it needs a guard.
7. **Observability.** There is no per-process connected-client metric. Add one, otherwise the outcome counter reports success while capacity is dipping.

8. **Candidate failure after displacement.** The backend has no reserve concept: connecting the candidate's instance replaces the old resident at once. If the candidate dies after connecting X but before the parent has released X, the old connection is already gone. Design a two-phase handover: the candidate reports `CLIENT_LIVE(X)`, the parent keeps X's supervisor armed (able to reconnect) until the candidate confirms it holds X after the release, and a candidate death or health-check failure in between makes the parent reconnect X. Test by killing the candidate at each step.

## Success criteria

Measured with the same per-process connection sampler used above, on a node with 1,000 or more proxies:

- The connection total never falls below 95% of its pre-swap value during the swap.
- At most one process holds a given client's connection at any instant, apart from the one being migrated.
- No increase in authentication failures over the quiet baseline.
- Peak memory stays within a stated bound.
- Wall-clock swap time is no worse than today's.
- Killing the candidate at any point in the handover leaves every client connected again within the normal reconnect time.

## Phases

| Phase | Work | Ships as |
|---|---|---|
| 0 | Research: answer questions 1, 2, 4 and 8 with experiments; measure questions 5 and 6 (authentication load and memory) so Phase 3 has numbers to gate on; read the backend paths involved | notes, no release |
| 1 | Add a per-process connected-client metric | normal point release |
| 2 | Fix the chained-swap regrowth (question 3) | normal point release |
| 3 | Implement the per-client protocol behind an opt-in environment flag | normal point release, off by default |
| 4 | Canary on a couple of nodes for a full update cycle, compare against the success criteria | no release |
| 5 | Make it the default; keep the flag as an escape hatch for one release | normal point release |

Phase 3 entry criteria: answers to questions 1, 2, 4 and 8; authentication load with both processes running shows no increase in authentication failures over the quiet baseline; peak memory with both client sets alive is within the stated bound.

## Related

- [HotSwap how-to and measured costs](../HotSwap.md)
- Tracking issue: to be filed once this design is agreed.
