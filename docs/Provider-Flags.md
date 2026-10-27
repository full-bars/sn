# Provider identity, and why there is nothing to set

If you have ever wondered what `--allow-client-registration` is for, this page
is the answer. It is the least self-explanatory flag in the provider, and as of
this release it does nothing at all.

## What a provider client is

A `provide` run carries traffic on behalf of the network. Before it can do that,
the platform has to know which provider is carrying it: that is the provider
client. It is a key pair plus a credential the platform issues, and the platform
uses it to authenticate the provider's control-plane calls and to attribute the
traffic the provider serves.

That last part is why anyone cares. A provider earns by serving traffic, and the
platform can only credit traffic to a provider it can identify. The client
identity is not paperwork — it is what the earnings hang from. A slot with no
working identity serves nothing and earns nothing, however healthy its proxy is.

Be precise about what this identity is not. It is not a Bittensor hotkey, it
does not prove miner admission to a subnet, and it does not establish seat,
stake, contract authority or account funding. Those belong to the subnet layer.
What connects the two is the binding: a fleet operator links a UR client to a
hotkey for an epoch window with a dual-signed binding, and that is what ties the
traffic this client carries to a miner's standing. `docs/Bittensor-Fleet.md`
documents that lifecycle — `register`, `publish`, `bind`, `sign`, `status`,
`revoke`. So the client is the platform-side identity that the subnet's work
travels under, and the binding is where the two meet.

## What registration is, and why upstream gated it

Creating that identity is registration: the provider calls
`/network/register-client-v1`, the platform records the client against the
provider's key, and the reply carries the credential.

Registration is the one moment where the provider asks the platform to bind a
lasting identity, and upstream decided that must never happen by accident. The
code around it shows the discipline:

- The provider records the request before sending it, so a lost reply is replayed
  rather than resolved by guessing.
- On any error it never falls back to `/network/auth-client`, never rotates the
  key, and never invents a validator identity to get past the failure.
- A known new installation or new proxy member required
  `--allow-client-registration`: an explicit statement that yes, an identity
  should be allocated here.

The point was not anti-abuse. It was anti-surprise: a refusal that names a flag
is the machine saying "I would allocate a new identity, and I will not do that on
my own". With an operator standing at the terminal, that is exactly right.

## Why this fork removes the gate

On an unattended fleet nobody is standing there. The same discipline that
protects someone at a terminal becomes an indefinite stop: the slot refuses,
waits for a flag it will never get, and earns nothing while looking
authenticated in the logs. On one real host fifty slots sat in that state for
hours, reported only as a count of failed providers, with no action anywhere in
the UI able to clear it.

So the fork keeps the parts that protect correctness and drops the part that
needs a human:

- Keep record-before-send, replay on a lost reply, and never take a weaker path
  or rotate a key to get past an error.
- Keep the refusal for an identity the platform has revoked. Minting there would
  hand you a new client for a key the platform deliberately disowned, and three
  tests across three layers hold that line.
- Drop the requirement that a human approve a new identity. A slot that cannot
  use its saved identity mints one, saves it, and serves.

`--allow-client-registration` is still accepted so existing units, scripts and
images keep working. It does nothing for client identities: a slot that cannot
use its saved identity mints one and serves, whether the flag is there or not.
One thing it still does, and the name does not say it: when the state directory
holds a `.provider.key` that exists but cannot be used — the all-zero seed a
v33-era install can carry, or a key of the wrong length — passing the flag
replaces it with a fresh seed and keeps the old file as a timestamped backup.
Without the flag that key is left alone (zero seed, with a loud warning) or
refused (wrong length). A missing key over retained provider history is refused
either way: no flag can mint a new provider identity in place of one that was
lost. A provider that cannot mint cannot serve, and a host you want stopped is a
host you stop.

## What a mint costs

A new identity is a new client as far as the platform is concerned. That slot's
attribution history starts over, and the old history cannot be attached to the
new identity afterwards.

That is the accepted trade: serving under a new identity beats waiting forever
under the old one. In the ordinary case a mint only happens when the platform
refuses the saved identity, so a host with intact state and a cooperative
platform mints nothing.

Do not read that as "minting always means a platform problem". A host restored
from a backup taken before these identities were written, or pointed at the
wrong state directory, has no saved identity for its slots and will mint one for
every one of them within seconds. That host is healthy by every other measure
and its log is full of `minted new identity (first start)`. So the first thing
to check when you see mints is not the platform, it is whether the state
directory being read is the state directory you think it is: a wrong mount, a
fresh volume, a restore that predates the identities, or a permissions change
that makes the existing files unreadable all look identical from inside the
process.

## What healing means in practice

The slot loads its saved identity and offers it to the platform. If the platform
accepts it, the identity is used and nothing else happens. If the platform
refuses it, or there is none, the provider mints a new identity, saves it
immediately, and serves from then on. If the platform has revoked it, the
refusal stands and the log says so.

## Recognising what happened

The line an operator actually watches is printed per slot, as it always has been:

```
♻️ client_id: 4f2c… (reused) provider=12 slot=proxy:203.0.113.9:1081
✨ client_id: 91bd… (new)    provider=13 slot=proxy:198.51.100.4:1081
```

`(reused)` means the slot kept the identity it had; `(new)` means it is a different
client from the last start. Grepping for `(reused)` finds every slot that kept
its identity, and `docs/Hot-Restart.md` treats an empty diff of those lines plus
all-`reused` as proof that a restart changed nothing. A `(new)` line for a slot
you did not touch is the one worth investigating.

On top of that per-slot detail, the provider reports the shape of the start in
the startup summary. It does not repeat the per-slot story: it counts how the
decisions went — minted new, minted replacement, adopted, revoked — and says
nothing extra when every slot reused its identity, so a healthy host's line is
the one it always printed.

When the platform rejects the identities the pool presented past `max(3, 5% of
them)`, one warning follows, naming both likely causes rather than guessing
between them:

```
⚠️ [identity] 37 of 500 saved identities were replaced or refused this start
(threshold 25); check first that the state directory is the one holding the
identities, then whether the platform is refusing identities that used to work
```

The count is over saved identities, not over slots: the slots that came up
reused are the denominator, so four healings on a 500-slot host read as four of
five hundred and stay a line. The decision waits for the last slot to resolve,
because a rate over the slots that happened to come up first is not a rate over
the pool: six replacements among the first six slots is not a wave if the other
494 then reuse.

That order matters. A restore older than the identities, a partial mount that
holds stale credentials, or a permissions change all produce identities the
platform will not accept, and none of them is the platform's fault. Only after
the state directory is confirmed should you suspect the platform.

Work through it in this order. First, confirm the state directory is the one you
think it is: a restore from before the identities existed, or a partial mount
that holds stale credentials, produces a full wave of replacements and looks
like nothing else is wrong. Second, if the state is intact and the mints are
replacements, ask the platform whether it revoked or refused those clients; a
platform-side refusal wave is not something a host can fix by restarting. Third,
if only some slots are affected, look at what those slots share — one proxy
address, one upstream source, one operator — because that points at the proxy
rather than at identity at all.

A start that finds no saved identity at all is not this warning. It mints one per
slot, which is what a first start does, and it prints `0 reused, N new` in the
startup summary instead. A wrong mount of an empty directory looks the same from
inside the process, so that summary line is where to look for it.

For the per-slot detail — which slot, which outcome, what reason, when — every
outcome is appended to the `identity_events` bucket in `provider_state.db`,
bounded so it cannot grow without limit. Two limits worth knowing before you go
looking:

- Nothing in `urnet-tools` reads that bucket yet. The records are durable and a
  reader is a small follow-up, but today you open the file to see them.
- A host running with hot restart off mints every identity every start by
  contract, and those mints record no per-slot event. The tally's `0 reused,
  N new` is the count for that host, and recording one event per slot per start
  would rotate the bucket's genuine replacement records out within a few
  restarts.

## The other identity flags

`--adopt-legacy-provider-key` adopts a legacy key and JWT as this provider's
custody. Use it only when automatic import cannot prove the identity and you
have recovered the original seed. It grants no new allocation.

`--no-legacy-import` turns off automatic import of 3.23-fix identities entirely.
The old files are left untouched. `--legacy-import-budget` sets the time budget
for that automatic import; the default is five minutes.

## Operator flags

`--all-operators` mines every operator listed at `--operators-url`.
`--auto-register` is the validator lane: it registers the operator's clients
automatically, as a one-shot, and needs `--hotkey_seed_file` and
`--all-operators` in the container helper. `--quarantine-rejected-sign-in` sets
aside an operator child whose network sign-in the renewal call rejects, instead
of letting it retry forever.

## What you will not find

There is no per-slot IPv4 or IPv6 switch. The slot decides from what the host and
the proxy can actually reach, and `urnet-tools status` reports what it measured.

There is no flag to purge one slot's identity. Clear that slot's files in the
state directory and the next start mints a fresh identity under the rules above.
This is deliberate: a purge flag is a foot-gun where several slots share one
proxy address.

## Further reading

`miner/PROVIDER-REGISTRATION.md` covers custody rules, the immutable operation,
and what replay does and does not guarantee. `docs/Miner-Image.md` covers the
container's `ALLOW_CLIENT_REGISTRATION`, which is now accepted and has no effect.
