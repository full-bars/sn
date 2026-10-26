package miner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/proxy"
	"golang.org/x/term"

	"github.com/docopt/docopt-go"

	"github.com/urfoundation/sn/miner/bandwidth"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"

	"github.com/urfoundation/sn/clientauth"
	"github.com/urfoundation/sn/operatorlist"
)

const DefaultApiUrl = "https://api.bringyour.com"
const DefaultConnectUrl = "wss://connect.bringyour.com"

// this value is set via the linker, e.g.
// -ldflags "-X main.Version=$WARP_VERSION-$WARP_VERSION_CODE"
var Version string

func init() {
	// debug.SetGCPercent(10)

	// Must run before initGlog(): initGlog reads URNETWORK_PROFILE/
	// URNETWORK_RAMLOGS from the environment to make its one-shot,
	// unrepeatable decision about redirecting stdout/stderr to a ramlog.
	// See startup_env_seed.go for why this cannot just be a later call in
	// provide() the way every other control-socket key works.
	seedEnvFromControlState()
	initGlog()

	// initPprof()
}

// isLongRunningSubcommand reports whether args invoke the long-running
// provide (or auth-provide) command, as opposed to a one-shot CLI subcommand
// like `proxy remove-dead` or `proxy summary`. Used to decide whether
// stdout/stderr should be redirected into the ramlog: doing so for one-shot
// commands left them producing no visible output to the caller at all, since
// the redirect is process-wide, not scoped to `provide`.
//
// -h/--help/--version are excluded even on `provide`/`auth-provide`: this
// runs from init(), before docopt.ParseArgs gets a chance to handle those
// flags and exit, so a terminating invocation like `provide --help` would
// otherwise have its usage/version text redirected into the ramlog too.
func isLongRunningSubcommand(args []string) bool {
	if len(args) < 2 {
		return false
	}
	if args[1] != "provide" && args[1] != "auth-provide" {
		return false
	}
	for _, arg := range args[2:] {
		switch arg {
		case "-h", "--help", "--version":
			return false
		}
	}
	return true
}

// ramlogsRequested reports whether the environment selects the /dev/shm
// ramlog: URNETWORK_RAMLOGS=1 asks for it directly, and the lowmem and eco
// profiles carry it. The v33 provider reads the same two variables with the
// same meaning (provider/main.go initGlog).
func ramlogsRequested() bool {
	profile := os.Getenv("URNETWORK_PROFILE")
	return profile == "lowmem" || profile == "eco" || os.Getenv("URNETWORK_RAMLOGS") == "1"
}

// ramlogRedirectRequested is the whole initGlog decision: the ramlog is
// selected (profile or env) AND this invocation is the long-running provider.
// Everything else keeps the default routing on stderr — the terminal, docker
// logs or the journal — where the select lines ([net][s]select), the proxy
// pipeline and the rest of the glog/tlog output are read. A one-shot
// subcommand keeps the terminal even with URNETWORK_RAMLOGS=1: the redirect
// is process-wide, so redirecting `proxy summary` would leave it silent to
// its caller.
func ramlogRedirectRequested(args []string) bool {
	return ramlogsRequested() && isLongRunningSubcommand(args)
}

func initGlog() {
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")
	flag.Set("v", "0")

	// The default routing is stderr, and it stays that way for every
	// invocation that does not ask for the ramlog. Only the long-running
	// provide (or auth-provide) invocation redirects stdout/stderr into
	// /dev/shm/urnetwork.log, and only when the profile or URNETWORK_RAMLOGS
	// selects it (v33 provider/main.go initGlog).
	if ramlogRedirectRequested(os.Args) {
		fmt.Fprintf(os.Stderr, "[ramlogs] Logs redirected to RAM — view with: urnet-tools logs\n")
		initSHMLogger()
	}
}

// mainUsage returns the docopt usage string. Package-level (rather than
// inline in `main`) so tests can parse argv against the real usage.
// Docopt needs two spaces between an option declaration and its description.
func mainUsage() string {
	return fmt.Sprintf(
		`Connect provider.

The default URLs are:
    api_url: %s
    connect_url: %s

A network saved with "provider choose_network" replaces these defaults;
"provider choose_network --show" prints the network actually in effect.
"provide --all-operators" ignores both and mines every operator in the
operator list, by default %[3]s.

Usage:
    provider setup [--auth-code-file=<path>] [--yes] [--no-service] [--system] [-v...]
    provider status [--json] [-v...]
    provider doctor [-v...]
    provider service install [--system] [-v...]
    provider service uninstall [--system] [-v...]
    provider service (start | stop | restart | status) [--system] [-v...]
    provider service logs [--follow] [--system] [-v...]
    provider update [--check | --status | --on | --off | --auto] [-v...]
    provider auth ([<auth_code>] | --user_auth=<user_auth> [--password=<password>]) [-f]
    	[--api_url=<api_url>]
    	[--max-memory=<mem>]
    	[-v...]
    provider auth --operator=<domain> ([<auth_code>] | --user_auth=<user_auth> [--password=<password>] | --hotkey_seed_file=<path>) [-f]
        [--operators-url=<url>]
        [-v...]
    provider provide --all-operators [--operators-url=<url>] [--operators-refresh=<duration>]
        [--auto-register --hotkey_seed_file=<path> | --hotkey_seed_file=<path>]
        [--port=<port>] [--max-memory=<mem>] [--allow-client-registration]
        [-v...]
    provider operators [--operators-url=<url>] [-v...]
    provider provide [--port=<port>]
		[--close-report-domain=<path>]
		[--whole-work-capture=<path> --whole-work-capture-sha256=<hash>]
		[--require-whole-work-capture]
		[--original-contract-capture=<path> --original-contract-capture-sha256=<hash>]
		[--require-original-contract-capture]
		[--allow-client-registration | --adopt-legacy-provider-key]
		[--no-legacy-import]
		[--legacy-import-budget=<dur>]
		[--quarantine-rejected-sign-in]
        [--proxy_file=<proxy_file>] [--file=<file>]
        [--api_url=<api_url>]
        [--connect_url=<connect_url>]
        [--proxy_url=<proxy_url>]... [--url=<url>]... [--URL=<URL>]...
        [--proxy_url_refresh=<dur>] [--proxy_url_max=<n>]
        [--proxy_dead_cleanup_scope=<scope>] [--proxy_dead_cleanup_interval=<dur>]
        [--wallet=<coldkey_ss58> [--provider-jwt=<path>] [--wallet-from-epoch=<epoch> --wallet-through-epoch=<epoch>] [--coldkey_seed_file=<path> | --message=<text> --signature=<hex>]]
		[--test-egress-source-ip=<source_ip>]
        [--max-memory=<mem>]
        [-v...]
    provider auth-provide ([<auth_code>] | --user_auth=<user_auth> [--password=<password>]) [-f]
		[--close-report-domain=<path>]
		[--whole-work-capture=<path> --whole-work-capture-sha256=<hash>]
		[--require-whole-work-capture]
		[--original-contract-capture=<path> --original-contract-capture-sha256=<hash>]
		[--require-original-contract-capture]
		[--allow-client-registration | --adopt-legacy-provider-key]
		[--no-legacy-import]
		[--legacy-import-budget=<dur>]
    	[--port=<port>]
        [--proxy_file=<proxy_file>] [--file=<file>]
        [--api_url=<api_url>]
        [--connect_url=<connect_url>]
        [--proxy_url=<proxy_url>]... [--url=<url>]... [--URL=<URL>]...
        [--proxy_url_refresh=<dur>] [--proxy_url_max=<n>]
        [--proxy_dead_cleanup_scope=<scope>] [--proxy_dead_cleanup_interval=<dur>]
        [--wallet=<coldkey_ss58> [--provider-jwt=<path>] [--wallet-from-epoch=<epoch> --wallet-through-epoch=<epoch>] [--coldkey_seed_file=<path> | --message=<text> --signature=<hex>]]
		[--test-egress-source-ip=<source_ip>]
        [--max-memory=<mem>]
        [-v...]
    provider wallet set <coldkey_ss58> [--provider-jwt=<path> | --legacy-network-wallet] [--wallet-from-epoch=<epoch> --wallet-through-epoch=<epoch>] [--coldkey_seed_file=<path> | --message=<text> --signature=<hex>]
        [--api_url=<api_url>]
        [-v...]
    provider wallet challenge <coldkey_ss58> [--provider-jwt=<path> | --legacy-network-wallet] [--wallet-from-epoch=<epoch> --wallet-through-epoch=<epoch>]
        [--api_url=<api_url>]
        [-v...]
    provider wallet sign <coldkey_ss58> [--port=<port>] [--no-open]
        [--api_url=<api_url>]
        [-v...]
    provider wallet status
        [--api_url=<api_url>]
        [-v...]
    provider wallet hotkey challenge <coldkey_ss58> --hotkey_seed_file=<path>
        [--wallet-from-epoch=<epoch> --wallet-through-epoch=<epoch>] [--operators-url=<url>]
        [-v...]
    provider wallet hotkey set <coldkey_ss58> --hotkey_seed_file=<path>
        [--coldkey_seed_file=<path> | --message=<text> --signature=<hex>]
        [--wallet-from-epoch=<epoch> --wallet-through-epoch=<epoch>] [--operators-url=<url>]
        [-v...]
    provider wallet hotkey status [--hotkey_seed_file=<path>] [--operators-url=<url>] [-v...]
    provider sn-status [--json]
        [--api_url=<api_url>]
        [-v...]
    provider claim [--store-client=<key> | --provider-jwt=<path> | --legacy-coldkey=<coldkey_ss58>] [--epoch=<epoch>] [--rpc=<rpc_url>]... [--key_file=<key_file>] [--dry-run]
        [--api_url=<api_url>]
        [-v...]
    provider claim-daemon --config=<path>
        [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
        [-v...]
    provider claim-daemon-v2 --config=<path> [--v1-config=<path>] [--threshold-rao=<rao>] [--claim-now]
        [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
        [-v...]
    provider fleet init --chain_id=<n> --coordinator=<0xhex> --hotkey=<hotkey> [--netuid=<n>] [--fleet_id=<0xhex>] [--generation=<n>] [--client_seed_file=<path>] [--client_id=<hex>] [--member=<client_id>:<client_key>]...
        [-v...]
    provider fleet manifest --manifest=<path>
        [-v...]
    provider fleet register --manifest=<path> --hotkey_seed_file=<path> --coldkey_seed_file=<path> --substrate=<ws_url>...
        [--burn_limit_rao=<n>] [--fee_limit_rao=<n>] [--apply | --dry-run]
        [--provisional-runtime-compatibility=<profile> --runtime-observation-dir=<path>]
        [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
        [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
        [-v...]
    provider fleet publish --manifest=<path> --substrate=<ws_url>... --hotkey_seed_file=<path>
        [--provisional-runtime-compatibility=<profile> --runtime-observation-dir=<path>]
        [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
        [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
        [-v...]
    provider fleet bind --manifest=<path> --client_id=<hex> --client_seed_file=<path> --hotkey_seed_file=<path> --valid_from_epoch=<e> --valid_to_epoch=<e> --rpc=<rpc_url>... --relayer_key_file=<path> [--dry-run]
        [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
        [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
        [-v...]
    provider fleet sign --manifest=<path> [--client_id=<hex>] [--client_seed_file=<path>] [--hotkey_seed_file=<path>] --valid_from_epoch=<e> --valid_to_epoch=<e>
        [-v...]
    provider fleet status --manifest=<path> --client_id=<hex> --substrate=<ws_url>... --rpc=<rpc_url>...
        [--provisional-runtime-compatibility=<profile> --runtime-observation-dir=<path>]
        [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
        [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
        [-v...]
    provider fleet revoke --manifest=<path> --client_id=<hex> --client_seed_file=<path> --effective_epoch=<e> --rpc=<rpc_url>... --relayer_key_file=<path> [--dry-run]
        [--mainnet-runtime-authority=<path> --mainnet-runtime-authority-sha256=<hex>]
        [--durable-volumes=<path> --durable-volumes-sha256=<hash>]
        [-v...]
    provider proxy auth add [<key>] <proxy_user> <proxy_password> [-f]
    provider proxy auth remove [<key>] [--all]
    provider proxy add [<key_address>...] [--proxy_file=<proxy_file>] [-f]
    provider proxy remove [<key_address>...] [--all]
    provider proxy remove --match=<pattern> [--yes] [--preview]
    provider proxy clear
    provider proxy add-source <url>
    provider proxy paste [--file=<file>]
    provider proxy remove-source <url>
    provider proxy refresh [--force]
    provider proxy trim <count> [--preview]
    provider proxy summary
    provider proxy exclude [<pattern>] [--remove]
    provider proxy remove-dead [--degraded[=<duration>]] [--auth-failures=<N>] [--source=<source>] [--yes] [--preview]
    provider proxy activity
    provider direct [<state>]
    provider logs [-n <lines>]
    provider print-network-id <file>
    provider choose_network <api_url> <connect_url>
    provider choose_network --reset
    provider choose_network --show

Options:
	--close-report-domain=<path>       Optional original client-key policy domain for signed close evidence; absence or refusal leaves evidence unknown without stopping providing.
	--whole-work-capture=<path>        Exact public launch profile for independently signed whole-work requests and retained private outboxes.
	--whole-work-capture-sha256=<hash>  Independently reviewed sha256: digest of that original launch profile.
	--require-whole-work-capture       Refuse startup without the complete original capture profile; never allocate replacement client keys.
	--original-contract-capture=<path>  Exact approved original request/admission source profile with prepared private custody.
	--original-contract-capture-sha256=<hash>  Independently reviewed sha256: digest of the original contract source profile.
	--require-original-contract-capture  Refuse startup without the complete original contract source profile and retained provider identity.
    --durable-volumes=<path>          Exact external storage declaration; fleet writes require the owner-local schema.
    --durable-volumes-sha256=<hash>   Reviewed sha256: digest; claim daemons require the daemon-volume schema.
    --auth-code-file=<path>          With setup, read the auth code from this file instead of asking.
    --yes                            Take the default answer to every question: with setup, no prompts; with
                                     'proxy remove-dead', skip the per-category confirmation prompts.
    --no-service                     With setup, don't install the background service.
    --system                         With setup and service, a system-wide service run as root, with its
                                     state in /var/lib/urnetwork. Needs sudo.
    --json                           Output in JSON format for scripts and monitoring (status and other
                                     commands that support it).
    --follow                         With service logs, keep printing new lines.
    --check                          With update, only say whether a newer release is out.
    --status                         With update, show the automatic update settings.
    --on                             With update, turn automatic updates on (the default).
    --off                            With update, turn automatic updates off.
    --auto                           With update, one automatic update check, as the service timer runs it.
    -h --help                        Show this help and exit.
    --version                        Show version.
    -v                               Enable verbose mode. Repeat for higher levels:
                                     one -v means level 1, -vv means level 2, and so on.
    -f                               Force overwrite the JWT token store file or proxy value, if exists.
                                     By default, existing values will not be overwritten.
    --api_url=<api_url>              Specify a custom API URL to use.
	--allow-client-registration       Explicitly permit a new durable provider client operation; never replaces retained identity.
	--adopt-legacy-provider-key       Assert the retained legacy provider key is original; refresh only, no new client allocation.
	--no-legacy-import               Disable the automatic import of 3.23-fix saved provider identities from
	                                 .client_jwts.json; no legacy file is read for import and no sn state file is
	                                 written, so startup behaves exactly as before this feature.
	--legacy-import-budget=<dur>     Total time bound for the automatic 3.23-fix identity import (default 5m;
	                                 also URNETWORK_LEGACY_IMPORT_BUDGET). On the bound the import stops loudly,
	                                 keeps every slot already proven and leaves the rest for the next start.
	--config=<path>                    Strict release-1.0 daemon/component configuration.
	--v1-config=<path>                 claim-daemon-v2: also run the unchanged v1 claim daemon of this configuration in
	                                   the same process; both use one relayer key and share its nonce admission.
	--threshold-rao=<rao>              claim-daemon-v2: claim an account once its claimable balance reaches this many
	                                   rao. Default 1900000000 (1.9 α, so a claim's planning gas is at most 10%% of it);
	                                   never below the 15200000-rao (0.0152 α) transfer floor.
	--claim-now                        claim-daemon-v2: on start, claim every account with anything claimable, once.
    --connect_url=<connect_url>      Specify a custom connect URL to use.
    <api_url>                        API URL to save (https://, or http:// only for an explicit loopback host).
    <connect_url>                    Connect URL to save (wss://, or ws:// only for an explicit loopback host).
    --reset                          With choose_network, clear the saved network and revert to the main network.
    --show                           With choose_network, print the network currently in effect and exit.
    --user_auth=<user_auth>	         Login with a username.
    --password=<password>            Login with a password. If --user_auth is used, you will be prompted for your
    				                 password anyways, if you don't specify it using this option.
    -p --port=<port>                 Status server port [default: 0].
    --max-memory=<mem>               Set the maximum amount of memory in bytes, or the suffixes b, kib, mib, gib may be used [This is a soft limit].
    --no-open                        wallet sign: do not open the browser; only print the local address.
                                     wallet sign serves the page on 127.0.0.1 only, for 15 minutes, and uses the
                                     network login saved by "provider auth". --port is the page's port: 8765 when
                                     omitted or 0 (a taken port falls back to any free one).
    --wallet-from-epoch=<epoch>     First prospective earning epoch in the signed provider mapping.
    --wallet-through-epoch=<epoch>  Last inclusive earning epoch; at most 65536 epochs.
                                     Omit both to use the next epoch through the finite 65536-epoch window.
                                     With wallet hotkey: the global consent's epochs. Omitted, generation 1 earns
                                     from epoch 0 and a later one from the operators' current epoch plus 1, each
                                     through 65535 epochs later.
    --store-client=<key>            claim: use the client token of this identity from the provider state
                                     directory's .client_jwts.json identity store. <key> is the proxy address
                                     the identity was minted for, or "direct"; with several identities at one
                                     address pass the full key (the address, \x1f, then the user). Exactly one
                                     of --store-client, --provider-jwt and --legacy-coldkey.
    --provider-jwt=<path>           Select one retained provider JWT; default: .provider.jwt in the provider state directory.
    --legacy-network-wallet         Explicit network-wallet compatibility; never selects a provider wallet.
    --legacy-coldkey=<coldkey_ss58>   Read a legacy network-only proof for this original committed coldkey.
                                     Uses the network JWT; never substitutes the current network wallet.
    --wallet=<coldkey_ss58>          Also set the subnet claim wallet at startup, same as provider wallet set.
                                     A failure is logged and does not block providing.
    --coldkey_seed_file=<path>       With --wallet / wallet set / wallet hotkey set: the coldkey's 32-byte sr25519 seed (raw, or 64 hex
                                     chars with an optional 0x prefix) in a private file that is never created here.
                                     The CLI fetches the wallet challenge and signs it, proving the coldkey; refused
                                     unless the seed derives <coldkey_ss58>. The seed never leaves the host.
                                     With fleet register: the same seed grammar for the fleet coldkey that signs
                                     register_limit (the hotkey's owner); keep it off the mining hosts.
    --message=<text>                 With --wallet / wallet set: the challenge printed by "provider wallet challenge",
                                     signed elsewhere (a literal \n stands for a newline). Needs --signature.
                                     With wallet hotkey set: the statement "provider wallet hotkey challenge" printed.
    --signature=<hex>                The coldkey's 64-byte sr25519 signature over --message, hex (0x optional), made
                                     in the "substrate" signing context over the exact UTF-8 text (LF line endings,
                                     no trailing newline) or over that text wrapped in <Bytes>...</Bytes> (what a
                                     Polkadot extension signRaw of type "bytes" signs). Without --coldkey_seed_file
                                     or --message/--signature the set is sent unsigned, which the main network refuses.
    --burn_limit_rao=<n>             fleet register: maximum registration burn in rao passed to register_limit;
                                     the runtime rejects a higher live burn. Omitted: the burn observed at the read.
    --fee_limit_rao=<n>              fleet register: maximum native transaction fee in rao, checked with
                                     payment_queryInfo before broadcast [default: 10000000].
    --apply                          fleet register: sign, journal and broadcast. Without it the command is a dry
                                     run that reads the live burn economics and quotes the signed extrinsic.
	--quarantine-rejected-sign-in        When the network rejects the saved sign-in, set it aside and exit 75 so
	                                     the --all-operators supervisor signs in again and restarts this provider.
	--test-egress-source-ip=<source_ip>  Integration-harness-only IPv4 loopback source bound to both
	                                     platform control and provider exit sockets.
    <coldkey_ss58>                   Subnet claim wallet: an ss58 coldkey address (prefix 42).
    --epoch=<epoch>                  Epoch to fetch the subnet pool claim for. Defaults to the last
                                     finalized epoch, which is the epoch before the current one.
    --rpc=<rpc_url>                  EVM json-rpc endpoint used to check the payout root on-chain.
                                     May be repeated; endpoints are tried in order until one answers.
	--substrate=<ws_url>               Substrate websocket endpoint; repeatable ordered failover.
	--provisional-runtime-compatibility=<profile>  Explicit testnet consumed-runtime profile.
	--runtime-observation-dir=<path>   Absolute durable directory required with the provisional profile.
	--mainnet-runtime-authority=<path>  Independently reviewed exact mainnet fleet runtime authority document.
	--mainnet-runtime-authority-sha256=<hex>  Approved SHA-256 of the exact authority bytes (64 lowercase hex digits).
	--manifest=<path>                  Canonical urnetwork-fleet-manifest-v1 JSON file; fleet init prints one.
	--chain_id=<n>                     fleet init: EVM chain id of the fleet coordinator.
	--coordinator=<0xhex>              fleet init: 20-byte fleet coordinator contract address.
	--hotkey=<hotkey>                  fleet init: fleet hotkey public key as 0x 32-byte hex or ss58.
	--netuid=<n>                       fleet init: subnet netuid; default 25.
	--fleet_id=<0xhex>                 fleet init: 32-byte fleet id; default 32 random bytes.
	--generation=<n>                   fleet init: manifest generation; default 1.
	--member=<client_id>:<client_key>  fleet init: one member, the client id as 16-byte hex or uuid and the
	                                   client key as a 0x 32-byte Ed25519 public key; repeatable. Also
	                                   with --client_seed_file and --client_id it adds this device.
	                                   fleet init reads only local files, sends nothing and prints the
	                                   manifest as fleet manifest does; save it as the --manifest file.
	--client_id=<hex>                  Stable 16-byte UR client identity from the fleet manifest. fleet sign:
	                                   optional with --client_seed_file (the member holding that key).
	--client_seed_file=<path>          Raw or hex 32-byte Ed25519 client key seed (a provider's is
	                                   .provider.key in its state directory, e.g. ~/.urnetwork/.provider.key).
	                                   fleet sign signs locally and prints the {binding, client_signature}
	                                   payload the ur.io Top 200 page takes, plus hotkey_signature with
	                                   --hotkey_seed_file; it sends nothing and never prints a seed.
	--hotkey_seed_file=<path>          Hex/raw 32-byte sr25519 hotkey seed in a private file that is never created
	                                   here: the fleet hotkey, or the miner hotkey that signs in to operators
	                                   as a TAO wallet (--auto-register, auth --operator) and signs the hotkey
	                                   wallet consent and each operator's delegation (wallet hotkey).
	--all-operators                    provide: run one provider per operator in the operator list, each in its
	                                   own state directory operators/<domain> below the provider state directory.
	                                   Excludes --api_url, --connect_url, --provider-jwt, the wallet, capture,
	                                   close-report and test egress flags, and --adopt-legacy-provider-key.
	                                   With --hotkey_seed_file and a hotkey wallet chain, every authenticated
	                                   operator gets the chain and a delegation to its head at start and hourly.
	--operators-url=<url>              The operator list (https, or http only for loopback); default %[3]s.
	                                   The last good copy is kept as operators.yml in the provider state directory.
	--operators-refresh=<duration>     How often --all-operators refetches the operator list, as a Go duration
	                                   of at least 1m; default 1h. Providers follow the list as it changes.
	--auto-register                    With --all-operators: sign in with the hotkey of --hotkey_seed_file to each
	                                   operator that has no jwt, creating the hotkey's network there at the first
	                                   sign-in; no chain transaction. New operator directories need a new provider
	                                   client, so every provider also gets --allow-client-registration.
	--operator=<domain>                auth: authenticate to this operator from the operator list and write its
	                                   jwt in operators/<domain> below the provider state directory.
	--valid_from_epoch=<e>             First settlement epoch in which a binding is active.
	--valid_to_epoch=<e>               Last settlement epoch in which a binding is active.
	--effective_epoch=<e>              Future epoch at which a fleet revocation takes effect.
	--relayer_key_file=<path>          EVM transaction relayer key; it receives no binding ownership.
    --key_file=<key_file>            Path to a hex-encoded 32-byte secp256k1 EVM private key. When given,
                                     claim signs and submits the settlement-vault claim (via the
                                     sn/miner/onchain path) instead of only printing the calldata.
    --dry-run                        With --key_file / --relayer_key_file, stop at the eth_call preflight and
                                     send nothing; for fleet register the explicit dry run (the default).
                                     Without a key the claim command only verifies, so it has no effect.
    <key>                            Authentication key
    <proxy_user>                     SOCKS5 user
    <proxy_password>                 SOCKS5 password
    <key_address>                    SOCKS5 server as host:port, host:port:user:pass, host:port::, or key@host:port
    --proxy_file=<proxy_file>        A path to a file where each line contains on entry as host:port, host:port:user:pass, host:port::, or key@host:port.
                                     With provide/auth-provide it selects the file-backed proxy source that the
                                     running provider reads again on reload; also PROXY_FILE.
    --file=<file>                    Alias for --proxy_file (provide/auth-provide); with 'proxy paste', the input file.
    <url>                            A proxy list URL, for 'proxy add-source' and 'proxy remove-source'.
    <count>                          Max number of running proxies to keep, for 'proxy trim'. 0/off clears the cap.
    --match=<pattern>                Case-insensitive substring matched against proxy hosts (never port or
                                     credentials). Removes matches from the proxy list, proxy file, and URL
                                     cache, and excludes the pattern from future URL fetches. See 'proxy exclude'.
    <pattern>                        Host substring for 'proxy exclude' (add). With --remove, deletes the pattern.
                                     With no pattern, 'proxy exclude' lists the active patterns.
    --force                          With 'proxy refresh', bypass the 8-hour warmup protection gate.
    --preview                        With 'proxy trim' or 'proxy remove-dead', list what would change and write nothing.
    --remove                         With 'proxy exclude', delete the pattern instead of adding it.
    --auth-failures=<N>              With 'proxy remove-dead', also remove proxies with at least N auth failures
                                     per day of uptime.
    --source=<source>                With 'proxy remove-dead', only consider proxies from this source: url, file, or internal.
    --proxy_url=<proxy_url>          Fetch SOCKS5 proxies from this URL (repeatable). Also --url / --URL / PROXY_URL (comma-separated).
                                     List lines are host:port, host:port:user:pass, socks5://host:port or socks5://user:pass@host:port.
    --proxy_url_refresh=<dur>        How often to re-fetch the --proxy_url sources and add new entries (default 1h).
                                     Also PROXY_URL_REFRESH. Minimum 10s.
    --proxy_url_max=<n>              Cap on total proxies sourced from --proxy_url (default 500). 0 = unlimited.
                                     Also PROXY_URL_MAX.
    --proxy_dead_cleanup_scope=<scope>  Automatic dead-proxy cleanup scope: none, url, or all (default url).
                                     Also PROXY_DEAD_CLEANUP_SCOPE.
    --proxy_dead_cleanup_interval=<dur>  How often automatic cleanup runs, when the scope is not none
                                     (default 6h). Also PROXY_DEAD_CLEANUP_INTERVAL. Minimum 1m.
    <state>                          Direct IP providing state: on | off | status. If omitted, reports current state.
    -n <lines>                       Number of lines to show from the end of the log [default: 0].`,
		DefaultApiUrl,
		DefaultConnectUrl,
		operatorlist.DefaultUrl,
	)
}

// parseLegacyImportBudget returns the total proving budget of the automatic
// 3.23-fix identity import: --legacy-import-budget when given, else
// URNETWORK_LEGACY_IMPORT_BUDGET, else 0 (the package default, 5m). A
// malformed value is refused at parse time, like --max-memory: an operator
// who asked for a specific bound must not silently get another.
func parseLegacyImportBudget(opts docopt.Opts) time.Duration {
	value, _ := opts.String("--legacy-import-budget")
	if strings.TrimSpace(value) == "" {
		value = strings.TrimSpace(os.Getenv("URNETWORK_LEGACY_IMPORT_BUDGET"))
	}
	if value == "" {
		return 0
	}
	budget, err := time.ParseDuration(value)
	if err != nil || budget <= 0 {
		panic(fmt.Errorf("Bad --legacy-import-budget argument: %s", value))
	}
	return budget
}

// Run is the miner CLI entry point (the executable lives at cli/miner). It takes
// the argument slice (os.Args[1:]) so it can be driven from tests. The miner is
// the subnet's provider: it runs the provide/proxy/auth flows plus the on-chain
// wallet / claim / fleet actions (formerly connect/provider).
func Run(args []string) {
	defer recoverCliPanic(args, os.Stderr)
	if code, done := minerServiceRun(args); done {
		os.Exit(code)
	}
	typed := args
	args, done, code := cliFront(args, os.Stdout, os.Stderr)
	if done {
		os.Exit(code)
	}
	// docopt alone would answer these with the whole usage text
	if err := operatorArgsError(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	opts, err := cliParser(args, typed, os.Stdout, os.Stderr).ParseArgs(mainUsage(), args, RequireVersion())

	if err != nil {
		panic(err)
	}
	providing, _ := opts.Bool("provide")
	authProviding, _ := opts.Bool("auth-provide")
	if providing || authProviding {
		path, _ := opts.String("--whole-work-capture")
		digest, _ := opts.String("--whole-work-capture-sha256")
		required, _ := opts.Bool("--require-whole-work-capture")
		profile, err := ReadProviderWorkCaptureProfile(context.Background(), path, digest, required)
		allowRegistration, _ := opts.Bool("--allow-client-registration")
		if err != nil || profile != nil && allowRegistration {
			// Complete-profile refusal precedes auth-provide authentication and
			// the optional startup wallet write as well as provider allocation.
			fmt.Fprintln(os.Stderr, "whole-work launch requires its reviewed profile, original provider identity and private outbox; no provider started")
			os.Exit(1)
		}
		contractPath, _ := opts.String("--original-contract-capture")
		contractDigest, _ := opts.String("--original-contract-capture-sha256")
		contractRequired, _ := opts.Bool("--require-original-contract-capture")
		contractProfile, err := ReadProviderContractCaptureProfile(context.Background(), contractPath, contractDigest, contractRequired)
		if err != nil || contractProfile != nil && allowRegistration {
			fmt.Fprintln(os.Stderr, "original contract launch requires its approved source profile and retained provider identity; no provider started")
			os.Exit(1)
		}
		// Host limits are graded once, before any provider starts. A node with
		// a file-descriptor ceiling too low for its sockets, or a conntrack
		// table too small for them, looks healthy until it silently stops
		// accepting clients. The check only reads, never exits, and reports
		// nothing on a tuned host (system_audit.go).
		startStartupHostAudit()
	}

	for _, command := range []struct {
		name string
		run  func(docopt.Opts, io.Writer, io.Writer) int
	}{
		// service first: its subcommands reuse the words status and start
		{"service", serviceCmd},
		{"setup", setupCmd},
		{"status", func(opts docopt.Opts, stdout io.Writer, _ io.Writer) int { return statusCmd(opts, stdout) }},
		{"doctor", func(opts docopt.Opts, stdout io.Writer, _ io.Writer) int { return doctorCmd(opts, stdout) }},
		{"update", updateCmd},
	} {
		if chosen, _ := opts.Bool(command.name); chosen {
			os.Exit(command.run(opts, os.Stdout, os.Stderr))
		}
	}

	if proxy, _ := opts.Bool("proxy"); proxy {
		if auth, _ := opts.Bool("auth"); auth {
			if add, _ := opts.Bool("add"); add {
				proxyAuthAdd(opts)
			} else if remove, _ := opts.Bool("remove"); remove {
				proxyAuthRemove(opts)
			}
		} else if addSource, _ := opts.Bool("add-source"); addSource {
			proxyAddSource(opts)
		} else if paste, _ := opts.Bool("paste"); paste {
			proxyPaste(opts)
		} else if removeSource, _ := opts.Bool("remove-source"); removeSource {
			proxyRemoveSource(opts)
		} else if exclude, _ := opts.Bool("exclude"); exclude {
			proxyExclude(opts)
		} else if add, _ := opts.Bool("add"); add {
			proxyAdd(opts)
		} else if removeDead, _ := opts.Bool("remove-dead"); removeDead {
			proxyRemoveDead(opts)
		} else if remove, _ := opts.Bool("remove"); remove {
			proxyRemove(opts)
		} else if clear, _ := opts.Bool("clear"); clear {
			proxyClear(opts)
		} else if refresh, _ := opts.Bool("refresh"); refresh {
			proxyRefresh(opts)
		} else if activity, _ := opts.Bool("activity"); activity {
			proxyActivity()
		} else if summary, _ := opts.Bool("summary"); summary {
			proxySummary()
		} else if trim, _ := opts.Bool("trim"); trim {
			proxyTrim(opts)
		}
	} else if wallet, _ := opts.Bool("wallet"); wallet {
		if hotkey, _ := opts.Bool("hotkey"); hotkey {
			hotkeyWalletCmd(opts)
		} else if set, _ := opts.Bool("set"); set {
			walletSet(opts)
		} else if challenge, _ := opts.Bool("challenge"); challenge {
			walletChallenge(opts)
		} else if sign, _ := opts.Bool("sign"); sign {
			walletSign(opts)
		} else if status, _ := opts.Bool("status"); status {
			walletStatus(opts)
		}
	} else if snStatus, _ := opts.Bool("sn-status"); snStatus {
		snStatusCmd(opts)
	} else if claim_, _ := opts.Bool("claim"); claim_ {
		claim(opts)
	} else if claimDaemon, _ := opts.Bool("claim-daemon"); claimDaemon {
		if err := runClaimDaemon(minerStorageContext(context.Background(), opts), fleetOpt(opts, "--config")); err != nil {
			panic(err)
		}
	} else if claimDaemonV2, _ := opts.Bool("claim-daemon-v2"); claimDaemonV2 {
		if err := runClaimDaemonV2Command(minerStorageContext(context.Background(), opts), opts); err != nil {
			panic(err)
		}
	} else if fleet_, _ := opts.Bool("fleet"); fleet_ {
		if err := fleetCommand(opts); err != nil {
			panic(err)
		}
	} else if auth_, _ := opts.Bool("auth"); auth_ {
		if _, err := opts.String("--operator"); err == nil {
			operatorAuthCmd(opts)
		} else {
			if err := auth(opts); err != nil {
				fmt.Printf("%s\n", err)
				os.Exit(1)
			}
		}
	} else if provide_, _ := opts.Bool("provide"); provide_ {
		if allOperators, _ := opts.Bool("--all-operators"); allOperators {
			provideAllOperatorsCmd(opts)
		} else {
			provideWith(opts, 0 < len(typed) && typed[0] == "start")
		}
	} else if authProvide, _ := opts.Bool("auth-provide"); authProvide {
		if err := auth(opts); err != nil {
			fmt.Printf("%s\n", err)
			os.Exit(1)
		}
		provide(opts)
	} else if logs, _ := opts.Bool("logs"); logs {
		providerLogs(opts)
	} else if printNetworkId, _ := opts.Bool("print-network-id"); printNetworkId {
		printNetworkIdCmd(opts)
	} else if chooseNetwork, _ := opts.Bool("choose_network"); chooseNetwork {
		if err := chooseNetworkCmd(opts); err != nil {
			fmt.Printf("%s\n", err)
			os.Exit(1)
		}
	} else if direct_, _ := opts.Bool("direct"); direct_ {
		cmdDirect(opts)
	} else if operators, _ := opts.Bool("operators"); operators {
		operatorsCmd(opts)
	}
}

// Authentication shares the selected state directory with provider startup and claims.
// A custody refusal is the host's directory, not an invariant: it is returned
// as a command failure with the store's own actionable message.
func auth(opts docopt.Opts) error {
	jwtPath, err := providerStatePath("jwt")
	if err != nil {
		panic(err)
	}
	if !confirmJwtOverwrite(opts, jwtPath) {
		return nil
	}

	apiUrl, err := resolveApiUrl(opts)
	if err != nil {
		return fmt.Errorf("network config error: %s", err)
	}

	// auth is short lived: only an explicit --max-memory sizes it
	applyProviderProcessMemory(newProviderAuthMemoryPlan(parseProviderMaxMemory(opts)))

	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(event.Ctx())
	defer cancel()

	byJwt, err := networkLogin(ctx, opts, apiUrl, terminalLoginPrompts())
	if err != nil {
		panic(err)
	}
	if byJwt != "" {
		if err := clientauth.WriteNetworkTokenWithContext(ctx, jwtPath, byJwt); err != nil {
			return fmt.Errorf("network token was not written: %w", err)
		}
		fmt.Printf("Jwt written to %s\n", jwtPath)
		if cliIsTerminal(os.Stdout) {
			signIn := readMinerSignIn()
			if signIn.networkName != "" {
				fmt.Printf("Signed in to network %q.\n", signIn.networkName)
			}
			fmt.Println("Next: provider start")
		}
	}
	return nil
}

// Without -f, an existing jwt is replaced only when the user answers y.
func confirmJwtOverwrite(opts docopt.Opts, jwtPath string) bool {
	if _, err := os.Stat(jwtPath); errors.Is(err, os.ErrNotExist) {
		return true
	}
	if force, _ := opts.Bool("-f"); force {
		return true
	}
	fmt.Printf("%s exists. Overwrite? [yN]\n", jwtPath)
	reader := bufio.NewReader(os.Stdin)
	confirm, _ := reader.ReadString('\n')
	return strings.ToLower(strings.TrimSpace(confirm)) == "y"
}

// Reads what the command line left out of a login. Tests replace the terminal.
type loginPrompts struct {
	password func() (string, error)
	authCode func() (string, error)
}

// contractProxyDescription labels the path a contract ran on. The miner has no
// proxy index, so the slot index and the address carry the same information the
// relay's proxy[i] label did, and the direct slot says so.
func contractProxyDescription(slot uint64, proxyAddr string) string {
	if proxyAddr == "" {
		return "proxy[0] (direct)"
	}
	return fmt.Sprintf("proxy[%d] (%s)", slot, proxyAddr)
}

func terminalLoginPrompts() loginPrompts {
	stdin := int(syscall.Stdin)
	return loginPrompts{
		password: func() (string, error) {
			fmt.Print("Enter password: ")
			passwordBytes, err := readHiddenInterruptible(stdin)
			if err != nil {
				return "", err
			}
			fmt.Printf("\n")
			return string(passwordBytes), nil
		},
		authCode: func() (string, error) {
			return readAuthCode(os.Stdin, os.Stdout, term.IsTerminal(stdin), func() ([]byte, error) {
				return readHiddenInterruptible(stdin)
			})
		},
	}
}

// The network jwt from a user and password, or else from an auth code,
// prompting for the password or code when the command line has none.
func networkLogin(ctx context.Context, opts docopt.Opts, apiUrl string, prompts loginPrompts) (string, error) {
	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)
	defer clientStrategy.Close()
	api := sdk.NewApi(ctx, clientStrategy, apiUrl)
	defer func() {
		_ = api.CloseAndWait(context.Background())
	}()

	if userAuth, err := opts.String("--user_auth"); err == nil {
		// an absent --password is not a string, so it prompts too
		password, err := opts.String("--password")
		if err != nil || password == "" {
			password, err = prompts.password()
			if err != nil {
				return "", err
			}
		}
		loginResult, err := api.AuthLoginWithPasswordSyncWithContext(ctx, &sdk.AuthLoginWithPasswordArgs{
			UserAuth: userAuth,
			Password: password,
		})
		if err != nil {
			return "", err
		}
		if loginResult.Error != nil {
			return "", fmt.Errorf("%s", loginResult.Error.Message)
		}
		if loginResult.VerificationRequired != nil {
			return "", fmt.Errorf("Verification required for %s. Use the app or web to complete account setup.", loginResult.VerificationRequired.UserAuth)
		}
		if loginResult.Network == nil {
			return "", errors.New("login returned no network")
		}
		return loginResult.Network.ByJwt, nil
	}

	authCode, _ := opts.String("<auth_code>")
	if authCode == "" {
		var err error
		authCode, err = prompts.authCode()
		if err != nil {
			return "", err
		}
	}
	authCodeLoginResult, err := api.AuthCodeLoginSyncWithContext(ctx, &sdk.AuthCodeLoginArgs{
		AuthCode: authCode,
	})
	if err != nil {
		return "", err
	}
	if authCodeLoginResult.Error != nil {
		return "", fmt.Errorf("%s", authCodeLoginResult.Error.Message)
	}
	return authCodeLoginResult.Jwt, nil
}

// testEgressDialContext returns the narrow source-bind seam used by
// sim-testnet to run several independently attributable provider exits on one
// Linux host. Requiring an IPv4 loopback address makes the flag incapable of
// selecting a production interface or exporting traffic outside the host.
func testEgressDialContext(opts docopt.Opts) (*connect.DialContextSettings, error) {
	raw, err := opts.String("--test-egress-source-ip")
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	settings, err := testEgressDialContextForIP(raw)
	if err != nil {
		return nil, fmt.Errorf("--test-egress-source-ip must be an IPv4 loopback address")
	}
	return settings, nil
}

// applyProviderMemoryTarget installs one provider's device target (see
// newProviderMemoryPlan). A nonpositive target deliberately preserves the
// SDK's positive per-device default: replacing it with zero would make
// unrelated SOCKS-backed providers share Connect's process-wide transport
// budget.
func applyProviderMemoryTarget(
	settings *sdk.DeviceLocalSettings,
	deviceMemoryTargetByteCount connect.ByteCount,
) {
	if deviceMemoryTargetByteCount <= 0 {
		return
	}
	settings.MemoryTargetByteCount = sdk.ByteCount(deviceMemoryTargetByteCount)
}

func provide(opts docopt.Opts) {
	provideWith(opts, false)
}

// provideWith is provide, or provider start when start is set: start asks
// before registering a new device and, in a terminal, reports in plain lines.
func provideWith(opts docopt.Opts, start bool) {
	// Pet systemd's watchdog before anything can block. A unit that sets
	// WatchdogSec= kills a service that never checks in, so without this a
	// healthy provider is restarted at every interval (Result=timeout).
	startSystemdWatchdog()
	if !providePreflight(opts, start, os.Stdin, os.Stdout, os.Stderr) {
		os.Exit(minerExitNeedsSetup)
	}
	var ui *startTerminalUi
	if start && cliIsTerminal(os.Stdout) && cliIsTerminal(os.Stderr) && cliVerbosity(os.Args[1:]) == 0 && os.Getenv("PROVIDER_DEBUG") == "" {
		ui = newStartTerminalUi(os.Stdout)
	}
	var out io.Writer = os.Stdout
	if ui != nil {
		out = ui
	}
	port, _ := opts.Int("--port")

	apiUrl, err := resolveApiUrl(opts)
	if err != nil {
		fmt.Printf("network config error: %s\n", err)
		os.Exit(1)
	}

	connectUrl, err := resolveConnectUrl(opts)
	if err != nil {
		fmt.Printf("network config error: %s\n", err)
		os.Exit(1)
	}
	testEgressDialer, err := testEgressDialContext(opts)
	if err != nil {
		panic(err)
	}

	// HotSwap candidate: a child spawned by a handoff inherits the parent's
	// IPC channel (fd 3 on Unix, the named pipe in the environment on Windows).
	// It runs its pre-flight here, before any provider state is touched, and
	// announces READY only once the account token and the API host check out;
	// the parent answers TAKEOVER when it is ready to yield. A failed
	// pre-flight exits 2 so a supervisor restart is the visible outcome rather
	// than a second provider starting while the parent still holds the
	// identities.
	hotSwapIPC, isHotSwapCandidate := getHotSwapChildIPC()
	if isHotSwapCandidate {
		if err := runHotSwapChildHandshake(hotSwapIPC, opts, apiUrl); err != nil {
			fmt.Fprintf(os.Stderr, "[hotswap] candidate pre-flight failed: %s\n", err)
			hotSwapIPC.Close()
			os.Exit(2)
		}
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	defer stopSignals()
	// on Windows, the service's supervisor asks through an inherited event
	ctx, stopService := minerServiceStopContext(ctx)
	defer stopService()

	// A hot swap execs in place: the pid survives and the previous image's
	// proxy.lock would read as held-by-a-live-holder, blocking every lock
	// consumer until the max age ages it out. Clear it before the manager
	// loops or any command can take the lock.
	cleanStaleSelfProxyLock()
	// The manager owns the desired proxy set (the legacy <state>/proxy config
	// plus any URL-sourced cache) and keeps the state current while the
	// providers run.
	// The baseline recorder writes its start mark when the manager starts, so a
	// persisted `set baseline off` has to be in force before then. This is a
	// read-only peek at the same file the full load below uses, so the queued
	// overrides and the load's error reporting stay exactly as they were.
	applyPersistedBaselineFlag()

	proxyManager := newProxyManager(proxyManagerConfigFromOpts(opts, apiUrl))
	allProxySettings, _, err := proxyManager.start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy state error: %s\n", err)
		os.Exit(1)
	}
	// Registered BEFORE proxyManager.stop() on purpose: defers run LIFO, so
	// this final drain runs AFTER the manager's loops have quiesced. A reload
	// or trim evaluation inside desired()→noteTrimSelection while the manager
	// is stopping queues a line after the flusher's own ctx.Done drain has
	// already returned, and the startup drain below is long past; registering
	// the drain after the stop defer would run it first and lose that line.
	defer writeDeferredCrit()
	defer proxyManager.stop()
	// The manager's desired() read is side-effect free, so the h3 ranking over
	// what this run serves is noted here, on the apply path: it must be in
	// place before any identity below builds its transport, and a count set
	// later ranks this real set instead of an empty one. No relauncher is
	// published yet, so nothing is restarted: this is the startup half.
	applyH3CandidateSet(allProxySettings, resolveH3Mode(globalControlState, currentProxyResolution()))
	// Startup holds no reload lock, so any critical-log line the cap lookup
	// queued (an unparseable proxy_trim) is written straight away here rather
	// than waiting for a reload that may be hours away.
	writeDeferredCrit()
	// Lines queued AFTER startup have no other consumer: the control socket's
	// oom-cap apply and every reload that calls noteTrimSelection can queue
	// one, and the startup drain above runs only once. Flush on a cadence and
	// once when ctx ends; the deferred drain registered before
	// proxyManager.stop() above covers whatever the manager queues while it
	// is stopping.
	go runDeferredCritFlusher(ctx, deferredCritFlushInterval)
	providerCount := len(allProxySettings)
	if providerCount == 0 {
		providerCount = 1
	}

	// sized per provider (provider_memory.go); an absent --max-memory has no
	// ceiling: the target is derived from the host memory
	maxMemory := parseProviderMaxMemory(opts)
	memoryPlan := newProviderMemoryPlan(
		maxMemory,
		hostMemoryByteCount(),
		providerCount,
	)
	applyProviderProcessMemory(memoryPlan)
	// v33's pool auto-size runs before any provider is built; see
	// applyPoolAutoSize for the pinned-connect adaptation.
	applyPoolAutoSize(maxMemory)

	allowClientRegistration, _ := opts.Bool("--allow-client-registration")
	adoptLegacyProviderKey, _ := opts.Bool("--adopt-legacy-provider-key")
	noLegacyImport, _ := opts.Bool("--no-legacy-import")
	legacyImportBudget := parseLegacyImportBudget(opts)
	// internal: an operator child of provide --all-operators --auto-register
	quarantineRejectedSignIn, _ := opts.Bool("--quarantine-rejected-sign-in")
	domainPath, _ := opts.String("--close-report-domain")
	domainHash, domainErr := ReadProviderCloseReportDomain(domainPath)
	if domainErr != nil {
		fmt.Fprintf(os.Stderr, "signed close evidence unavailable: %v\n", domainErr)
	}
	settings := providerRunSettings{apiUrl: apiUrl, connectUrl: connectUrl, port: port, proxySettings: allProxySettings, proxySourceOf: proxyManager.proxySourceOf(), memoryPlan: memoryPlan, maxMemory: maxMemory, testEgressDialer: testEgressDialer, allowClientRegistration: allowClientRegistration, adoptLegacyProviderKey: adoptLegacyProviderKey, noLegacyImport: noLegacyImport, legacyImportBudget: legacyImportBudget, quarantineRejectedSignIn: quarantineRejectedSignIn, hotSwapIPC: hotSwapIPC, hotSwapCandidate: isHotSwapCandidate, opts: opts}
	settings.wallet, _ = opts.String("--wallet")
	if settings.wallet != "" {
		settings.walletProof, err = snWalletProofFromOpts(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "subnet wallet not set: %v; continuing to provide\n", err)
			settings.wallet = ""
		}
	} else if misuse := snProvideWalletMisuse(opts); misuse != "" {
		fmt.Fprintln(os.Stderr, misuse)
	}
	settings.closeReportDomainHash = domainHash
	settings.workCapturePath, _ = opts.String("--whole-work-capture")
	settings.workCaptureSha256, _ = opts.String("--whole-work-capture-sha256")
	settings.requireWorkCapture, _ = opts.Bool("--require-whole-work-capture")
	settings.contractCapturePath, _ = opts.String("--original-contract-capture")
	settings.contractCaptureSha256, _ = opts.String("--original-contract-capture-sha256")
	settings.requireContractCapture, _ = opts.Bool("--require-original-contract-capture")
	// The startup disk audit is bounded (256 MiB and 3 s) and runs in its own
	// goroutine, so startup never waits for it. It writes disk-bench.json into
	// the state dir, which is what the metrics seam reports on the next scrape;
	// nothing on this path reads the result back.
	startStartupDiskAudit()
	// A complete-profile launch must report refusal to its supervisor, and a
	// child that set its rejected network sign-in aside says so by its exit
	// code. The owned run has joined every child before this exit decision.
	if ui != nil {
		ui.begin()
	}
	err = settings.run(ctx, out)
	if ui != nil {
		ui.finish(err, ctx.Err() != nil)
	}
	if code, ok := providerRunExitCode(err); ok {
		stopSignals()
		// A refused or quarantined startup is a fatal exit: the process is
		// about to disappear with the evidence of what the run saw. The
		// bundle is bounded and best effort, and cannot change the code.
		captureIncidentBundle(incidentTriggerFatal, fmt.Sprintf("exit %d: %v", code, err), nil)
		if errors.Is(err, errProviderStartupRefused) {
			// The documented startup_recovery_required JSON is emitted by the
			// run, but its diagnostic stream is best effort (a redirected
			// regular-file stdout refuses it). Name the refusal on stderr too:
			// a refused startup that leaves no trace reads as a clean exit.
			fmt.Fprintf(os.Stderr, "provider startup refused: %v\n", err)
			if hint := registrationRefusalHint(err); hint != "" {
				fmt.Fprintln(os.Stderr, hint)
			}
		}
		exitProviderRun(code)
		// The exit seam only returns under test.
		return
	}
	if err != nil && ctx.Err() == nil && (settings.requireWorkCapture || settings.workCapturePath != "" || settings.workCaptureSha256 != "" || settings.requireContractCapture || settings.contractCapturePath != "" || settings.contractCaptureSha256 != "") {
		stopSignals()
		captureIncidentBundle(incidentTriggerFatal, "capture launch refused: "+err.Error(), nil)
		fmt.Fprintln(os.Stderr, "provider original capture launch refused; restore approved profiles, retained identity and prepared custody")
		exitProviderRun(1)
		return
	}
	if start && err != nil && ctx.Err() == nil {
		// provide has always exited 0 here; start tells a service it failed
		stopSignals()
		os.Exit(1)
	}
}

// Each invocation owns its output, status server and provider children. The
// finite CLI setup above retains its existing output and validation behavior.
func (self providerRunSettings) run(parent context.Context, writer io.Writer) (returnErr error) {
	output, err := newProviderDiagnostics(parent, writer)
	if err != nil {
		return err
	}
	hooks, _ := parent.Value(providerDiagnosticHooksKey{}).(providerDiagnosticHooks)
	defer func() {
		closeErr := output.close()
		returnErr = errors.Join(returnErr, closeErr)
		if hooks.afterClose != nil {
			hooks.afterClose(output, closeErr)
		}
	}()
	if hooks.afterCreate != nil {
		hooks.afterCreate(output)
	}
	ctx, rawCancel := context.WithCancel(parent)
	// Record what first cancelled the run for the SIGNAL line in events.log:
	// whichever caller cancels first (signal handler goroutine, control-socket
	// shutdown, quarantine, or a failed worker) leaves its stack, the same
	// first-canceller record the v33 provider keeps in provideSetupSignals.
	cancelSource := &runCancelSource{}
	cancel := func() {
		cancelSource.record()
		rawCancel()
	}
	defer cancel()
	// The retention event writer is process-wide and started lazily from the
	// ingest path, so it cannot take this context as an argument. Register it
	// here, before any provider is built: a cancelled run then ends the
	// writer's supervisor, which is what closes its done channel — otherwise a
	// panicking writer would sit in the supervisor's backoff while shutdown or
	// the hotswap drain waits for it.
	setRetentionEventRunContext(ctx)
	// The control surface's run state: start time, the previous run's marker
	// (restart reason and version), and a clean-exit marker for the next
	// start. Declared before the child machinery so a clean return marks the
	// run; a crash or kill leaves no marker, which reads as "unclean".
	beginControlRun(time.Now())
	defer func() {
		// A run that returns a non-nil error skips finishControlRun below; the
		// caller then leaves through os.Exit, which skips whatever the caller
		// deferred. Write the counts here, bounded, so the failures of a run
		// that is already dying are recorded instead of disappearing with it.
		// On a clean return this is a no-op: finishControlRun flushes the same
		// store a few lines later.
		flushPersistentErrorsAtExit()
		if returnErr == nil {
			finishControlRun("clean")
			// events.log keeps the clean shutdown across a reboot that wipes
			// /dev/shm; the line matches the v33 provide teardown.
			critLog("PROVIDER EXIT: normal shutdown (code=0)")
		}
	}()
	if err := validateProviderRegistrationSlots(self.proxySettings); err != nil {
		output.observe(providerStartupRecoveryRequired, 0, false, err, 0, nil)
		return startupRefusal(ctx, err)
	}
	providers := self.proxySettings
	if len(providers) == 0 {
		providers = []*connect.ProxySettings{nil}
	}
	// Publish the launched proxy slots to the control surface (P2b): the
	// running provider is the only process that knows its running set, which
	// is what `proxy trim --preview` asks it for. The direct slot has no key.
	controlRuntime.setRunningProxies(providerSlotKeys(providers))
	slotIdentities := newProviderSlotIdentities(providers)
	workProfile, err := ReadProviderWorkCaptureProfile(ctx, self.workCapturePath, self.workCaptureSha256, self.requireWorkCapture)
	if err != nil {
		output.observe(providerWorkCaptureRequired, 0, false, err, 0, nil)
		return err
	}
	contractProfile, err := ReadProviderContractCaptureProfile(ctx, self.contractCapturePath, self.contractCaptureSha256, self.requireContractCapture)
	if err != nil {
		output.observe(providerContractCaptureRequired, 0, false, err, 0, nil)
		return err
	}
	captureEvent := providerWorkCaptureRequired
	if contractProfile != nil {
		captureEvent = providerContractCaptureRequired
	}
	if workProfile != nil || contractProfile != nil {
		slots := make([]string, 0, len(providers))
		for _, proxy := range providers {
			slots = append(slots, providerRegistrationSlot(slotIdentities, proxy))
		}
		if err := workProfile.validateRole(self.apiUrl, slots, self.closeReportDomainHash); err != nil {
			output.observe(providerWorkCaptureRequired, 0, false, err, 0, nil)
			return err
		}
		if err := contractProfile.validateRole(self.apiUrl, slots, self.closeReportDomainHash, workProfile); err != nil {
			output.observe(providerContractCaptureRequired, 0, false, err, 0, nil)
			return err
		}
		if self.allowClientRegistration {
			err := errors.New("original capture launch requires retained provider identity and cannot allocate registration")
			output.observe(captureEvent, 0, false, err, 0, nil)
			return err
		}
	}
	progressMembers := make([]providerProgressConfigMember, 0, len(providers))
	for _, proxy := range providers {
		progressMembers = append(progressMembers, providerProgressConfigMember{Slot: providerRegistrationSlot(slotIdentities, proxy), ApiUrl: self.apiUrl, ConnectUrl: self.connectUrl})
	}
	progress, err := newProviderProgressOwner("standalone", progressMembers)
	if err != nil {
		return err
	}
	defer progress.close()
	// Per-slot sessions: the control snapshot counts connected/ready slots
	// through this owner.
	controlRuntime.setProviderProgress(progress)
	// provider status reads this; it stops before progress closes
	defer startMinerStatusFile(ctx, progress)()
	status, err := newProviderStatusServer(self.port, output, cancel, progress)
	if err != nil {
		output.observe(providerStatusFailed, 0, false, err, 0, nil)
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, status.close()) }()

	// The control socket is the daemon surface the unmodified urnet-tools and
	// urnet-docker clients drive. It starts beside the status server,
	// before any provider child runs, so a command that arrives during startup
	// is served. The control state is the miner's record of every socket-set
	// setting; the queue urnet-tools wrote while the provider was stopped is
	// merged once here.
	controlState, err := loadControlState()
	if err != nil {
		output.observe(providerStartupRecoveryRequired, 0, false, err, 0, nil)
		return startupRefusal(ctx, err)
	}
	mergePendingControlOverrides(controlState)
	globalControlState = controlState
	// The profile tiers' Go runtime knobs (profiles/memory, PM-04) and the
	// once-per-start resource check are one ordered step
	// (applyStartupMemoryTuning). That step replays a persisted
	// `set gomemlimit` and `set gogc` first, so the operator's pins survive a
	// restart and the tiers skip their defaults for them (the provider's
	// applyPersistedRuntimeTuning, in the miner's order: after the memory plan
	// and the control-state load, before applyProfileProcessTuning). The check
	// then reads the soft limit actually in force, and runs before the launch
	// loop with the pool size this start opens (`proxy trim` already applied).
	// Warnings only: nothing here changes a limit or refuses a start.
	applyStartupMemoryTuning(self.maxMemory, len(self.proxySettings))
	// Apply the resolved h3 setting once, before any provider builds its
	// transport, so a later live change compares against the mode the running
	// identities were actually built with (the v33 startup replay). The proxy
	// resolution is still Pending here; a settled direct-only node picks its
	// default up on the first settle (see h3_direct_default.go).
	replayH3ModeAtStartup(controlState)
	// The measured-cost preference is a live-applied key, so its persisted
	// value has to be replayed here or a restart loses it and the scheduler
	// (smart_dialer_scheduler.go) starts with no preference to feed.
	replaySmartDialerAtStartup(controlState)
	defer stopSmartDialerScheduler()
	unregSmartDialer := RegisterCoordinatorCloser(stopSmartDialerScheduler)
	defer unregSmartDialer()
	initControlAuditRing()
	// A HotSwap successor labels its start entry "hotswap". A Docker in-place
	// execve successor is not a spawned candidate (no IPC descriptor) but the
	// env marker survives the exec, so both kinds are labelled. The persist
	// gate is armed only for a spawned candidate: its ring was loaded before
	// the parent's final flush and must not clobber it until the takeover
	// merge.
	recordProcessStart(self.hotSwapCandidate || os.Getenv(EnvHotSwapExec) == "1", self.hotSwapCandidate)
	controlState.shutdownFn = cancel
	// The control socket is also the handoff's process-level switch: the
	// retiring parent quiesces (closes) it at the commit point and the
	// successor binds its own once the path is free. A spawned candidate does
	// not bind here — the parent still holds the socket — and binds from its
	// takeover continuation instead.
	controlSocket := &hotSwapControlSocketOwner{ctx: ctx, state: controlState}
	if !self.hotSwapCandidate {
		if err := controlSocket.start(); err != nil {
			output.observe(providerStatusFailed, 0, false, err, 0, nil)
			return err
		}
	}
	defer controlSocket.finish()
	// The handoff trigger: SIGUSR2 on Unix and the control socket's hotswap
	// command both run the parent-side handoff. A spawned candidate starts its
	// own listener and trigger in the takeover continuation, once the parent
	// has yielded.
	if !self.hotSwapCandidate {
		startHotSwapSignalListener(ctx, cancel, self.opts)
		setHotSwapTrigger(func() error {
			return hotSwapHandoff(ctx, cancel, self.opts)
		})
	}
	// /metrics: URNETWORK_METRICS binds a fixed address; otherwise the
	// persisted `metrics` setting rules and defaults to on. The listener
	// starts beside the control socket so a scrape during startup already
	// answers; the byte counters appear once the registry is published below.
	// A HotSwap candidate skips the bind: the parent still holds the address
	// until it yields, and the candidate's takeover continuation rebinds it
	// once the parent releases the listener at the handoff commit point.
	startMetricsAtStartup(controlState, self.hotSwapCandidate)
	defer func() { _ = stopMetrics() }()
	// pprof: URNETWORK_PPROF only (the provider's enable path), loopback only,
	// off by default. A HotSwap candidate's bind is retried in the background
	// because the parent still holds the port for the whole drain
	// (profile_listen.go).
	startProfilingAtStartup(ctx)
	defer func() { _ = stopProfiling() }()
	// One rate sample per second feeds the snapshot's throughput history.
	// Supervised: this same loop also drives the swap-thrash watchdog, so a
	// panic in a sample must restart it rather than end the process's only
	// always-on sampler (loop_supervisor.go). It is also joined at exit, like
	// every other supervised loop: the join cancels the run through the raw
	// cancel func first, so a shutdown never waits on a sampler whose context
	// nobody has ended yet, and the SIGNAL line still names the real first
	// canceller rather than this join.
	controlSamplerDone := make(chan struct{})
	go func() {
		defer close(controlSamplerDone)
		superviseLoop(ctx, newLoopRef("control_sampler"), func() { controlRuntime.runSampler(ctx) }, nil)
	}()
	defer func() {
		rawCancel()
		<-controlSamplerDone
	}()
	// The health heartbeat (v33 health_heartbeat.go): once immediately, then
	// every URNETWORK_HEALTH_INTERVAL (5m default, 1m floor), it publishes this
	// process's own health into the status file the proxy health cadence already
	// writes, so an external monitor can tell a wedged process (a heartbeat that
	// stopped advancing) from a quiet one. Supervised and joined like the
	// sampler: a panic restarts the pulse rather than ending it, and the join
	// cancels the run first so a shutdown never waits on a sleeping tick.
	//
	// The history-store prune is a second loop at the same cadence, not a step
	// in the pulse: it reads three files, and a slow read there must not hold
	// the stamp an external monitor judges this process by. It is joined with
	// the same bounded grace, so a prune stuck in a syscall cannot hang exit.
	heartbeatStart := controlRuntime.startedAtSnapshot()
	if heartbeatStart.IsZero() {
		heartbeatStart = time.Now()
	}
	healthHeartbeatDone := make(chan struct{})
	go func() {
		defer close(healthHeartbeatDone)
		superviseLoop(ctx, newLoopRef("health_heartbeat"), func() {
			runHealthHeartbeat(ctx, heartbeatStart, os.Getenv("URNETWORK_PROFILE"))
		}, nil)
	}()
	defer func() {
		rawCancel()
		joinHealthLoop(healthHeartbeatDone, "health heartbeat")
	}()
	proxyHistoryPruneDone := make(chan struct{})
	go func() {
		defer close(proxyHistoryPruneDone)
		superviseLoop(ctx, newLoopRef("proxy_history_prune"), func() {
			runProxyHistoryPruneLoop(ctx, healthHeartbeatConfiguredInterval())
		}, nil)
	}()
	defer func() {
		rawCancel()
		joinHealthLoop(proxyHistoryPruneDone, "proxy history prune")
	}()
	// Startup barrier. The control socket is bound and control state is
	// loaded, so the miner is self-managing; the proxy launch below is retry
	// loops that supervise themselves. READY must not wait for any proxy: see
	// announceStartupComplete. A HotSwap candidate does not own the unit yet
	// and signals after its takeover (hotSwapTakeoverCandidate).
	if !self.hotSwapCandidate {
		announceStartupComplete()
	}
	// systemd's watchdog runs from the start of the unit's life: feed it
	// through the whole of startup, not only once the proxy list is loaded.
	// Inert without WATCHDOG_USEC and without a self-exit supervisor
	// (liveness_watchdog.go).
	startLivenessWatchdog(ctx)
	// The streak's wait belongs to a fresh start whose node is actually
	// restarting. A HotSwap candidate must authenticate immediately so the
	// handoff can take over, and the parent still owns the persisted state.
	if !self.hotSwapCandidate {
		if err := runAuthBackoffWait(ctx); err != nil {
			return err
		}
	}
	keyPath, err := providerStatePath(".provider.key")
	if err != nil {
		output.observe(providerStartupRecoveryRequired, 0, false, err, 0, nil)
		return startupRefusal(ctx, err)
	}
	// Fork-only: automatically import 3.23-fix saved identities before the key
	// gate runs. Slots whose saved identity could not be proven are allowed to
	// register a new client below, exactly like 3.23-fix mints fresh when its
	// own reuse fails; the original 3.23-fix files are never written.
	stateDir := filepath.Dir(keyPath)
	legacyImport := runLegacyProviderImport(ctx, legacyImportOptions{
		stateDir:       stateDir,
		networkJwtPath: filepath.Join(stateDir, "jwt"),
		providers:      self.proxySettings,
		disabled:       self.noLegacyImport,
		explicitCreate: self.allowClientRegistration,
		explicitAdopt:  self.adoptLegacyProviderKey,
		recorder:       output,
		budget:         self.legacyImportBudget,
		newProver: func(wait legacyImportProofDialWait) (legacyImportProver, func(), error) {
			return newSdkLegacyImportProver(ctx, self.apiUrl, wait)
		},
	})
	keyOwner, err := clientauth.OpenProviderClientKey(ctx, keyPath, clientauth.ProviderClientKeyOptions{AllowCreate: self.allowClientRegistration, AdoptLegacyKey: self.adoptLegacyProviderKey})
	if err != nil {
		if ctx.Err() == nil {
			output.observe(providerStartupRecoveryRequired, 0, false, err, 0, nil)
		}
		return startupRefusal(ctx, err)
	}
	defer func() { returnErr = errors.Join(returnErr, keyOwner.Close()) }()
	if backup := keyOwner.RotationBackupName(); backup != "" {
		// The operator asked for a new durable identity; the replaced key file
		// must stay recoverable and the replacement must be visible.
		fmt.Fprintf(os.Stderr, "provider client key rotated: a fresh seed replaces the unusable retained key; the replaced file is preserved as %s\n", backup)
	}
	// v33 accepted the all-zero seed as a real key and the server can hold its
	// public key, so the retained identity is used. It is publicly known,
	// though, and the operator must rotate deliberately once the migration has
	// preserved the existing identities. stderr, like the startup refusal:
	// this warning must survive a diagnostic sink the exporter had to refuse.
	warnZeroProviderSeed(os.Stderr, keyOwner.Seed())
	networkJwtPath, err := providerStatePath("jwt")
	if err != nil {
		output.observe(providerStartupRecoveryRequired, 0, false, err, 0, nil)
		return startupRefusal(ctx, err)
	}
	var quarantined atomic.Bool
	stopRenewal := startNetworkTokenRenewal(ctx, self.apiUrl, providers[0], self.testEgressDialer, networkJwtPath, output, self.quarantineRejectedSignIn, func(clientauth.NetworkTokenQuarantine) {
		// the supervisor signs in again once this child has exited
		quarantined.Store(true)
		cancel()
	})
	// joinRenewal stops and joins the renewal watcher exactly once. The final
	// quarantine verdict below must be read after the watcher's goroutine has
	// finished: its rejection handling runs on that goroutine, so deciding the
	// exit code without joining first races the very callback that sets the
	// verdict (a rejected sign-in could look like an ordinary run failure).
	var stopRenewalOnce sync.Once
	var stopRenewalErr error
	joinRenewal := func() error {
		stopRenewalOnce.Do(func() { stopRenewalErr = stopRenewal() })
		return stopRenewalErr
	}
	defer func() { returnErr = errors.Join(returnErr, joinRenewal()) }()

	// Bandwidth registry tracks per-proxy byte counters for billing.
	// The wrapped dial counts TCP egress; UDP/QUIC egress in this lane is
	// not wrapped (see bandwidth.WrapDialContextSettings).
	bwRegistry := bandwidth.NewRegistry()
	// The control socket's snapshot and traffic replies read these counters.
	controlRuntime.setBandwidthRegistry(bwRegistry)

	// The IPv6 pin of the proxied slots (proxy_v6_pin_controller.go): one
	// controller for the run, its verdicts in their own file. A hot-swap
	// candidate reads the file but writes it only from its takeover on
	// (hotswap.go), so the parent's last verdicts are merged, not clobbered.
	v6LedgerPath := mustStatePathQuiet("v6-ledger.json")
	v6Verdicts := loadV6Ledger(v6LedgerPath, nil)
	v6Pins := newV6PinController(v6Verdicts, newV6LedgerPersister(v6LedgerPath, v6Verdicts, !self.hotSwapCandidate))
	setProcessV6Pins(v6Pins)
	defer setProcessV6Pins(nil)
	defer v6Pins.start(ctx)()

	// events.log records the start of this run - version, pid, host - once
	// its identity is staged and before any provider child starts, the same
	// line the v33 provider writes from provideSetupMemory.
	providerStartupCritLog()

	// READY=1 marks startup complete for a Type=notify unit, and this is the
	// same point the 3.23-fix provider sends it. It deliberately does NOT wait
	// for any proxy to authenticate: proxy auth can back off for hours or fail
	// during an API outage, and gating READY on it makes systemd time the start
	// job out (90s) and SIGTERM a healthy provider. Proxy health is reported
	// continuously through STATUS= instead. notifySystemdReady was ported from
	// the relay with no caller, which is why the unit never left "activating".
	if err := notifySystemdReady(); err != nil {
		tlog("[systemd] READY=1 notify failed: %s\n", err)
	}

	// events.log records the shutdown cause too: the first canceller's stack,
	// or v33's parent-cancellation text when the startup signal handler
	// cancelled the parent context before this run's own cancellers ran.
	go func() {
		<-ctx.Done()
		tlog("[provider] shutting down: main context cancelled\n")
		critLog("SIGNAL: context cancelled — draining goroutines\nsource:\n%s", cancelSource.text())
	}()

	// The bring-up narration: per-slot auth outcomes and, while slots are
	// pending, the running/pending/failed heartbeat. Declared before the
	// worker closure; started once the launch list is fixed below. A direct
	// slot the operator has turned off launches no worker at all (the handle
	// parks it, direct_slot.go), so it must not leave the tally pending.
	bringUpSlots := len(providers)
	if !isDirectEnabled() {
		if _, ok := directSlotIndex(providers); ok {
			bringUpSlots--
		}
	}
	bringUp := newProviderBringUpTracker(bringUpSlots)
	// Slots that completed authentication at least once during this run, kept
	// apart from the tracker because a slot that later fails leaves "running".
	var authenticatedSlots atomic.Int64
	provideWithProxy := func(slotCtx context.Context, index uint64, proxySettings *connect.ProxySettings) (returnErr error) {
		display := providerSlotDisplay(proxySettings)
		defer func() {
			// Registered first, this runs after every real cleanup below.
			// Keep cleanup causes even when an SDK/operation panics.
			if cause := recover(); cause != nil {
				// The recover is the only record of this panic: the process
				// keeps running, so the runtime prints nothing. Take the
				// bounded evidence bundle before the cause is folded away.
				captureIncidentBundle(incidentTriggerPanic,
					fmt.Sprintf("provider slot %d panicked: %v", index, cause), debug.Stack())
				if original, ok := cause.(error); ok {
					returnErr = errors.Join(returnErr, original)
				} else {
					returnErr = errors.Join(returnErr, errors.New("provider worker panicked"))
				}
			}
		}()
		proxyCtx, proxyCancel := context.WithCancel(slotCtx)
		defer proxyCancel()

		// A handoff's commit point (the parent's yieldCoordinatorSession) drops
		// this slot's platform session so the successor's connections under the
		// same client_id become the residents. Cancelling the slot's context
		// ends its serve loop; the deferred teardown then closes the device and
		// API session, which is the miner's equivalent of the v33 provider's
		// per-proxy platformTransport.Close() closer.
		unregSlotCloser := RegisterCoordinatorCloser(proxyCancel)
		defer unregSlotCloser()

		// One place assembles the slot's client strategy settings: its proxy,
		// the test-egress seam (direct slots only), the counters it is
		// charged to, and the network space's QUIC endpoint. See
		// slotClientStrategySettings. The closure keeps the run's side
		// effects on them — the leg observer's log sink and the earnings
		// publication below.
		clientStrategySettings, bw, proxyAddr := slotClientStrategySettings(
			proxySettings, bwRegistry, self.testEgressDialer,
		)
		// Observe before authentication starts: the platform dials that run
		// during bring-up go through this strategy, so a leg failure there is
		// the same evidence as one during steady state.
		proxyLegId := fmt.Sprintf("direct:%d", index)
		if proxySettings != nil {
			proxyLegId = proxySettings.Key()
		}
		clientStrategySettings.Log = withProxyLegObserver(clientStrategySettings.Log, proxyLegId)
		// Name the slot in its select lines, and report its first successful
		// dial: the evidence that this proxy is talking to the control plane.
		slotDisplay := providerSlotDisplay(proxySettings)
		clientStrategySettings.Log = withSlotSelectLabel(clientStrategySettings.Log, slotSelectLabel(index, proxyAddr), func(dur time.Duration, via string) {
			bringUp.controlReached(index, slotDisplay, dur, via)
		})
		// The one strategy the run itself drives for this identity. The slot's
		// API and its device keep the strategies the SDK builds from these
		// same settings (the space builds one, and a non-hosted device shares
		// it), which the run cannot name; this one carries the dials the run
		// makes itself — the wallet proof and the client-identity renewal — and
		// it is what the smart dialer probes (see smart_dialer_scheduler.go).
		// One strategy per slot, built where the slot's settings are assembled,
		// so the measured transport order and the dial actually taken are the
		// same dialer set.
		clientStrategy := connect.NewClientStrategy(proxyCtx, clientStrategySettings)
		defer clientStrategy.Close()
		if proxySettings != nil {
			// Publish the same counter for the earnings recording loop
			// (proxy_manager_earnings_loop.go), which folds it into the earned
			// history and the earn tracker once a minute. Withdrawn when the
			// slot exits: an offline identity leaves the live snapshot, while
			// its earnings record survives in the store.
			publishProxyEarnBandwidth(proxySettings.Key(), bw)
			defer unpublishProxyEarnBandwidth(proxySettings.Key())
		}
		networkSpace := sdk.NewNetworkSpaceWithUrls(proxyCtx, self.apiUrl, self.connectUrl, clientStrategySettings)
		defer networkSpace.Close()
		api := networkSpace.GetApi()
		// Authentication refusal also joins the API before the shared key owner
		// is released; later callback cleanup may join this same owner again.
		defer func() {
			returnErr = errors.Join(returnErr, api.CloseAndWait(context.Background()))
			if hook, ok := ctx.Value(providerRegistrationHooksKey{}).(providerRegistrationHooks); ok && hook.afterApiJoined != nil {
				hook.afterApiJoined()
			}
		}()

		networkJwtPath, err := providerStatePath("jwt")
		if err != nil {
			panic(err)
		}
		clientJwtPath, err := providerClientJwtPath(slotIdentities, proxySettings)
		if err != nil {
			panic(err)
		}

		seed := keyOwner.Seed()
		// Fork hook: the v33 auth admission gate (proxy_manager_admission_gate.go)
		// orders this identity's wait for a shared auth slot, weighted by its
		// LIFETIME failure count (v33 main.go's admitFailureCount), so a
		// chronically dead address cannot re-enter the lottery at full
		// "untried" priority every time the URL requeue brings it back. The
		// gate waits on the shared auth rate limiter itself, and its release
		// func holds the in-flight concurrency slot for exactly as long as
		// the attempt runs: in v33 the gate wraps provideAuth, so here it
		// wraps the whole client-identity acquisition below — a stored-JWT
		// reuse costs no API round trip, but a renewal or fresh mint does,
		// and those are what the shared limiter governs.
		admitFailureCount := 0
		if proxySettings != nil {
			admitFailureCount = globalProxyFailureHistory.FailureCount(proxySettings.Key())
		}
		release, admitErr := globalProxyAdmissionGate.Admit(proxyCtx, admitFailureCount)
		if admitErr != nil {
			// A cancelled wait never reached the API, so it is not evidence
			// about the API's rate and must not be reported to the limiter
			// (v33 returns waitErr down the same path).
			if proxyCtx.Err() == nil {
				output.observe(providerAuthenticationRecoveryEvent(admitErr), index, true, admitErr, 0, nil)
				bringUp.authFailed(index, display, admitErr, providerAuthRetryToken(providerAuthGiveUpInfo{}))
			}
			return admitErr
		}
		// The explicit flag always allocates; the automatic import enables a
		// new registration only for this slot's recorded fallback.
		allowCreate := self.allowClientRegistration || legacyImport.mintAllowed(legacyImportStoreKey(proxySettings))
		// Fork hook: the ported v33 hot-restart reuse (client_jwt_hotrestart.go)
		// sits in front of the durable-registration mint. With hot restart on
		// (the v33 default), a stored client JWT for this proxy identity is
		// reused — or renewed with the same client_id when expired — so a
		// restart keeps the identity's server-side reliability history. With
		// hot restart off, the fresh mint below registers a new client with a
		// new client_id and never reads the retained credential (v33 off).
		identityKey := jwtStoreKey(proxySettings)
		byClientJwt, clientId, reused, err := provideClientJWT(proxyCtx, clientJWTProvideConfig{
			Api:                    api,
			NetworkJwtPath:         networkJwtPath,
			ClientJwtPath:          clientJwtPath,
			KeyOwner:               keyOwner,
			ProxySettings:          proxySettings,
			IdentityKey:            identityKey,
			ApiUrl:                 self.apiUrl,
			ClientStrategySettings: clientStrategySettings,
			ClientStrategy:         clientStrategy,
			Mint: func(mintCtx context.Context) (string, connect.Id, error) {
				return authenticateProvider(mintCtx, api, networkJwtPath, clientJwtPath, keyOwner, providerRegistrationSlot(slotIdentities, proxySettings), allowCreate, output, index)
			},
			FreshMint: func(mintCtx context.Context) (string, connect.Id, error) {
				return authenticateProviderFresh(mintCtx, api, networkJwtPath, clientJwtPath, keyOwner, providerRegistrationSlot(slotIdentities, proxySettings), allowCreate, output, index)
			},
		})
		release()
		if err != nil {
			// Fork hook: the ported v33 give-up/backoff accounting
			// (proxy_manager_auth.go) records this identity's failed
			// authentication, and gives up on it once it reaches the ceiling.
			// Its result names the retry half of the per-slot auth line below.
			giveUp := noteProviderAuthFailure(proxySettings, err)
			// Fork hook: the shared limiter learns from the completed
			// attempt (v33 main.go reports after the failure accounting, so
			// a first-ever success is already "proven" when it lands).
			reportProviderAuthResult(proxySettings, err)
			if proxyCtx.Err() == nil {
				output.observe(providerAuthenticationRecoveryEvent(err), index, true, err, 0, nil)
				bringUp.authFailed(index, display, err, providerAuthRetryToken(giveUp))
			}
			return err
		}
		authenticatedSlots.Add(1)
		// A successful login ends the streak. This is the running-provider
		// path too: a reload that authenticates must clear it, never add to it.
		if previous := recordAuthSuccess(authBackoffNow()); previous.ConsecutiveFailures > 0 {
			logAuthRecoveredBanner(previous, authBackoffNow())
		}
		if reused {
			bringUp.authReused(index, clientId, display)
		} else {
			bringUp.authNew(index, clientId, display)
		}
		// Fork hook: a completed authentication proves the identity and clears
		// its failure/give-up history.
		noteProviderAuthSuccess(proxySettings)
		reportProviderAuthResult(proxySettings, nil)

		// The smart dialer ranks transports it has measured, and the
		// first-choice transport always wins where it works, so the others are
		// never dialed and never measured. This identity joins its EGRESS PATH
		// in the process's one probe scheduler (smart_dialer_scheduler.go): a
		// path is probed once however many identities share it, and the
		// scheduler owns the startup grace, the traffic-idle gate and the
		// process budget. Registered only now, after this identity
		// authenticated: an identity that never gets through auth, or sits in
		// slow retry, must not be a probe target.
		//
		// The path is the proxy's address, not the slot label: a reload hands
		// the same proxy a new slot index, and the address is what the dial
		// outcome belongs to.
		probeAddr, probeViaSocks := "", false
		if proxySettings != nil {
			probeAddr, probeViaSocks = proxySettings.Address, true
		}
		stopSmartDialerIdentity := startSmartDialerIdentity(clientStrategy, bw, self.apiUrl, int(index), probeAddr, probeViaSocks)
		// Registered after the strategy's own close, so it runs first: a round
		// dialing with this strategy is joined before the strategy goes away.
		defer stopSmartDialerIdentity()

		// Each authenticated slot owns its optional wallet set. A fresh or
		// proxy provider never borrows the direct provider's credential.
		if self.wallet != "" {
			proof := snWalletProof{}
			if self.walletProof != nil {
				proof = *self.walletProof
			}
			if proof.credentials.jwtFile == "" {
				proof.credentials.jwtFile = clientJwtPath
			}
			proof.credentials.original = byClientJwt
			// The slot's own strategy, not a throwaway: the same dialer set the
			// smart dialer probes, so the wallet's route is the measured one.
			walletErr := snSetWallet(proxyCtx, clientStrategy, self.apiUrl, self.wallet, &proof)
			if hooks, ok := proxyCtx.Value(providerRegistrationHooksKey{}).(providerRegistrationHooks); ok && hooks.afterWallet != nil {
				if err := hooks.afterWallet(walletErr); err != nil {
					return errors.Join(walletErr, err)
				}
			}
			if walletErr != nil {
				fmt.Fprintf(os.Stderr, "provider %d subnet wallet not set: %v; continuing to provide\n", index, walletErr)
			}
		}

		callbacks := &providerAuthenticationCallbacks{diagnostics: output, provider: index, clientJwtPath: clientJwtPath, networkJwtPath: networkJwtPath, identityKey: identityKey, cancel: cancel, custody: keyOwner}
		boundRefresh := &providerBoundRefresh{original: byClientJwt, callbacks: callbacks}
		refreshSub := api.AddJwtRefreshListener(boundRefresh)
		integritySub := api.AddClientRefreshIntegrityListener(boundRefresh)
		logoutSub := api.AddAuthLogoutListener(callbacks)
		defer func() {
			// Callbacks can cancel, but only this enclosing owner joins them.
			returnErr = errors.Join(returnErr, api.CloseAndWait(context.Background()))
			refreshSub.Close()
			integritySub.Close()
			logoutSub.Close()
			returnErr = errors.Join(returnErr, callbacks.failure())
		}()

		certPem, keyPem, _ := readProviderTlsCertAndKey()
		extenderKeySeed, _ := readProviderExtenderKeySeed(slotIdentities, proxySettings)
		settings := ProviderDeviceSettings(self.closeReportDomainHash)
		// Contract lifecycle lines name the path they ran on, so a contract
		// that never establishes traces to a proxy instead of the whole box.
		// The miner has no proxy index, so the slot index and the address
		// stand in for the relay's proxy[i] label.
		if settings.ClientSettings.ContractManagerSettings != nil {
			settings.ClientSettings.ContractManagerSettings.ProxyDescription = contractProxyDescription(index, proxyAddr)
		}
		// The transport's own logger observes control dials, so a dial that
		// fails inside this slot's proxy connect step lands on this slot's
		// leg evidence (proxy_leg.go, proxy_leg_observer.go).
		settings.Log = withProxyLegObserver(settings.Log, proxyLegId)
		// The extender role binds this host's carrier ports and publishes this
		// host's address, and it can do neither more than once per process: a
		// second space's bind fails and its record names an address the host
		// does not hold (an egress proxy's). Every slot of one run therefore
		// competes for one role, and only the direct slot can honestly hold it
		// -- the same rule the swarm members and hosted devices follow. The
		// remaining slots still provide; they just do not listen.
		settings.ProvideExtenderEnabled = proxySettings == nil
		// A proxied identity runs one family-agnostic transport, not the
		// v4-pinned/v6-pinned/standby group: a pinned transport dials from
		// this host, and behind a SOCKS proxy that dial cannot leave the
		// proxy, so the pins exist only to fail. 3.23 gates the group the same
		// way (provider/family_transport.go, useFamilyTransports returns false
		// for a proxied identity), which is why a production 3.23 provider
		// runs one transport per proxy. The direct slot keeps the group, since
		// it dials on this host's own routes and can honestly prove both
		// families.
		settings.DisablePlatformFamilyTransports = providerDisablesFamilyTransports(proxySettings)
		settings.KeyMaterial = providerDeviceKeyMaterial(seed, certPem, keyPem, extenderKeySeed)
		applyProviderMemoryTarget(settings, self.memoryPlan.DeviceMemoryTargetByteCount)
		// Wrap ProviderDialContextSettings with bandwidth tracking too.
		// The SDK copies this into its buffer dial paths; only the TCP
		// DialContext is wrapped, so this counts TCP egress.
		//
		// The same settings carry the slot's H3 endpoint: the SDK hands their
		// PacketConnFactory to the provider carrier as
		// H3PacketConnFactory, which is what keeps a proxied identity's QUIC
		// on its proxy's UDP relay instead of a socket on this host. A direct
		// slot gets none, so its H3 keeps the engine's own
		// wildcard-plus-physical-egress socket. See h3_packet_conn.go.
		settings.ProviderDialContextSettings = providerSlotDialSettings(
			self.testEgressDialer, proxySettings, bw, proxyAddr,
		)
		// The h3 mode reaches this slot's provider carrier as one read: the
		// policy is built (and the launch row recorded) before
		// newProviderDeviceLocal, which sets it on the device right after the
		// sdk built the carrier on its own default. The row therefore never
		// claims a launch that has not happened. See h3_provider_policy.go for
		// the make-before-break window that setter opens.
		device, err := newProviderDeviceLocal(
			proxyCtx,
			networkSpace,
			clientStrategySettings,
			byClientJwt,
			fmt.Sprintf("provider %s %s", runtime.GOOS, RequireVersion()),
			settings,
			workProfile,
			providerRegistrationSlot(slotIdentities, proxySettings),
			clientId,
			h3SlotTransportSettings(proxySettings),
			contractProfile,
		)
		if err != nil {
			if workProfile != nil || contractProfile != nil {
				output.observe(captureEvent, index, true, err, 0, nil)
			}
			return err
		}
		defer func() {
			returnErr = errors.Join(returnErr, device.CloseAndWait(context.Background()))
		}()

		// Fork hook: drive this identity's live health from its platform
		// transport's real connection state (proxy_transport_health.go). The
		// provider marks a proxy up only once its transport has registered
		// routes and down when that connection drops; without the same watch
		// here, a proxy that authenticated and then lost its relay stayed "up"
		// and kept its slot. Registered after the close defer above, so the
		// stop runs first and the close itself is never read as a drop.
		if proxySettings != nil {
			defer watchProxyTransportHealth(proxyCtx, proxySettings.Key(), device, proxyTransportWatchInterval)()
		}
		// Step 2: a proxied slot whose proxy has proven IPv6 egress also runs
		// an IPv6-pinned transport, so the platform records it dual-stack;
		// every other slot keeps its one transport and pays only a periodic
		// probe. The slot never waits on it. A nil proxySettings (the direct
		// slot, which keeps the family group) registers nothing. Deferred
		// after the device close, so it stops first.
		defer v6Pins.register(proxyCtx, proxySettings, display, device, networkSpace.GetPlatformUrlV6(), v6ControlPlatformUrl(networkSpace))()
		// Fork hook: charge this slot's relayed bytes to its counters, and its
		// open provider contracts to the same counter's live client count, from
		// the accounting the SDK keeps inside the device (provider_billing.go).
		// Without the bytes, BillableRx/Tx and ContractUsed* never move and
		// every billable and settled series this node publishes is a structural
		// zero. Without the contracts, `clients` has no source at all: the
		// network space's own dials are the node's control plane and are counted
		// total-only (h3_packet_conn.go), not as clients.
		// Registered after the close defer above, so the watch stops before the
		// device it reads is closed. Every slot gets it, the direct identity
		// included: it relays traffic and its bytes are part of the node's
		// aggregate in the same scrape.
		defer watchProxyBilling(proxyCtx, bw, device, providerBillingSampleInterval)()

		// Fork hook: count this slot's acquired contracts for
		// urnet_contracts_total. The provider reads its own per-index registry,
		// fed by a connect contract status callback
		// (provider/contract_metrics.go); the miner owns no connect client, so
		// it counts the provider contract details the SDK publishes for this
		// slot instead (contract_metrics.go). Registered for every slot,
		// including the direct identity, because every slot is a full provider
		// device with its own contract feed. Deferred after the device close, so
		// the listeners stop before the device does.
		defer registerContractOutcomes(device)()

		// Always-on public mode includes network and friends/family service,
		// matching the SDK's hierarchical provide contract.
		device.SetProvideControlMode(sdk.ProvideControlModeAlways)
		progressGeneration, err := progress.attach(proxyCtx, providerRegistrationSlot(slotIdentities, proxySettings), device)
		if err != nil {
			return err
		}
		defer progress.retire(providerRegistrationSlot(slotIdentities, proxySettings), progressGeneration)

		// HotSwap candidate: this process exists to take the parent's place.
		// This slot is live, so the takeover is real and must be confirmed —
		// the ACK is what lets the parent yield its coordinator session and
		// drain. The continuation (bind this process's own control socket,
		// merge the parent's final audit entries, take over the handoff
		// triggers) follows on the goroutine that sent the ACK and runs once
		// for the process.
		if self.hotSwapCandidate {
			candidateAckOnce.Do(func() { self.hotSwapTakeoverCandidate(ctx, cancel, controlSocket) })
		}

		keyMaterial := device.GetKeyMaterial()
		if !bytes.Equal(keyMaterial.GetClientKeySeed(), seed) {
			return errors.New("provider device changed its retained registration key")
		}
		persistProviderAuxiliaryKeyMaterial(output, index, slotIdentities, proxySettings, keyMaterial)
		observeProviderExtenderIdentity(output, index, keyMaterial.GetExtenderKeySeed())
		// one line per change, so the activation prints once it settles and
		// nothing repeats while it holds (connect/EXTENDER.md F3, G3)
		extenderStatusSub := device.AddExtenderProvideStatusChangeListener(
			newProviderExtenderStatusListener(output, index))
		defer extenderStatusSub.Close()

		select {
		case <-proxyCtx.Done():
		}
		return nil
	}

	// The native direct slot's handle. Its launch stays exactly the startup
	// launch: provideWithProxy with nil settings. The handle is what the
	// control path reaches so `provider direct off` stops a slot that is
	// already serving and `provider direct on` starts it again without a
	// restart (direct_slot.go). It is not a proxy identity, so it is never
	// handed to the reloader's registry below.
	var direct *directSlot
	if directIndex, ok := directSlotIndex(providers); ok {
		direct = newDirectSlot(
			ctx,
			func(slotCtx context.Context, index uint64) error {
				return provideWithProxy(slotCtx, index, nil)
			},
			directIndex,
			isDirectEnabled(),
			func(index uint64) {
				output.observe(providerStarted, index, true, nil, 0, nil)
			},
		)
	}

	// The live reloader: `proxy add`/`remove`/`trim` writes the reload trigger
	// (proxy_manager_reload.go) and this applies the new desired set to the
	// running provide without a restart. It is built before the launch loop
	// because it must adopt the slots the loop starts (seed), and only for an
	// unprofiled run: a profile run validates and registers a fixed slot set at
	// start, and a slot appearing at runtime would bypass that gate.
	var reloader *minerProxyReloader
	if workProfile == nil && contractProfile == nil {
		// Reloaded slots continue past the configured index range, so a slot
		// added at runtime never collides with a configured slot's label.
		var reloadSlotIndex atomic.Uint64
		reloadSlotIndex.Store(uint64(len(providers)))
		reloader = newMinerProxyReloader(
			func() ([]*connect.ProxySettings, error) {
				// The manager-backed desired set, read fresh (proxy.state +
				// URL cache + operator trim cap). Its error is the reload's
				// abort signal: a transient read failure must not be read as
				// "no proxies" and cancel the fleet.
				//
				// desiredForReload, not desired: this read's set IS applied
				// below, so a binding trim cap here is a live capacity
				// decision and belongs in the action ledger. The command
				// readers keep the silent read.
				launch, _, _, err := newProxyManager(defaultProxyManagerConfig()).desiredForReload()
				if err != nil {
					return nil, err
				}
				desired := make([]*connect.ProxySettings, 0, len(launch))
				for _, settings := range launch {
					if settings == nil {
						continue
					}
					desired = append(desired, settings)
				}
				// Identities the quality reaper cancelled are held out until
				// their window elapses (proxy_manager_degraded_reaper.go).
				// Without this the very next reload would re-add the proxy the
				// reaper just judged: the desired set still contains it,
				// because the reaper does not rewrite proxy.state or the URL
				// cache.
				return filterReapHeldProxySettings(desired, time.Now()), nil
			},
			func(settings *connect.ProxySettings) (context.CancelFunc, error) {
				// One identity per live address. provider_slot_identity.go
				// classifies an address as shared once, at start, and the slot
				// hash follows from it: a second identity at a live host:port
				// would silently re-identify the running slot, so refuse it.
				if err := reloader.reserveAddress(settings); err != nil {
					return nil, err
				}
				index := reloadSlotIndex.Add(1) - 1
				// The census was fixed at start (its ConfigHash is published to
				// monitors), so admit the slot outside it: it serves, its
				// progress just is not part of that census.
				progress.admitLateSlot(providerRegistrationSlot(slotIdentities, settings))
				slotCtx, slotCancel := context.WithCancel(ctx)
				go func() {
					if err := provideWithProxy(slotCtx, index, settings); err != nil {
						tlog("❌ [proxy] reloaded slot %s (provider %d) stopped: %v\n", settings.Key(), index, err)
					}
				}()
				return slotCancel, nil
			},
		)
		if direct != nil {
			// The control path for the native direct slot: every reload
			// re-reads the persisted operator toggle and starts or stops the
			// slot to match. The reloader only calls it - the slot stays
			// outside the reloader's registry and its failure history
			// (direct_slot.go).
			reloader.onReload = func() {
				applyDirectToggle(direct)
			}
		}
		reloader.onChange = func(keys []string) {
			// Both describe what this run is really serving: the OOM cap counts
			// running proxies, and the control surface is what
			// `proxy trim --preview` reads.
			setOOMCapRunningProxies(len(keys))
			controlRuntime.setRunningProxies(keys)
		}
		reloader.onApplied = func(applied []*connect.ProxySettings) {
			// The apply half of the h3 candidate set: desired() itself is a
			// pure read, so the ranking over the set this reload just applied
			// is noted here, and a ranking that moved relaunches the
			// identities that joined or left it.
			applyH3CandidateSet(applied, resolveH3Mode(globalControlState, currentProxyResolution()))
		}
		// Publish the slot owner for proxy audit's park (proxy_manager_audit_park.go):
		// the audit loop was started by the manager before this reloader
		// existed, so it resolves the pointer per tick and parks nothing until
		// it finds one. Clearing it is stop()'s job.
		currentMinerProxyReloader.Store(reloader)
	}

	// Launch ordering (W-02): v33's warmth/earnings prioritization
	// (proxy_manager_warmth.go) decides the order the provider goroutines
	// start in and the per-tier stagger between them, so the scarce auth-mint
	// slots go to the identities that can dial with a stored client JWT or
	// renew one in a single step, and within a tier the proven earners go
	// first. The direct slot keeps its relative place at the end of the list
	// (it is not a proxy and carries no warmth). Each slot keeps its original
	// index, so the diagnostics label a provider by the slot it was
	// configured as, exactly as before.
	// Startup grading gate readiness hook (proxy_manager_gate.go): the
	// manager's active probe loops are held until the workers launched below
	// are up, and this is the moment their clocks (plateau, deadline) start
	// from. The hook never blocks and changes nothing about the launches.
	noteProviderWorkerLaunches()
	launchSlots := scheduleProxyLaunch(providers, self.proxySourceOf, "")
	// Arm the warmup signal (proxy_manager_warmup.go): the hold releases
	// once every curated worker below has passed its stagger, so the URL
	// pool and the URL fetcher wait for an uncontested curated ramp, bounded
	// by urlWarmupLaunchGrace either way.
	curatedWarmup := 0
	for _, slot := range launchSlots {
		if slot.Settings == nil {
			// The direct slot carries no proxy settings and no ramp.
			continue
		}
		if warmupIsCurated(self.proxySourceOf[slot.Settings.Key()]) {
			curatedWarmup++
		}
	}
	noteCuratedWarmupTotal(curatedWarmup)
	// Start the bring-up heartbeat once the launch list is fixed; a run with
	// no slots closes it immediately.
	bringUp.start()
	defer bringUp.shutdown()
	var wg sync.WaitGroup
	results := make(chan error, len(providers))
	for _, slot := range launchSlots {
		index := slot.Index
		proxySettings := slot.Settings
		baseDelay := slot.Delay
		staggerDuration := slot.Stagger
		// A proxy slot gets its own context, handed to the reloader: the
		// reloader now owns that identity, so a later reload that drops or
		// rotates it cancels this same context instead of leaving a second
		// provider for the identity running. The direct slot carries no
		// identity and keeps the run context.
		slotCtx := ctx
		if proxySettings != nil && reloader != nil {
			var slotCancel context.CancelFunc
			slotCtx, slotCancel = context.WithCancel(ctx)
			reloader.seed(proxySettings, slotCancel)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var result error
			defer func() {
				if cause := recover(); cause != nil {
					// Same recover, second registration: a panic in the slot
					// goroutine before it reaches provideWithProxy.
					captureIncidentBundle(incidentTriggerPanic,
						fmt.Sprintf("provider slot %d goroutine panicked: %v", index, cause), debug.Stack())
					if original, ok := cause.(error); ok {
						result = original
					} else {
						result = errors.New("provider worker panicked")
					}
				}
				if result != nil {
					if workProfile != nil || contractProfile != nil {
						// A complete launch cannot leave its other declared members
						// running after one actual owner failed admission.
						cancel()
					}
					output.observe(providerWorkerFailed, uint64(index), true, result, 0, nil)
					if ctx.Err() == nil && !errors.Is(result, context.Canceled) && !errors.Is(result, context.DeadlineExceeded) {
						bringUp.workerFailed(uint64(index))
					}
				}
				results <- result
			}()
			// The warmth-derived base delay plus a jittered
			// per-slot stagger; a zero delay starts immediately.
			if !backoffPacerWithDelay(baseDelay, staggerDuration, ctx) {
				return
			}
			if ctx.Err() != nil {
				return
			}
			var source string
			if proxySettings != nil {
				source = self.proxySourceOf[proxySettings.Key()]
			}
			if warmupIsCurated(source) {
				noteCuratedWarmupLaunch()
			}
			// An unproven URL-sourced identity holds until the curated
			// ramp completes or the grace elapses (proxy_manager_warmup.go,
			// the relay's warmup launch gate); its curated peers are
			// unaffected because each slot waits in its own goroutine.
			if source == "url" && shouldDeferURLWarmupLaunch(source, proxySettings.Key(), time.Now()) {
				if !waitURLWarmupHold(ctx) {
					return
				}
			}
			if proxySettings != nil {
				output.observe(providerStarted, uint64(index), true, nil, 0, nil)
				result = provideWithProxy(slotCtx, uint64(index), proxySettings)
				return
			}
			// The direct slot keeps one handle for the whole run (direct_slot.go).
			// serve() runs provideWithProxy while the operator toggle is on,
			// stops that launch when it goes off, and parks without returning so
			// the run stays up for a later `direct on`. It narrates its own
			// provider_started on every launch.
			if direct == nil {
				return
			}
			result = direct.serve()
		}()
	}
	// Apply reload triggers for the rest of the run, and stop whatever it
	// started on the way out: the reloader's slots are outside the launch
	// WaitGroup, so nothing else would end them. This starts after the launch
	// loop and before the join - the slots below run until the run ends.
	//
	// The watcher itself is supervised and joined: a panic in a reload must
	// restart the watcher (a bare goroutine would take the process down), and
	// the run must not return while it is still polling a trigger for a
	// process that no longer serves. The join is registered last, so it runs
	// first (LIFO) and the watcher is stopped before its slots are cancelled;
	// it cancels through the raw cancel func, so it cannot wait on a context
	// nobody has ended.
	if reloader != nil {
		reloadWatchDone := make(chan struct{})
		// The pool controller's pressure cut cancels through this same
		// reloader (proxy_manager_pressure.go): the controller is started by
		// the proxy manager, which runs before this reloader exists, so it
		// reads the installed one when a cut fires. Clearing it on the way out
		// keeps a late tick from cancelling into a pool this run no longer
		// serves.
		setPressureShedReloader(reloader)
		defer setPressureShedReloader(nil)
		// Publish the run's per-identity relaunch facility: an h3 mode change
		// reconnects the identities whose eligibility moved through the
		// reloader (see minerH3Relauncher), without a restart. Cleared on the
		// way out so a later offline read (CLI, tests) sees no run.
		clearH3Relauncher := installProvideH3Relauncher(ctx, reloader)
		defer clearH3Relauncher()
		if reloadPath, err := proxyReloadPath(); err == nil {
			go func() {
				defer close(reloadWatchDone)
				superviseLoop(ctx, newLoopRef("reloader_watch"), func() { reloader.watch(ctx, reloadPath, 0) }, nil)
			}()
		} else {
			close(reloadWatchDone)
		}
		// The quality reaper can only cancel what the reloader owns, so it is
		// installed only when the reloader is (same condition: an unprofiled
		// run). It shares the reloader's slots and stops with the run's ctx.
		// It is a long-running ticker loop, so it runs under the supervisor: a
		// panic in one pass restarts it with backoff instead of leaving the run
		// without a reaper. Built once, so a restart resumes the same reaper
		// rather than a fresh one.
		reaper := newMinerDegradedReaper(reloader)
		reaperRef := newLoopRef("degraded_reaper")
		go func() {
			superviseLoop(ctx, reaperRef, func() { reaper.run(ctx) }, nil)
		}()
		defer reloader.stop()
		defer func() {
			rawCancel()
			<-reloadWatchDone
		}()
	}
	wg.Wait()
	cancel()
	close(results)
	var slotResults []error
	for result := range results {
		slotResults = append(slotResults, result)
		returnErr = errors.Join(returnErr, result)
	}
	if parent.Err() == nil {
		if noSlot := noSlotAuthenticatedError(int(authenticatedSlots.Load()), len(launchSlots), slotResults); noSlot != nil {
			tlog("[provider] %v\n", noSlot)
			returnErr = errors.Join(returnErr, noSlot)
			// A HotSwap candidate is a fresh process but not a fresh node:
			// the parent is still serving, so its failure must not touch the
			// persisted streak or ask for the terminal stop.
			var noSlotErr *providerNoSlotAuthenticatedError
			if !self.hotSwapCandidate && errors.As(noSlot, &noSlotErr) && noSlotErr.credentialsRejected {
				if stopErr := authBackoffTerminal(authBackoffNow(), noSlot.Error()); stopErr != nil {
					returnErr = errors.Join(returnErr, stopErr)
				}
			}
		}
	}
	// A handed-off parent must outlive the slots the yield cancelled: the drain
	// window keeps in-flight streams alive until the successor owns the
	// traffic, and the drain owns the exit. Without this the run would return
	// the moment the last yielded slot unwinds.
	waitForHotSwapDrain()
	// Join the sign-in renewal watcher before reading its verdict, so a
	// rejection it is still processing has set `quarantined` first (see
	// joinRenewal above).
	if stopErr := joinRenewal(); stopErr != nil {
		returnErr = errors.Join(returnErr, stopErr)
	}
	if quarantined.Load() {
		returnErr = errors.Join(returnErr, errProviderNetworkSignInQuarantined)
	}
	return returnErr
}

// slotClientStrategySettings is the one place a slot's client strategy
// settings are assembled: the proxy it dials through, whether the run's
// test-egress seam applies, the bandwidth counters its traffic is charged to,
// and the network space's QUIC endpoint (networkSpaceConnectSettings).
//
// Named rather than inlined so a test can read what the run asks of the sdk
// without standing up a slot (h3_packet_conn_test.go), the same reason
// swarmMemberDeviceSettings exists. The run keeps its own side effects on the
// result: the earnings publication and the leg observer's log sink.
//
// Two rules live here, both of them the reason a proxied slot cannot be built
// by hand at a call site:
//
//   - The test-egress seam is installed only while no proxy is configured. A
//     non-nil DialContextSettings bypasses ProxySettings in connect's dial path
//     (net.go), so installing it next to a proxy would leak that slot's dials
//     around the proxy. A proxied slot therefore starts with no
//     DialContextSettings of its own.
//   - The counters are keyed by proxy IDENTITY (address, or address+user for a
//     credentialed proxy), not by the bare address: two accounts behind one
//     gateway are different paid proxies, and a counter shared between them
//     would credit each identity with the other's bytes — the earnings store
//     and the per-proxy earn tracker both key by identity, and the control
//     report's totals are per proxy. The direct slot keeps index 0.
//
// The returned bw is the counter the caller must publish for the earnings
// loop, and proxyAddr the slot's proxy address ("" for the direct slot).
func slotClientStrategySettings(
	proxySettings *connect.ProxySettings,
	bwRegistry *bandwidth.ProxyRegistry,
	testEgressDialer *connect.DialContextSettings,
) (*connect.ClientStrategySettings, *bandwidth.ProxyBandwidth, string) {
	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategySettings.ProxySettings = proxySettings
	if proxySettings == nil {
		clientStrategySettings.DialContextSettings = testEgressDialer
	} else {
		// Only the direct slot keeps the extender network client and the gossip
		// node. Every slot builds its own network space and the SDK starts
		// both for each one, so with hundreds of proxy identities that is
		// hundreds of directories, clients and gossip nodes on one machine.
		clientStrategySettings.DisableExtenderNetwork = true
	}
	// Count through the proxy, not around it, and give the space's own QUIC
	// dialers (the api's alt-h3/alt-whodis carriers) the slot's proxy
	// endpoint: for a proxied slot the wrap builds the H3 packet conn factory
	// from that proxy, so no QUIC dial of this network space can open a host
	// socket under the proxied identity. A direct slot gets no factory and
	// keeps the engine's own socket. See networkSpaceConnectSettings.
	bw := bwRegistry.Register(0) // direct connection by default
	proxyAddr := ""
	if proxySettings != nil {
		proxyAddr = proxySettings.Address
		bw = bwRegistry.Register(proxyIdentityBandwidthIndex(proxySettings))
	}
	clientStrategySettings.ConnectSettings = networkSpaceConnectSettings(
		clientStrategySettings.ConnectSettings, proxySettings, bw, proxyAddr,
	)
	return clientStrategySettings, bw, proxyAddr
}

// errProviderNetworkSignInQuarantined ends a run whose rejected network
// sign-in was set aside (--quarantine-rejected-sign-in).
var errProviderNetworkSignInQuarantined = errors.New("the rejected network sign-in was set aside")

// providerQuarantinedExitCode tells the supervisor that this child set its
// rejected network sign-in aside (EX_TEMPFAIL): it signs in again and starts
// the child, without the crash backoff.
const providerQuarantinedExitCode = 75

// providerStartupRefusedExitCode ends a run that stopped at startup admission
// before any provider served. A supervisor (systemd Restart=on-failure, the
// miner image's own supervisor, provide --all-operators) must see the refusal
// as a failure and retry, never as a clean completion.
const providerStartupRefusedExitCode = 1

// errProviderStartupRefused marks a run whose retained custody was refused
// before any provider served (a 3.23-fix state the automatic import could not
// prove, a missing or mismatched key identity marker, an unusable key). The
// run's own error stays unwrap-able; this sentinel only selects the exit code.
var errProviderStartupRefused = errors.New("provider startup refused")

// registrationRefusalHint says how to proceed after a refusal that needs an
// explicit decision about this device's identity. The refusal itself is a safety
// rule and unchanged; the line it prints just did not say what to do next.
func registrationRefusalHint(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if !strings.Contains(text, "requires_explicit_recovery") {
		return ""
	}
	return `If this is a new device, register it on purpose: run "provider start" in a terminal, run "provider setup", or add --allow-client-registration to "provider provide". If it already had an identity, restore its state folder from backup instead.`
}

// providerStartupRefusalError keeps the refusal cause in the error chain while
// marking the run for the nonzero startup exit.
type providerStartupRefusalError struct{ cause error }

func (self *providerStartupRefusalError) Error() string { return self.cause.Error() }
func (self *providerStartupRefusalError) Unwrap() error { return self.cause }
func (self *providerStartupRefusalError) Is(target error) bool {
	return target == errProviderStartupRefused
}

// startupRefusal marks a pre-provider startup error as a refusal. A stop
// signal already cancelling the run is a clean stop, not a refusal: it keeps
// the bare error and the ordinary exit policy.
func startupRefusal(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil {
		return err
	}
	return &providerStartupRefusalError{cause: err}
}

// errProviderNoSlotAuthenticated ends a run in which every launched slot
// failed and none authenticated. Without it the provide command returns
// normally and the process exits 0, which Restart=on-failure never restarts.
var errProviderNoSlotAuthenticated = errors.New("no provider slot authenticated")

// providerNoSlotAuthenticatedExitCode is a retryable failure: the slots could
// not reach or complete auth, and a restart may succeed.
const providerNoSlotAuthenticatedExitCode = 1

// errProviderAuthStopped is the terminal stop after repeated authentication
// refusals. The persisted streak, not a single run, owns the decision.
var errProviderAuthStopped = errors.New("authentication stopped after repeated refusals")

type providerNoSlotAuthenticatedError struct {
	tried  int
	top    string
	topNum int
	// credentialsRejected is true only when every slot failure was a
	// credential rejection or a rate-limit refusal, never a transient network
	// or protocol failure.
	credentialsRejected bool
}

func (self *providerNoSlotAuthenticatedError) Error() string {
	return fmt.Sprintf("%v: tried %d slots, top failure (%d): %s", errProviderNoSlotAuthenticated, self.tried, self.topNum, self.top)
}
func (self *providerNoSlotAuthenticatedError) Is(target error) bool {
	return target == errProviderNoSlotAuthenticated
}

// noSlotAuthenticatedError is non-nil only when slots were launched, none ever
// authenticated, and every slot ended with an error. A run with at least one
// authenticated slot (partial failure) keeps its existing exit behaviour, as
// does a run whose slots ended cleanly.
func noSlotAuthenticatedError(authenticated, tried int, results []error) error {
	if authenticated > 0 || tried == 0 || len(results) != tried {
		return nil
	}
	counts := map[string]int{}
	credentialsRejected := true
	var order []string
	for _, result := range results {
		if result == nil {
			return nil
		}
		if !authFailureCounts(result) {
			credentialsRejected = false
		}
		reason := result.Error()
		if len(reason) > 200 {
			reason = reason[:200]
		}
		if counts[reason] == 0 {
			order = append(order, reason)
		}
		counts[reason]++
	}
	top := order[0]
	for _, reason := range order {
		if counts[reason] > counts[top] {
			top = reason
		}
	}
	return &providerNoSlotAuthenticatedError{
		tried:               tried,
		top:                 top,
		topNum:              counts[top],
		credentialsRejected: credentialsRejected,
	}
}

// providerRunExitCode is the process exit code a run's error asks for, if any.
func providerRunExitCode(err error) (int, bool) {
	if errors.Is(err, errProviderNetworkSignInQuarantined) {
		return providerQuarantinedExitCode, true
	}
	if errors.Is(err, errProviderStartupRefused) {
		return providerStartupRefusedExitCode, true
	}
	if errors.Is(err, errProviderAuthStopped) {
		return minerExitAuthStopped, true
	}
	if errors.Is(err, errProviderNoSlotAuthenticated) {
		return providerNoSlotAuthenticatedExitCode, true
	}
	return 0, false
}

// exitProviderRun terminates the process with code after settings.run reported
// a failure that must end it.
//
// os.Exit skips every deferred call, including the one that runs
// finishControlRun's shutdown flush, so the failure categories counted while the
// run went down -- a startup refusal, a worker panic, a refused capture profile,
// a quarantined sign-in -- would never reach disk. Those are precisely the
// events a crash-loop investigation needs, so the pending counts are written
// here, bounded and panic-proof: a state disk that is itself the reason the node
// is dying must not keep the process alive.
//
// exitFunc is the package's exit seam (hotswap.go's test hooks), so a test can
// drive this path and observe the code without taking the test binary down.
func exitProviderRun(code int) {
	flushPersistentErrorsAtExit()
	exitFunc(code)
}

// Resolves the shared auth, network, proxy and provider state directory.
// URNETWORK_STATE_DIR overrides ~/.urnetwork; resolution creates no files.
func providerStateDir() (string, error) {
	if override := strings.TrimSpace(os.Getenv("URNETWORK_STATE_DIR")); override != "" {
		absolute, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		return filepath.Clean(absolute), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork"), nil
}

// Resolves one file in the selected state directory without creating it.
// applyPersistedBaselineFlag puts the resolved `set baseline` value in force
// before the proxy manager can write its start mark. The full control-state load
// runs later and is untouched; this only reads the same file plus the queue
// urnet-tools leaves while the provider is stopped, for one key.
func applyPersistedBaselineFlag() {
	early, err := loadControlState()
	if err != nil {
		return
	}
	// A queued `set baseline ...` lands on this start, so the value that takes
	// effect has to be persisted-plus-queued: a queued off must stop the
	// recorder before the start mark exists, and a queued on or clear must beat
	// a persisted off. Only baseline ops are replayed here; the queue itself is
	// consumed later by mergePendingControlOverrides, so nothing is persisted or
	// removed at this point.
	if ops, err := loadPendingControlOps(); err == nil {
		for _, op := range ops {
			if op.Key != "baseline" {
				continue
			}
			switch op.Op {
			case "set":
				_ = early.setWithValue(op.Key, op.Value, SourcePending)
			case "clear":
				_ = early.clear(op.Key)
			}
		}
	}
	v, ok := early.get("baseline")
	if !ok {
		// Cleared, or never set: recording is on by default.
		baselineEnabled.Store(true)
		return
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "0", "false", "no":
		baselineEnabled.Store(false)
	default:
		baselineEnabled.Store(true)
	}
}

func providerStatePath(name string) (string, error) {
	dir, err := providerStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// providerClientJwtPath gives each independently connected proxy provider a
// stable renewable client identity without placing proxy credentials in a
// filename. The direct (non-proxy) provider keeps the simple legacy-adjacent
// name. An address configured more than once hashes the full 3.23-fix
// identity (address + user), so two usernames at one host:port get distinct
// files, never one credential holding either identity; an address configured
// once keeps its historical name byte-identical.
func providerClientJwtPath(identities providerSlotIdentities, proxySettings *connect.ProxySettings) (string, error) {
	if proxySettings == nil {
		return providerStatePath(".provider.jwt")
	}
	sum := identities.hash(proxySettings)
	return providerStatePath(fmt.Sprintf(".provider-%x.jwt", sum[:8]))
}

// providerExtenderKeyPath gives each provider slot its own extender identity.
// The identity also names the space's gossip peer, and one process runs one
// gossip node per slot. A single file shared by every slot made those nodes
// present one peer id to the mesh: they ignore each other's publications, and
// the ones that reach the operator under that id supersede each other's
// address set as fast as they publish. A slot keeps one identity across
// launches so an activated extender keeps the id its record names. The name
// follows the same slot identity as the client jwt path, so two usernames at
// one host:port never share one extender identity either.
func providerExtenderKeyPath(identities providerSlotIdentities, proxySettings *connect.ProxySettings) (string, error) {
	if proxySettings == nil {
		return providerStatePath(".provider.extender.key")
	}
	sum := identities.hash(proxySettings)
	return providerStatePath(fmt.Sprintf(".provider.extender-%x.key", sum[:8]))
}

// readProviderClientKeySeed loads the Ed25519 seed for the provider
// client's long-lived identity key from `~/.urnetwork/.provider.key`.
// Returns (nil, nil) when the file does not exist — a fresh install.
// The file is the raw 32-byte seed; no encoding.
func readProviderClientKeySeed() ([]byte, error) {
	p, err := providerStatePath(".provider.key")
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// writeProviderClientKeySeed persists the Ed25519 seed to
// `~/.urnetwork/.provider.key` with 0600 permissions (sensitive
// material — anyone with this file can impersonate the provider
// against the platform identity layer).
func writeProviderClientKeySeed(seed []byte) error {
	p, err := providerStatePath(".provider.key")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	return os.WriteFile(p, seed, 0600)
}

// providerDeviceKeyMaterial is the identity material the provider device is
// built with. The space keeps no local state, so the extender's keys live in
// the provider state directory and ride the key material:
//   - the extender identity seed (connect/EXTENDER.md B1, G2): without it the
//     role would activate under a new key every launch and the operator would
//     revoke the old one as fast as it publishes it.
//   - the file the lag.dev measurement key is kept in (lag.dev/PLAN.md C4),
//     which the role creates at its first start and asks the operator to
//     certify. Without it the role serves no measurement name, which left
//     lag.dev with no extender to offer.
func providerDeviceKeyMaterial(clientKeySeed []byte, certPem []byte, keyPem []byte, extenderKeySeed []byte) *sdk.DeviceLocalKeyMaterial {
	keyMaterial := sdk.NewDeviceLocalKeyMaterial(clientKeySeed, certPem, keyPem)
	keyMaterial.SetExtenderKeySeed(extenderKeySeed)
	if measurementKeyFile, err := providerStatePath(providerExtenderMeasurementKeyFileName); err == nil {
		keyMaterial.SetExtenderMeasurementKeyFile(measurementKeyFile)
	}
	return keyMaterial
}

// The provider state file of the extender's lag.dev measurement key: a PEM
// private key the sdk role writes 0600 when it is absent.
const providerExtenderMeasurementKeyFileName = ".provider.extender.measurement.key"

// readProviderExtenderKeySeed loads the ed25519 seed of this provider slot's
// extender identity from its state file -- `.provider.extender.key` for the
// direct provider, one per proxy slot otherwise (connect/EXTENDER.md B1).
// Returns (nil, nil) when the file does not exist -- a slot that has not been
// an extender yet. The file is the raw 32-byte seed; no encoding, exactly as
// the client key seed beside it.
func readProviderExtenderKeySeed(identities providerSlotIdentities, proxySettings *connect.ProxySettings) ([]byte, error) {
	p, err := providerExtenderKeyPath(identities, proxySettings)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// writeProviderExtenderKeySeed persists the extender identity seed of one
// provider slot to its state file with 0600 permissions (sensitive
// material -- anyone with this file can present this host's extender identity
// to the mesh and to the operator).
func writeProviderExtenderKeySeed(identities providerSlotIdentities, proxySettings *connect.ProxySettings, extenderKeySeed []byte) error {
	p, err := providerExtenderKeyPath(identities, proxySettings)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	return os.WriteFile(p, extenderKeySeed, 0600)
}

// readProviderTlsCertAndKey loads the sequence-level TLS server cert
// chain and matching private key from `~/.urnetwork/.provider.cert`
// (PEM, leaf first, possibly chained) and the private key from the
// same file (the PEM blocks are concatenated: cert blocks first,
// then a single `PRIVATE KEY` block). Returns (nil, nil, nil) when
// the file does not exist.
func readProviderTlsCertAndKey() (certPem []byte, keyPem []byte, returnErr error) {
	p, err := providerStatePath(".provider.cert")
	if err != nil {
		return nil, nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	// Split into cert blocks and the private key block.
	rest := b
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		// re-encode the block so the output is canonical PEM (one
		// trailing newline per block).
		blockPem := pem.EncodeToMemory(block)
		if block.Type == "CERTIFICATE" {
			certPem = append(certPem, blockPem...)
		} else {
			// First non-cert block (typically `PRIVATE KEY` or
			// `EC PRIVATE KEY`) is treated as the key. Stop after
			// the first key block.
			keyPem = blockPem
			break
		}
		rest = next
	}
	return certPem, keyPem, nil
}

// writeProviderTlsCertAndKey persists the sequence-level TLS server
// cert and private key to `~/.urnetwork/.provider.cert` with 0600
// permissions. The cert blocks are written first, then the private
// key block, so the on-disk file is a self-contained PEM bundle.
func writeProviderTlsCertAndKey(certPem, keyPem []byte) error {
	p, err := providerStatePath(".provider.cert")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	out := make([]byte, 0, len(certPem)+len(keyPem))
	out = append(out, certPem...)
	out = append(out, keyPem...)
	return os.WriteFile(p, out, 0600)
}

type Status struct {
	diagnostics *providerDiagnostics
	progress    *providerProgressOwner
}

func (self *Status) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/provider-progress" {
		serveProviderProgress(w, r, self.progress)
		return
	}
	type WarpStatusResult struct {
		Version       string                    `json:"version,omitempty"`
		ConfigVersion string                    `json:"config_version,omitempty"`
		Status        string                    `json:"status"`
		ClientAddress string                    `json:"client_address,omitempty"`
		Host          string                    `json:"host"`
		Diagnostics   *providerDiagnosticStatus `json:"diagnostics,omitempty"`
	}

	result := &WarpStatusResult{
		Version: RequireVersion(),
		// ConfigVersion: RequireConfigVersion(),
		Status:      "ok",
		Host:        RequireHost(),
		Diagnostics: self.diagnostics.snapshot(),
	}

	responseJson, err := json.Marshal(result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(responseJson)
}

func Host() (string, error) {
	host := os.Getenv("WARP_HOST")
	if host != "" {
		return host, nil
	}
	host, err := os.Hostname()
	if err == nil {
		return host, nil
	}
	return "", errors.New("WARP_HOST not set")
}

func RequireHost() string {
	host, err := Host()
	if err != nil {
		panic(err)
	}
	return host
}

func RequireVersion() string {
	if version := os.Getenv("WARP_VERSION"); version != "" {
		return version
	}
	return Version
}

func proxyAuthAdd(opts docopt.Opts) {
	proxyConfig := readProxyConfig()

	key, _ := opts.String("<key>")
	if key == "" {
		key, _ = opts.String("key")
	}
	user, _ := opts.String("<proxy_user>")
	if user == "" {
		user, _ = opts.String("proxy_user")
	}
	password, _ := opts.String("<proxy_password>")
	if password == "" {
		password, _ = opts.String("proxy_password")
	}

	if proxyConfig.Auths == nil {
		proxyConfig.Auths = map[string]*ProxyAuth{}
	}

	if _, ok := proxyConfig.Auths[key]; ok {
		if force, _ := opts.Bool("-f"); !force {
			fmt.Printf("auth key \"%s\" exists. Overwrite? [yN]\n", key)

			reader := bufio.NewReader(os.Stdin)
			confirm, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
				return
			}
		}
	}

	proxyConfig.Auths[key] = &ProxyAuth{
		User:     user,
		Password: password,
	}

	writeProxyConfig(proxyConfig)
	// Tell a running provider: its reloader applies the stored list on the
	// trigger, and without it the change waits for the next start.
	triggerProxyReload()
}

func proxyAuthRemove(opts docopt.Opts) {
	proxyConfig := readProxyConfig()

	if all, _ := opts.Bool("--all"); all {
		clear(proxyConfig.Auths)
	} else {

		key, _ := opts.String("<key>")
		if key == "" {
			key, _ = opts.String("key")
		}

		if proxyConfig.Auths == nil {
			proxyConfig.Auths = map[string]*ProxyAuth{}
		}

		delete(proxyConfig.Auths, key)
	}

	writeProxyConfig(proxyConfig)
	// Tell a running provider: its reloader applies the stored list on the
	// trigger, and without it the change waits for the next start.
	triggerProxyReload()
}

func proxyAdd(opts docopt.Opts) {
	// File-backed providers (Workflow A: proxy.state.Source set by
	// --proxy_file, C-10) re-read that file on every reload, so additions must
	// land in THAT file; a write to the internal config is discarded by the
	// next reload. The paste hand-off (`proxy add --proxy_file=<tmp> -f`)
	// relies on this routing.
	if state, err := readProxyState(); err == nil && state.Source != "" {
		proxyAddFileBacked(state.Source, opts)
		return
	}

	proxyConfig := readProxyConfig()

	allKeyAddress := proxyAddCollectAddresses(opts)

	if proxyConfig.Servers == nil {
		proxyConfig.Servers = map[string]string{}
	}

	for _, keyAddress := range allKeyAddress {
		entry := parseProxyEntry(keyAddress, proxyConfig.Auths)
		address, user, password := entry.address, entry.user, entry.password
		if proxyConfig.Auths != nil {
			if proxyAuth, ok := proxyConfig.Auths[entry.key]; ok {
				user = proxyAuth.User
				password = proxyAuth.Password
			}
		}

		// Registered auth keys stay keyed by address alone, preventing credential
		// duplication in dictionary keys and preserving auth rotation. Inline
		// credentials store the canonical host:port:user:pass.
		proxyKey := entry.address
		if entry.key == "" && (entry.user != "" || entry.password != "") {
			proxyKey = canonicalProxyKey(entry.address, entry.user, entry.password)
		}

		if currentKey, ok := proxyConfig.Servers[proxyKey]; ok && currentKey != entry.key {
			if force, _ := opts.Bool("-f"); !force {
				fmt.Printf(
					"server %s (%s/%s) exists with different key. Change key? [yN]\n",
					address,
					obfuscateUser(user),
					obfuscatePassword(password),
				)

				reader := bufio.NewReader(os.Stdin)
				confirm, _ := reader.ReadString('\n')
				if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
					return
				}
			}
		}

		fmt.Printf(
			"added server %s (%s/%s)\n",
			address,
			obfuscateUser(user),
			obfuscatePassword(password),
		)

		proxyConfig.Servers[proxyKey] = entry.key
	}

	writeProxyConfig(proxyConfig)
	// Tell a running provider: its reloader applies the stored list on the
	// trigger, and without it the change waits for the next start.
	triggerProxyReload()
}

// proxyEntry is one proxy list entry after parsing.
type proxyEntry struct {
	// key is a registered auth key name, empty for inline credentials.
	key      string
	address  string
	user     string
	password string
}

// parseProxyEntry reads one proxy list entry. Two inline credential forms are
// accepted, and both keep working when a credential contains `:` or `@`:
//
//	user:pass@host:port
//	host:port:user:pass
//
// In the first form the split is the last `@` that leaves a `host:port`, so an
// `@` inside the password is not a separator; the userinfo splits at its first
// `:`, so the password may contain colons. In the second form the address is
// the first two fields, the user is the third, and everything after it is the
// password.
//
// A prefix that names a registered auth key is a key and takes precedence,
// which keeps historical `key@host:port` files working. A prefix with no colon
// that names no auth stays a key, as it always has.
func parseProxyEntry(keyAddress string, auths map[string]*ProxyAuth) proxyEntry {
	keyAddress = strings.TrimSpace(keyAddress)
	if i := strings.Index(keyAddress, "@"); 0 <= i {
		prefix := keyAddress[:i]
		if _, ok := auths[prefix]; ok {
			return proxyEntry{key: prefix, address: keyAddress[i+1:]}
		}
	}
	for i := strings.LastIndex(keyAddress, "@"); 0 <= i; i = strings.LastIndex(keyAddress[:i], "@") {
		if validProxyAddress(keyAddress[i+1:]) {
			userinfo := keyAddress[:i]
			if j := strings.Index(userinfo, ":"); 0 <= j {
				return proxyEntry{
					address:  keyAddress[i+1:],
					user:     userinfo[:j],
					password: userinfo[j+1:],
				}
			}
			return proxyEntry{key: userinfo, address: keyAddress[i+1:]}
		}
	}
	if n := proxyAddressLen(keyAddress); 0 < n && n < len(keyAddress) && keyAddress[n] == ":"[0] {
		rest := keyAddress[n+1:]
		if i := strings.Index(rest, ":"); 0 <= i {
			return proxyEntry{
				address:  keyAddress[:n],
				user:     rest[:i],
				password: rest[i+1:],
			}
		}
	}
	return proxyEntry{address: keyAddress}
}

// canonicalProxyKey is the one shape stored in the proxy config for inline
// credentials. Registered keys remain stored by address alone, preventing
// credential leakage into dictionary keys and preserving auth rotation.
func canonicalProxyKey(address string, user string, password string) string {
	if user == "" && password == "" {
		return address
	}
	return address + ":" + user + ":" + password
}

// proxyAddressLen returns the end of a leading host:port, or 0 when the text
// does not start with one. A bracketed IPv6 host is one address, not fields.
func proxyAddressLen(value string) int {
	hostEnd := 0
	if strings.HasPrefix(value, "[") {
		i := strings.Index(value, "]")
		if i < 0 {
			return 0
		}
		hostEnd = i + 1
	} else {
		i := strings.Index(value, ":")
		if i <= 0 {
			return 0
		}
		hostEnd = i
	}
	if len(value) <= hostEnd || value[hostEnd] != ":"[0] {
		return 0
	}
	portEnd := hostEnd + 1
	for portEnd < len(value) && "0"[0] <= value[portEnd] && value[portEnd] <= "9"[0] {
		portEnd += 1
	}
	if portEnd == hostEnd+1 {
		return 0
	}
	return portEnd
}

func validProxyAddress(value string) bool {
	return proxyAddressLen(value) == len(value)
}

func proxyRemove(opts docopt.Opts) {
	// --match routes to the pattern collection/removal (v33 proxyRemove):
	// the pattern is matched against hosts in the internal config, the proxy
	// state and the URL cache, and persisted as a fetch exclusion.
	if pattern, _ := opts.String("--match"); pattern != "" {
		proxyRemoveMatch(pattern, opts)
		return
	}

	if all, _ := opts.Bool("--all"); all {
		// v33's remove --all is the full reset (clearProxyStores): the internal
		// config, proxy.state - Proxies and the ID counter - and the URL cache
		// and source list. Failures are warnings, matching v33: a partial reset
		// beats refusing the operator.
		if err := clearProxyStores(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
		// a partial reset still changed the list: tell a running provider
		triggerProxyReload()
		return
	}

	proxyConfig := readProxyConfig()

	allKeyAddress := []string{}
	if allKeyAddressAny, ok := opts["<key_address>"]; ok {
		allKeyAddress = append(allKeyAddress, allKeyAddressAny.([]string)...)
	}

	if proxyConfig.Servers == nil {
		proxyConfig.Servers = map[string]string{}
	}

	for _, keyAddress := range allKeyAddress {
		entry := parseProxyEntry(keyAddress, proxyConfig.Auths)
		user, password := entry.user, entry.password
		if proxyConfig.Auths != nil {
			if auth, ok := proxyConfig.Auths[entry.key]; ok {
				user = auth.User
				password = auth.Password
			}
		}

		// Delete by exact address or canonical address:user:password
		delete(proxyConfig.Servers, entry.address)
		if user != "" || password != "" {
			delete(proxyConfig.Servers, canonicalProxyKey(entry.address, user, password))
		}

		// If removing by key@address, delete any server pointing to this key
		if entry.key != "" {
			for k, v := range proxyConfig.Servers {
				if v == entry.key && (k == entry.address || strings.HasPrefix(k, entry.address+":")) {
					delete(proxyConfig.Servers, k)
				}
			}
		}

		// If removing by address alone, remove all entries matching that address
		if entry.key == "" && entry.user == "" && entry.password == "" {
			for k := range proxyConfig.Servers {
				if k == entry.address || strings.HasPrefix(k, entry.address+":") {
					delete(proxyConfig.Servers, k)
				}
			}
		}
	}

	writeProxyConfig(proxyConfig)
	// Tell a running provider: its reloader applies the stored list on the
	// trigger, and without it the change waits for the next start.
	triggerProxyReload()
}

type ProxyConfig struct {
	Auths map[string]*ProxyAuth `json:"auths"`
	// TODO is there a use case for multiple keys to the same address?
	// address -> key
	Servers map[string]string `json:"servers"`
}

type ProxyAuth struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// readProxySettings returns the desired provider slots. It is manager-backed:
// when proxy.state exists the desired set is the manager's (operator config
// plus URL cache, capped by the operator trim target); a state directory the
// port has never touched falls back to the legacy <state>/proxy file.
func readProxySettings() []*connect.ProxySettings {
	return proxyManagerReadSettings()
}

// readProxySettingsLegacy is the pre-port reader: the internal `<state>/proxy`
// config only. It stays the fallback for a fresh state directory and the
// internal-config reader used by the dead-proxy removal and desired-set
// queries.
func readProxySettingsLegacy() []*connect.ProxySettings {
	proxyConfig := readProxyConfig()

	if proxyConfig.Servers == nil {
		return nil
	}

	var allProxySettings []*connect.ProxySettings
	for proxyAddress, key := range proxyConfig.Servers {
		address, user, password := parseProxyAddress(proxyAddress)
		proxySettings := &connect.ProxySettings{
			Network: "tcp",
			Address: address,
		}
		if user != "" || password != "" {
			proxySettings.Auth = &proxy.Auth{
				User:     user,
				Password: password,
			}
		}
		if proxyConfig.Auths != nil {
			proxyAuth, ok := proxyConfig.Auths[key]
			if ok {
				proxySettings.Auth = &proxy.Auth{
					User:     proxyAuth.User,
					Password: proxyAuth.Password,
				}
			}
		}
		allProxySettings = append(allProxySettings, proxySettings)
	}

	return allProxySettings
}

// parseProxyAddress splits one stored proxy-config key into its parts. It
// accepts "host:port", "host:port:user:pass", and "user:pass@host:port";
// the shapes are decided by parseProxyEntry.
func parseProxyAddress(proxyAddress string) (address string, user string, password string) {
	entry := parseProxyEntry(proxyAddress, nil)
	return entry.address, entry.user, entry.password
}

func obfuscateUser(user string) string {
	if user == "" {
		return "<no user>"
	} else if len(user) < 6 {
		return "***"
	} else {
		return fmt.Sprintf("%s***%s", user[:2], user[len(user)-2:])
	}
}

func obfuscatePassword(password string) string {
	if password == "" {
		return "<no password>"
	} else if len(password) < 6 {
		return "***"
	} else {
		return fmt.Sprintf("%s***%s", password[:2], password[len(password)-2:])
	}
}

// Proxy selection follows the same state directory as its provider credentials.
func readProxyConfig() *ProxyConfig {
	proxyPath, err := providerStatePath("proxy")
	if err != nil {
		panic(err)
	}

	if _, err := os.Stat(proxyPath); errors.Is(err, os.ErrNotExist) {
		return &ProxyConfig{}
	}

	b, err := os.ReadFile(proxyPath)
	if err != nil {
		panic(err)
	}

	var proxyConfig ProxyConfig
	err = json.Unmarshal(b, &proxyConfig)
	if err != nil {
		panic(err)
	}
	return &proxyConfig
}

// Updates only the selected directory's proxy configuration.
func writeProxyConfig(proxyConfig *ProxyConfig) {
	proxyPath, err := providerStatePath("proxy")
	if err != nil {
		panic(err)
	}

	b, err := json.Marshal(proxyConfig)
	if err != nil {
		panic(err)
	}

	err = os.WriteFile(proxyPath, b, 0700)
	if err != nil {
		panic(err)
	}
}

// addressHash returns a stable uint32 hash of a string,
// used as a proxy index for bandwidth tracking in v2026 where
// ProxySettings has no Index field.
//
// DESIGN ADAPTATION: The fork's connect.ProxySettings carried an int Index
// field assigned at proxy registration time. v2026 removed this —
// ProxySettings only has Network, Address, Auth. We hash the proxy identity
// to produce a stable integer key for the bandwidth registry. Collisions are
// acceptable (two proxies sharing a hash just share a counter) and rare
// for realistic proxy counts.
func addressHash(addr string) uint32 {
	var h uint32
	for i := 0; i < len(addr); i++ {
		h = h*31 + uint32(addr[i])
	}
	return h
}

// proxyIdentityBandwidthIndex is the bandwidth registry index for one proxy
// slot: the hash of the proxy IDENTITY key (address, or address+user for a
// credentialed proxy). Hashing the identity instead of the bare address keeps
// two accounts behind one gateway address on separate counters, so the
// earnings store and the earn tracker — which both key by identity — never
// credit one account with the other's billable bytes. The direct slot is not
// a proxy and keeps index 0.
func proxyIdentityBandwidthIndex(settings *connect.ProxySettings) int {
	if settings == nil {
		return 0
	}
	return int(addressHash(settings.Key()) & 0x7fffffff)
}
