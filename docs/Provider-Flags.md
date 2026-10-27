# Provider identity and registration flags

A provider slot is a client identity: a key plus a token the platform issues.
Creating a new one is called *registration*, and it is the only part of the
provider that talks to the platform about who the box is. These flags control
when the provider may create, adopt or import one.

The short version: **a slot never stops to ask.** It uses the identity it has,
and if the platform refuses that identity it mints a new one and saves it. The
flags below exist for the cases where you want to override that judgement, or
where you want to see exactly what happened.

## Identity

| Flag | Default | What it does | When to use it |
|---|---|---|---|
| `--allow-client-registration` | permitted | Explicit form of permission to create an identity. | Deliberate key rotation over a zero or wrong-length `.provider.key`: only explicit creation rotates, and the replaced file is kept as `.provider.key-<UTC timestamp>.bak`. |
| `--no-client-registration` | — | Withholds creation for this run. A slot that needs an identity then fails with `client_registration_disabled` in one line. | You are investigating identity churn, or you want a run that provably allocates nothing. |
| `--adopt-legacy-provider-key` | off | Adopts a legacy key and JWT as this provider's custody. | Migrating a box whose saved identity predates this engine, when automatic import could not prove it. |
| `--no-legacy-import` | off | Disables the automatic import of 3.23-fix saved identities entirely. | You want no migration at all; the old files stay untouched. |
| `--legacy-import-budget` | 5m | Time budget for the automatic import. | A very slow or large state directory needs more time. |

### What "never stops to ask" means in practice

1. The slot loads its saved identity and tries it with the platform.
2. The platform accepts it: the identity is used, and nothing else happens.
3. The platform refuses it, or there is no usable identity: the provider mints
   a new identity, saves it immediately, and serves from the next start on.
4. An identity the platform has explicitly **revoked** is not replaced. Minting
   there would replace a client the platform bound to this provider key, so the
   refusal stands and three tests across three layers hold it in place.

A minted identity is a *new* client as far as the network is concerned, so that
slot's earnings history starts over. That is the accepted cost of a slot that
serves instead of waiting for a human.

### Reading the outcome

| Line | Meaning |
|---|---|
| `minted new identity (first start)` | the slot had no saved identity |
| `minted replacement (saved identity rejected)` | the saved identity was refused by the platform |
| `client_registration_disabled` | creation was withheld by `--no-client-registration` |
| `client_revoked` | the platform revoked this identity; it is not replaced |

## Operators

| Flag | Default | What it does |
|---|---|---|
| `--all-operators` | off | Mine every operator listed at `--operators-url`. |
| `--auto-register` | off | Validator lane: register the operator's clients automatically, as a one-shot. Needs `--hotkey-seed-file` and `--all-operators` in the container helper. |
| `--quarantine-rejected-sign-in` | off | An operator child whose network sign-in the renewal call rejects is set aside instead of retrying forever. |

## Flags people expect but will not find

- There is no flag to pick an IPv4 or IPv6 transport per slot: the slot decides
  from what the host and the proxy can actually reach, and the capability line
  in `urnet-tools status` reports what it measured.
- There is no flag to purge a single slot's identity. Clear the files for that
  slot in the state directory; the next start mints a fresh identity under the
  rules above. This is deliberate, because a purge flag is a foot-gun on a box
  where several slots share a proxy address.
