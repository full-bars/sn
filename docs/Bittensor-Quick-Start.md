# Bittensor Quick Start: Do I Need to Do Anything?

This page is for regular operators. You run URnetwork nodes and you may have heard about Bittensor Subnet 25 (SN25). Start here. Almost everyone can stop after one step.

## The 30-second answer

- Your nodes keep running exactly as they are. **Do not change anything on your nodes.**
- To get paid on SN25 you sign **one message** with your Bittensor wallet (your "coldkey"). You do it **once**, from **your own computer**. Never from a node.
- That one signature covers your whole URnetwork account, so it covers all your nodes, now and later. You do not renew it.
- Everything else in the Bittensor docs (hotkeys, claim daemons, fleets, multi-operator) is optional or advanced. Skipping it costs you nothing.

## Important: this fork's `provider` cannot sign the consent

The `sn` fork's `provider` program (the one in the default Docker image and the Linux, macOS and Windows installers) has **no** `provider wallet sign` and **no** `provider wallet status`. Its `wallet set <coldkey>` only works with `--legacy-network-wallet`, which sends an unsigned request that is **not paid** (see [the old commands](#the-old-commands-in-this-fork-that-do-not-get-you-paid) below).

That is fine. The consent covers your **whole account**, whichever line your nodes run. So you sign it with a different `provider` program, once, on your own computer:

1. Download a **3.23-fix** (or meso-miner) `provider` for your computer from the 3.23-fix releases page: <https://github.com/full-bars/urnetwork-3.23-fix/releases>. Pick the file named `urnetwork-provider-<version>-<platform>.tar.gz`, where `<platform>` is `linux-amd64`, `linux-arm64`, `darwin-amd64`, `darwin-arm64`, `windows-amd64`, `windows-arm64`, `freebsd-amd64` or `freebsd-arm64`. Unpack it. The `provider` (or `provider.exe`) file inside is what you need. You only run it for the signing steps. It does not replace anything on your nodes.
2. Or, once it is live, use the URnetwork app or ur.io. A plain "connect wallet" is **not enough**. It has to be the consent signing page. Whether that is live yet is **UNCONFIRMED**.

## Step 1: Have a wallet

If you do not have a coldkey, install a Polkadot-style browser wallet extension (Talisman, SubWallet, Polkadot{.js} or Nova) and create an account. Write the recovery phrase on paper and keep it offline. The account address starts with `5`. This address is your coldkey.

## Step 2: Sign the consent (on your own computer)

Use the 3.23-fix `provider` you downloaded above.

1. Log it in to your URnetwork account with an auth code from the website. This only saves your login. It does not start a node.
   ```bash
   provider auth
   ```
   Paste the code when prompted rather than passing it as an argument: an argument shows up in `ps` and in your shell history.
2. Sign the consent:
   ```bash
   provider wallet sign <your_coldkey_address>
   ```
   A page opens on your own computer only. Your wallet extension pops up and shows the message. It starts with `Approve URnetwork network wallet mapping`. Approve it. The page counts down 15 minutes. Your recovery phrase never leaves the extension. You can leave out the address and pick the account on the page. Add `--no-open` if you want the address printed instead of the browser opening.
3. Look for the success line: `subnet wallet consent for 5... stored: epochs N through M ...`, then "You are done. Nothing changes on your provider machines."
4. Confirm:
   ```bash
   provider wallet status
   ```
   A good result shows `network consent ... in effect now` (or `starts in 1 epoch(s)`). The `receipt` line appears only when the platform returns a mapping hash, so a missing receipt is not a failure.

Good to know:

- The consent starts at the **next** weekly payout period (an "epoch"), never the current one. Epochs before your first consent are not paid.
- It lasts 65,536 epochs. You never renew it. Sign again only if you want to change the wallet.
- No browser on that computer? Run `provider wallet challenge <coldkey>` to print the exact message (it signs nothing). Sign it somewhere else (for example btcli or a hardware wallet). Then send it within about 5 minutes with `provider wallet set <coldkey> --message='<printed text>' --signature=0x<128 hex characters>`.
- If `wallet status` says you are not logged in, the saved login (`~/.urnetwork/jwt`) is not where the command looked.

## If you already linked a wallet on the website

Check once with the 3.23-fix `provider wallet status`. A website or app "connect wallet" from before the consent page is only a login proof, and payouts do not use it. If it says `NOT a consent` or `no consent is stored`, do the steps above. You already have a wallet, so skip Step 1.

## Which line are my nodes on?

The step is the same for everyone. Nothing changes on the nodes.

| You run your nodes with | What to do |
|---|---|
| **sn Docker image (`:latest`) or sn installer** | Nothing in the containers. On your own computer: 3.23-fix `provider auth <code>`, then `provider wallet sign <coldkey>`. |
| **sn miner image (`:miner`)** | Same. Its `provider` has `wallet set` with a signature, but that makes a consent for one node only (see below). The account-wide consent is simpler. |
| **3.23-fix or meso-miner nodes** | Nothing on the node. Sign from your own computer. `urnet-tools wallet sign` refuses on a node on purpose. |
| **The URnetwork app or ur.io** | A signing page is planned or rolling out. **UNCONFIRMED** that it is live. |

On Windows, use `provider.exe`. This has not been tested end to end on Windows yet, so check with `wallet status` afterwards.

## The old commands in this fork that do NOT get you paid

These all send an address with **no signature**. That is the old login-proof path. Payouts do not use it. If nothing else is set up, you earn nothing on SN25 from them.

| Command | What it really does |
|---|---|
| `provider wallet set <coldkey> --legacy-network-wallet` | Sends the old unsigned request. Without that flag this fork's `wallet set` is refused. |
| `provider provide --wallet=<coldkey>` and `auth-provide --wallet=...` | The same unsigned request at startup. If it fails it is only logged. The provider keeps providing. |
| `provider sn-status` | Read-only dashboard. It reads the old-style wallet record, so it may say "not configured" even after you signed. **UNCONFIRMED** at the current head. Its rank lines are the bandwidth leaderboard, not Bittensor eligibility. |
| `provider bind-head`, `provider unbind-head` | The older head-tier path (hotkey, registrant and contract flags). It is not the current fleet flow. Do not use it. See [Head Tier](Bittensor-Head-Tier.md). |

The `urnet-tools wallet ...` group in this fork calls `provider wallet status|sign`. This fork's release `provider` does not have those two, so expect them to fail here. Use the 3.23-fix binary on your computer instead. This has not been re-run on every setup.

## One note on the miner image

The `:miner` image's `provider` can do `wallet set <coldkey> --coldkey_seed_file=<path>` (or `--message` with `--signature`). That is a signed consent, but for **one node's client only**, and it wins over the account-wide consent for that node. Most operators do not want that. It also needs a coldkey seed file, which must never sit on a node.

## Commands you do NOT need

| Command | Why you can skip it |
|---|---|
| `provider wallet hotkey ...` | Only for people who sign in with a hotkey across several operators. Today there is one operator. |
| `provide --all-operators`, `provider operators` | Multi-operator mode (miner image). Today there is one operator. |
| `provider fleet ...` | Head tier only (miner image). See [the head tier page](Bittensor-Head-Tier.md). |
| `provider claim-daemon`, `claim-daemon-v2`, `snclaim` | Optional claim automation and a low-level tool. |
| `provider claim` | Optional manual claim. |

## When you DO need more

- **You want the head tier** (your own seat on the subnet, paid directly). It is advanced, costs money up front, and only makes sense if you control many different internet address ranges. Most operators should not do it. Read [Bittensor Head Tier](Bittensor-Head-Tier.md) first.
- **You sign in with a hotkey** (`--auto-register` with a hotkey seed file). That makes a **new** URnetwork account owned by the hotkey. Your website consent does not cover it.
- **You want automatic claims.** Most people do not need this.

For the full picture and a FAQ, see [Bittensor Operations](Bittensor-Operations.md). For plain meanings of the words, see the [Glossary](Bittensor-Glossary.md).
