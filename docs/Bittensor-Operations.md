# Bittensor Subnet 25: Overview for Operators (sn fork)

> [!TIP]
> **Most operators can stop at the [Quick Start](Bittensor-Quick-Start.md).** You sign one message from your own computer and your nodes keep running as they are. Everything below is background, or for people who want more.

> [!NOTE]
> Checked against upstream server `50851997d`, connect `f1837db6`, sdk `23b3c7dd` and sn `83e6f48c2` (2026-10-21). Upstream is still changing the Bittensor side quickly. If a command prints something different from this page, trust the command.

## The short version

- URnetwork is **Subnet 25 (SN25)** on Bittensor. Your nodes can earn the SN25 token (alpha, branded $UR) on top of normal URnetwork earnings.
- You get paid by signing **one** consent with your Bittensor wallet (your coldkey). It says "pay every node on my URnetwork account to this coldkey". One signature covers every node on your URnetwork account, now and later.
- **Every sn build from release `v2026.10.21-1077735390-meso` can sign**: the release `provider` download, the `:latest` image and the `:miner` image have `provider wallet sign` and `wallet status`. Older builds do not (`provider --help` will not list them); update, or sign with a 3.23-fix or meso-miner `provider`. Sign on your own computer, not inside a container. The consent covers your whole account whichever line the nodes run. Steps are in the [Quick Start](Bittensor-Quick-Start.md).
- Check where you stand with `provider wallet status`, run with that same `provider`. Words you do not know are in the [Glossary](Bittensor-Glossary.md).

## Pool or head: which one am I?

| | Pool tier (almost everyone) | Head tier (optional, advanced) |
|---|---|---|
| What it is | Your nodes earn inside URnetwork's shared pool. | You get your own seat (UID) on the subnet. |
| What you do | Sign the consent once. | A long setup with keys, a fleet file, a burn and per-node binding. See [Head Tier](Bittensor-Head-Tier.md). |
| How you are scored | By the work your nodes do in the pool: usage weighted by quality, with quality kept between 0.75 and 1.0. | By how many different IP ranges (/29 IPv4, /48 IPv6) your nodes served traffic through. |
| How you are paid | Credited to your coldkey every epoch (7 days). A claim moves it to your wallet. | Paid directly to your hotkey every tempo (about 72 minutes). No claim. |
| Where the money waits | In the settlement vault contract (`STSettlementVault` for v1, `STSettlementVault2` for v2). | Nowhere. It is paid by the network. |
| Up-front cost | None. | A registration burn plus gas, and you can lose the seat. |
| Earnings promise | None is promised. A share that rounds to zero pays nothing. | None is promised. The seat can be lost. |

If your head seat is lost, your nodes fall back to the pool. A node earns in only one tier at a time.

## Claims, explained simply

Your pool earnings are credited to your coldkey. A **claim** is a small blockchain transaction that moves the credit into your wallet.

- Anyone can send the claim. The money always goes to your coldkey.
- **Optional.** You can wait until the balance is worth the gas fee.
- **Settlement v2** starts at epoch 3 (block 9,348,028, about 2026-10-23 18:10 UTC). Upstream's launch notes call that epoch decided but the launch **not sealed**, because it depends on release gates passing. In v2, credit **never expires**.
- Before v2 (settlement v1, the earliest epochs), a claim stays open for 8 epochs plus 1 epoch of grace, and an expired claim is carried for the same operator (`claim_ttl_epochs`, `claim_grace_epochs` and `expired_claim_action` in upstream's `deploy/mainnet/policy-v1.yml`). What carrying means for one operator's payout is not documented here.
- The SDK the apps are built on can sync pool claims (sdk `23b3c7dd`, `SnPoolClaimSync`). Whether the app you have shows a claim button is **not confirmed**.
- Command-line claims exist for people who want automation. They need an EVM key with TAO for gas. See [Claiming by hand](#claiming-by-hand-advanced). Claim daemons are in the miner image.

## Signing the consent (reference)

The Quick Start has the steps. In short, with a current sn `provider` (or a 3.23-fix or meso-miner one) on your own computer:

```bash
provider auth                            # prompts; paste the code at the prompt
provider wallet sign <coldkey_ss58>    # opens a local page for your wallet extension
provider wallet status                 # confirm
```

Releases: <https://github.com/full-bars/sn/releases>.

Options on that binary:

- `provider wallet sign [<coldkey_ss58>] [--port=<port>] [--no-open]`. Leave the address out to choose the account on the page.
- No browser: `provider wallet challenge <coldkey_ss58>` prints the exact text (it signs nothing). Sign it elsewhere, then run `provider wallet set <coldkey_ss58> --message=<text> --signature=<hex>`.
- `provider wallet set <coldkey_ss58> --coldkey_seed_file=<path>` signs from a seed file. Use it only on your own computer, never on a node.
- `--wallet-from-epoch=<e> --wallet-through-epoch=<e>` pick an exact window. It spans at most 65,536 epochs. A newer consent must start at a later epoch than the one it replaces.

### What this fork's own commands do

| Command | Result |
|---|---|
| `provider wallet set <coldkey>` (sn release binary) | Refused. It only works with `--legacy-network-wallet`. |
| `provider wallet set <coldkey> --legacy-network-wallet` | Old unsigned request. A login proof, **not paid**. |
| `provider provide --wallet=<coldkey>` | The same unsigned request at startup. A failure is logged and the provider keeps providing. |
| `provider wallet hotkey challenge|set|status` | Hotkey delegation. Not needed with one operator. `set` has `--replace-other-hotkey`. |
| `provider wallet set <coldkey> --coldkey_seed_file=<path>` (miner image) | A signed consent for **one node's client** only. It wins over the account-wide consent for that node. |
| `provider bind-head`, `unbind-head` (sn release binary) | The older head path (`--hotkey`, `--registrant`, `--contract`). Not the current fleet flow. Do not use it. |

Docker users: you do not change the container command for this. A run that still passes `--wallet=<coldkey>` and `--legacy-network-wallet` can drop both.

## bind-head and unbind-head (old head-tier path)

`provider bind-head` and `provider unbind-head` are the head-tier path from before the `provider fleet` group. The default sn release `provider` has only these two; the `fleet` group lives in the miner image (`ghcr.io/full-bars/sn:miner`). Do not use bind-head for new work. Their grammar in this fork is:

```text
provider bind-head --hotkey=<hex> --registrant=<registrant> --contract=<contract> [--rpc=<rpc_url>]... [--key_file=<key_file>] [--dry-run]
provider unbind-head --hotkey=<hex> [--contract=<contract>] [--rpc=<rpc_url>]... [--key_file=<key_file>] [--dry-run]
```

## Other pages, only if you need them

- [Head Tier](Bittensor-Head-Tier.md): the advanced head-tier path. The fleet commands live in the miner image.
- Hotkey delegation, multi-operator mode (`provide --all-operators`) and claim daemons are in the miner image (`ghcr.io/full-bars/sn:miner`, see [Miner Image](Miner-Image.md)). Most operators do not need them.

## Claiming by hand (advanced)

You do not need this if you use the app button or wait. `provider claim` logs in with the token of a client that served traffic, because only such a client has a payout. Pass exactly one of:

| Flag | Credential |
|---|---|
| `--store-client=<key>` | One identity from `~/.urnetwork/.client_jwts.json`. The key is a proxy address or `direct`. |
| `--provider-jwt=<path>` | A client token in a file. |
| `--legacy-coldkey=<coldkey_ss58>` | The network token plus this coldkey. Only for an epoch without a provider artifact. |

To print the claim calldata without sending anything (safe, keeps keys off the node):

```bash
provider claim --store-client=direct --epoch=<epoch>
```

To submit it yourself, add `--rpc=<rpc_url> --key_file=<path>`. Add `--dry-run` to check without sending. `snclaim` is a separate low-level binary that takes the calldata (`snclaim --help` lists its commands, including `v2 status`, `v2 claim` and `v2 check`).

## The status dashboard (`sn-status`)

Optional. It is a read-only summary:

```bash
provider sn-status
provider sn-status --json
```

In this fork it reads the old-style wallet record. It may say the coldkey is "Not configured" even after you signed the consent. That is **UNCONFIRMED** at the current head, so use `provider wallet status` to check the consent. The 3.23-fix dashboard has a green `Wallet consent` line; this fork's may not.

> [!NOTE]
> The "Global Rank" and tier lines on this screen are the URnetwork **bandwidth leaderboard**. They are **not** head-tier eligibility and have nothing to do with the Top 200 seats. Ignore them for Bittensor purposes. 

## FAQ

### Do I have to do anything?
Run `provider wallet status` (every sn build from `v2026.10.21-1077735390-meso` has it). If it shows a network consent in effect, you are done. If not, sign once from your own computer. See the [Quick Start](Bittensor-Quick-Start.md). Nothing else is required, and your nodes keep providing either way.

### I linked my wallet on the website or in the app. Am I paid?
Not necessarily. The older "connect wallet" link is only a login proof, and payouts do not use it. A signed consent is what counts. Confirm with `provider wallet status`. If it says `NOT a consent`, sign once.

### Why did my browser wallet not pop up?
You probably ran the old unsigned path (`provide --wallet=`, `--legacy-network-wallet`). That sends an address without a signature, and without a signature nothing gets paid. Use `provider wallet sign`, which does open your wallet.

### What can the signature do?
It covers every node on your URnetwork account. Your recovery phrase never leaves the wallet extension, which shows exactly what it signs. The signature cannot move your funds. It decides which wallet gets paid.

### Do I need a UID to earn?
No. Pool operators have no UID. Only head fleets hold a seat.

### Do I need a hotkey?
Not unless you do the head tier or sign in with a hotkey.

### Do I need to renew the consent?
No. It lasts 65,536 epochs. Sign again only to change the wallet.

### What happens to claims while a claim daemon is stopped?
Nothing is lost. Signed entries are saved and finished after a restart.

### Is the head tier worth it?
For most operators, no. See [Head Tier](Bittensor-Head-Tier.md) for what it takes and the current blockers. It needs `provider fleet ...`, which only the miner image (or a 3.23-fix or meso-miner `provider`) has.

## Safety rules

1. Never put a token or seed in a command line where `ps` can show it.
2. Keep `~/.urnetwork/jwt` and any key files at mode `0600`.
3. Give each container its own volume so state does not get mixed up.
4. Seed files (coldkey, hotkey, client keys) belong on your own computer, never on a mining node. The tool never creates them for you.
