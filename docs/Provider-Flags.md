# Provider identity, and why there is nothing to set

If you have ever wondered what `--allow-client-registration` is for, this page
is the answer. It is the least self-explanatory flag in the provider, and as of
this release it does nothing at all.

## What a provider client is

A `provide` run carries traffic on behalf of the network. Before it can do that,
the platform has to know *which* provider is carrying it: that is the provider
client. It is a key pair plus a credential the platform issues, and the platform
uses it to authenticate the provider's control-plane calls and to attribute the
traffic the provider serves.

That last part is why anyone cares. A provider earns by serving traffic, and the
platform can only credit traffic to a provider it can identify. The client
identity is not paperwork — it is what the earnings hang from. A slot with no
working identity serves nothing and earns nothing, however healthy its proxy is.

Be precise about what this identity is **not**: it is not a Bittensor hotkey, it
does not prove miner admission to a subnet, and it does not establish seat,
stake, contract authority or account funding. Those belong to the subnet layer.
The provider client is the platform-side identity that the subnet's work travels
under, and `docs/Bittensor-Fleet.md` covers the other layer.

## What registration is, and why upstream gated it

Creating that identity is registration: the provider calls
`/network/register-client-v1`, the platform records the client against the
provider's key, and the reply carries the credential.

Registration is the one moment where the provider asks the platform to bind a
lasting identity, and upstream decided that must never happen by accident. The
code around it shows the discipline:

- The provider **records the request before sending it**, so a lost reply is
  replayed rather than resolved by guessing.
- On any error it **never falls back** to `/network/auth-client`, never rotates
  the key, and never invents a validator identity to get past the failure.
- A known new installation or new proxy member required
  `--allow-client-registration`: an explicit statement that yes, an identity
  should be allocated here.

The point was not anti-abuse. It was anti-surprise: a refusal that names a flag
is the machine saying "I would allocate a new identity, and I will not do that
on my own". With an operator standing at the terminal, that is exactly right.

## Why this fork removes the gate

On an unattended fleet nobody is standing there. The same discipline that
protects someone at a terminal becomes an indefinite stop: the slot refuses,
waits for a flag it will never get, and earns nothing while looking
authenticated in the logs. On one real host fifty slots sat in that state for
hours, reported only as a count of failed providers, with no action anywhere in
the UI able to clear it.

So the fork keeps the parts that protect correctness and drops the part that
needs a human:

- **Keep** record-before-send, replay on a lost reply, and never take a weaker
  path or rotate a key to get past an error.
- **Keep** the refusal for an identity the platform has **revoked**. Minting
  there would hand you a new client for a key the platform deliberately
  disowned; three tests across three layers hold that line.
- **Drop** the requirement that a human approve a new identity. A slot that
  cannot use its saved identity mints one, saves it, and serves.

`--allow-client-registration` is still accepted so existing units, scripts and
images keep working, and it is ignored. There is no
`--no-client-registration`, and we do not intend to add one: a provider that
cannot mint cannot serve, and a host you want stopped is a host you stop.

## What a mint costs

A new identity is a new client as far as the platform is concerned. That slot's
attribution history starts over, and the old history cannot be attached to the
new identity afterwards.

That is the accepted trade: serving under a new identity beats waiting forever
under the old one. The saving grace is that a mint only happens when the
platform refuses the saved identity, so a healthy host mints nothing, ever.

## What "heals instead of parking" means in practice

1. The slot loads its saved identity and offers it to the platform.
2. The platform accepts it: the identity is used, and nothing else happens.
3. The platform refuses it, or there is none: the provider mints a new identity,
   saves it immediately, and serves from then on.
4. The platform has revoked it: the refusal stands, and the log says so.

## Recognising what happened

Every start reports the outcome per slot, one line each:

| Line | What happened |
|---|---|
| `minted new identity (first start)` | there was no saved identity; this is a fresh slot |
| `minted replacement (saved identity rejected)` | the platform refused the saved identity, so a new one was minted |
| `adopted identity (legacy)` | a 3.23-fix identity was verified and taken over unchanged |
| `client_revoked` | the platform revoked this identity; it is not replaced |

The first two are the pair worth watching. A few `minted new identity` on a
fresh host is normal. A wave of `minted replacement` means the platform is
refusing identities that used to work, which is a different problem from a bad
proxy — and the two are otherwise easy to confuse, because both show up as slots
that are not serving.

## The other identity flags

| Flag | What it does |
|---|---|
| `--adopt-legacy-provider-key` | Adopts a legacy key and JWT as this provider's custody. Only when automatic import cannot prove the identity and you have recovered the original seed. Grants no new allocation. |
| `--no-legacy-import` | Turns off automatic import of 3.23-fix identities entirely. Old files are left untouched. |
| `--legacy-import-budget` | Time budget for that automatic import; default 5 minutes. |

## Operators

| Flag | What it does |
|---|---|
| `--all-operators` | Mine every operator listed at `--operators-url`. |
| `--auto-register` | Validator lane: register the operator's clients automatically, as a one-shot. Needs `--hotkey_seed_file` and `--all-operators` in the container helper. |
| `--quarantine-rejected-sign-in` | An operator child whose network sign-in the renewal call rejects is set aside instead of retrying forever. |

## Flags people expect but will not find

- **No per-slot IPv4/IPv6 switch.** The slot decides from what the host and the
  proxy can actually reach, and `urnet-tools status` reports what it measured.
- **No flag to purge one slot's identity.** Clear that slot's files in the state
  directory and the next start mints a fresh identity under the rules above.
  This is deliberate: a purge flag is a foot-gun where several slots share one
  proxy address.

## Further reading

- `miner/PROVIDER-REGISTRATION.md` — custody rules, the immutable operation, and
  what replay does and does not guarantee.
- `docs/Miner-Image.md` — the container's `ALLOW_CLIENT_REGISTRATION`, which is
  now accepted and has no effect.
