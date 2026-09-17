package provider

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

// main() is the CLI entry point. It dispatches to subcommands (auth,
// provide, proxy, wallet, sn-status, claim, etc.) via docopt.
func Main() {
	// G-M2: sanitize PATH when running as root to prevent hijacking
	// of exec.Command bare names (systemctl, docker, etc.) via attacker-writable
	// directories earlier in root's PATH. Filter rather than replace so
	// reachable entries (e.g. NixOS /run/current-system/sw/bin) survive.
	if os.Getuid() == 0 {
		sanitizeRootPath()
	}

	profile := os.Getenv("URNETWORK_PROFILE")
	ramlogs := os.Getenv("URNETWORK_RAMLOGS")

	// If in auto mode and RAM logs aren't already explicitly on, we audit the disk speed
	// BEFORE initializing the logger. This allows us to auto-enable it.
	autoRamLogTriggered := false
	finishAudit := bannerPhase("System audit")
	if profile == "auto" && isLongRunningSubcommand() {
		manualRamLogs := (ramlogs == "1")
		slowDisk, _ := RunStartupAudit()
		if slowDisk && !manualRamLogs {
			finishAudit("slow disk → auto RAM logs", "⚠")
			autoRamLogTriggered = true
		} else {
			detail := "disk OK"
			if slowDisk {
				detail = "slow disk (RAM logs manually set)"
			}
			finishAudit(detail)
		}
	} else if isLongRunningSubcommand() {
		RunStartupAudit()
		finishAudit("manual profile")
	} else {
		finishAudit("not long-running")
	}

	// Seed URNETWORK_PROFILE / URNETWORK_RAMLOGS from persisted control
	// state BEFORE initGlog(): initGlog reads those env vars for its
	// one-shot ramlog redirect decision, and provide()'s later
	// loadControlState() runs far too late to affect them.
	seedEnvFromControlState()

	finishInit := bannerPhase("Logger init")
	initGlog()
	finishInit("glog + stderr")

	// If auto-tuner enabled RAM logs, perform the countdown handover now.
	// Only for the long-running provide process — see isLongRunningSubcommand.
	if autoRamLogTriggered && isLongRunningSubcommand() {
		initSHMLoggerWithHandover()
	}

	usage := fmt.Sprintf(
		`Connect provider.

The default URLs are:
    api_url: %s
    connect_url: %s

Usage:
    provider auth ([<auth_code>] | --user_auth=<user_auth> [--password=<password>]) [-f]
    	[--api_url=<api_url>]
    	[--max-memory=<mem>]
    	[-v...]
    provider provide [--port=<port>]
        [--api_url=<api_url>]
        [--connect_url=<connect_url>]
        [--wallet=<coldkey_ss58>]
        [--max-memory=<mem>]
        [--proxy_file=<proxy_file>]
        [--file=<file>]
        [--proxy_url=<proxy_url>...]
        [--url=<url>...]
        [--URL=<URL>...]
        [--proxy_url_refresh=<proxy_url_refresh>]
        [--proxy_url_max=<proxy_url_max>]
        [--proxy_dead_cleanup_scope=<proxy_dead_cleanup_scope>]
        [--proxy_dead_cleanup_interval=<proxy_dead_cleanup_interval>]
        [-v...]
    provider auth-provide ([<auth_code>] | --user_auth=<user_auth> [--password=<password>]) [-f]
    	[--port=<port>]
        [--api_url=<api_url>]
        [--connect_url=<connect_url>]
        [--wallet=<coldkey_ss58>]
        [--max-memory=<mem>]
        [--proxy_file=<proxy_file>]
        [--file=<file>]
        [--proxy_url=<proxy_url>...]
        [--url=<url>...]
        [--URL=<URL>...]
        [-v...]
    provider wallet set <coldkey_ss58>
        [--api_url=<api_url>]
        [-v...]
    provider sn-status [--json]
        [--api_url=<api_url>]
        [-v...]
    provider claim [--epoch=<epoch>] [--rpc=<rpc_url>]... [--key_file=<key_file>] [--dry-run]
        [--api_url=<api_url>]
        [-v...]
    provider bind-head --hotkey=<hex> --registrant=<registrant> --contract=<contract> [--rpc=<rpc_url>]... [--key_file=<key_file>] [--dry-run]
        [-v...]
    provider unbind-head --hotkey=<hex> [--contract=<contract>] [--rpc=<rpc_url>]... [--key_file=<key_file>] [--dry-run]
        [-v...]
    provider proxy auth add [<key>] <proxy_user> <proxy_password> [-f]
    provider proxy auth remove [<key>] [--all]
    provider proxy add [<key_address>...] [--proxy_file=<proxy_file>] [--file=<file>] [--url=<url>] [--URL=<URL>] [-f]
    provider proxy remove [<key_address>...] [--all]
    provider proxy remove --match=<pattern> [--yes] [--preview]
    provider proxy remove-dead [--degraded[=<duration>]] [--auth-failures=<N>] [--source=<source>] [--yes] [--preview]
    provider proxy activity
    provider proxy refresh [--force]
    provider proxy add-source <url>
    provider proxy remove-source <url>
    provider proxy exclude [<pattern>] [--remove]
    provider proxy summary
    provider proxy trim <count> [--preview]
    provider direct [<state>]
    provider proxy paste [--file=<file>]
    provider logs [-n <lines>]
    provider print-network-id <file>
    provider choose_network <api_url> [<connect_url>]
    provider choose_network --reset

Options:
    -h --help                        Show this help and exit.
    --version                        Show version.
    -v...                            Enable verbose mode. -v implies verbose level 1,
    					                 -vv implies level 2... etc.
    --json                           Output in JSON format for automated tooling.
    -f                               Force overwrite the JWT token store file or proxy value, if exists.
                                     By default, existing values will not be overwritten.
    --api_url=<api_url>              Specify a custom API URL to use.
    --connect_url=<connect_url>      Specify a custom connect URL to use.
    <api_url>                        API URL to save as the chosen network (http:// or https://),
                                     or a preset: main | beta.
    <connect_url>                    Connect URL to save as the chosen network (ws:// or wss://).
    --reset                          With choose_network, clear the saved network and revert to the main network.
    --user_auth=<user_auth>	         Login with a username.
    --password=<password>            Login with a password. If --user_auth is used, you will be prompted for your
    					                 password anyways, if you don't specify it using this option.
    -p --port=<port>                 Status server port [default: 0].
    --max-memory=<mem>               Set the maximum amount of memory in bytes, or the suffixes b, kib, mib, gib may be used [This is a soft limit].
    --wallet=<coldkey_ss58>          Also set the subnet claim wallet at startup, same as provider wallet set.
                                     A failure is logged and does not block providing.
    <coldkey_ss58>                   Subnet claim wallet: an ss58 coldkey address (prefix 42).
    --epoch=<epoch>                  Epoch to fetch the subnet pool claim for. Defaults to the last
                                     finalized epoch, which is the epoch before the current one.
    --rpc=<rpc_url>                  EVM json-rpc endpoint used to check the payout root on-chain.
                                     May be repeated; endpoints are tried in order until one answers.
    --key_file=<key_file>            EVM private key file. When given, claim/bind-head/unbind-head sign
                                     and submit the transaction (via --rpc); without it, the ready-to-submit
                                     calldata is printed for the offline/air-gapped snclaim path.
    --dry-run                        Build and sign the extrinsic but do not submit.
    --hotkey=<hex>                   Head-tier miner hotkey as a 0x-optional 32-byte hex account id.
    --registrant=<registrant>        The EVM address that will submit bindHead via snclaim (0x, 20 bytes).
                                     The head-bind digest is bound to this address, so it MUST equal the
                                     snclaim sender, whose mirror must be the hotkey's on-chain coldkey.
    --contract=<contract>            STSubnet proxy contract address (0x, 20 bytes).
    <key>                            Authentication key
    <proxy_user>                     SOCKS5 user
    <proxy_password>                 SOCKS5 password
    <key_address>                    SOCKS5 server as host:port, host:port:user:pass, host:port::, or key@host:port
    --proxy_file=<proxy_file>        A path to a file where each line contains on entry as host:port, host:port:user:pass, host:port::, or key@host:port
    --proxy_url=<proxy_url>          A live proxy list URL. Repeatable. Additive with --proxy_file / internal config. Also settable via PROXY_URL (comma-separated for multiple).
    --url=<url>                      Alias for --proxy_url.
    --URL=<URL>                      Alias for --proxy_url.
    --proxy_url_refresh=<dur>        How often to re-fetch --proxy_url sources and add new entries. Also settable via PROXY_URL_REFRESH.
    --proxy_url_max=<n>              Cap on total proxies sourced from --proxy_url. 0 = unlimited, defaults to 500. Also settable via PROXY_URL_MAX.
    --proxy_dead_cleanup_scope=<s>   Automatic dead-proxy cleanup scope: none, url, or all. Defaults to url (URL-sourced only). Also settable via PROXY_DEAD_CLEANUP_SCOPE.
    --proxy_dead_cleanup_interval=<dur>  How often automatic cleanup runs, when scope isn't none. Also settable via PROXY_DEAD_CLEANUP_INTERVAL.
    <url>                            A proxy list URL.
    --match=<pattern>                Case-insensitive substring matched against proxy hosts (never port or
                                     credentials). Removes matches from the proxy list, proxy file, and URL
                                     cache, and excludes the pattern from future URL fetches. See 'proxy exclude'.
    <pattern>                        Host substring for 'proxy exclude' (add). With --remove, deletes the pattern.
                                     With no pattern, 'proxy exclude' lists active patterns.
    --file=<file>                    A path to a proxy file (alias for --proxy_file, or input for 'proxy paste').
    <state>                          Direct IP providing state: on | off | status. If omitted, reports current state.
    <count>                          Max number of running proxies to keep. The A-F worst-graded above it are shed. 0/off clears the cap.
    --force                          Bypass the 8-hour warmup protection gate.
    -n <lines>                       Number of lines to show from the end of the log [default: 0].`,
		DefaultApiUrl,
		DefaultConnectUrl,
	)

	// Allow `provider help` as a friendlier alias for --help
	if len(os.Args) == 2 && os.Args[1] == "help" {
		os.Args[1] = "--help"
	}

	opts, err := docopt.ParseArgs(usage, os.Args[1:], RequireVersion())

	if err != nil {
		panic(err)
	}

	// Support auth code via environment variable for Docker/dash-prefixed tokens.
	// An explicit CLI positional argument takes precedence over the env var.
	if cur, _ := opts.String("<auth_code>"); cur == "" {
		if envAuthCode := os.Getenv("URNETWORK_AUTH_CODE"); envAuthCode != "" {
			opts["<auth_code>"] = envAuthCode
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
		if set, _ := opts.Bool("set"); set {
			walletSet(opts)
		}
	} else if snStatus, _ := opts.Bool("sn-status"); snStatus {
		snStatusCmd(opts)
	} else if claim_, _ := opts.Bool("claim"); claim_ {
		claim(opts)
	} else if bindHead_, _ := opts.Bool("bind-head"); bindHead_ {
		bindHead(opts)
	} else if unbindHead_, _ := opts.Bool("unbind-head"); unbindHead_ {
		unbindHead(opts)
	} else if auth_, _ := opts.Bool("auth"); auth_ {
		auth(opts)
	} else if provide_, _ := opts.Bool("provide"); provide_ {
		provide(opts)
	} else if authProvide, _ := opts.Bool("auth-provide"); authProvide {
		if os.Getenv(EnvHotSwap) != "1" {
			auth(opts)
		}
		provide(opts)
	} else if logs, _ := opts.Bool("logs"); logs {
		providerLogs(opts)
	} else if printNetworkId, _ := opts.Bool("print-network-id"); printNetworkId {
		printNetworkIdCmd(opts)
	} else if chooseNetwork, _ := opts.Bool("choose_network"); chooseNetwork {
		chooseNetworkCmd(opts)
	} else if direct_, _ := opts.Bool("direct"); direct_ {
		cmdDirect(opts)
	}
}

// isLongRunningSubcommand returns true when the invoked subcommand is
// provide or auth-provide — the two long-lived daemons that benefit from
// startup audits, RAM logging, and the hot-swap path.
func isLongRunningSubcommand() bool {
	if len(os.Args) < 2 {
		return false
	}
	if os.Args[1] != "provide" && os.Args[1] != "auth-provide" {
		return false
	}
	for _, arg := range os.Args[2:] {
		switch arg {
		case "-h", "--help", "--version":
			return false
		}
	}
	return true
}

// initGlog configures the glog-based logging subsystem for the provider.
func initGlog() {
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")
	flag.Set("v", "0")
	// unlike unix, the android/ios standard is for diagnostics to go to stdout
	os.Stderr = os.Stdout

	profile := os.Getenv("URNETWORK_PROFILE")
	ramlogs := os.Getenv("URNETWORK_RAMLOGS") == "1"
	if (profile == "lowmem" || profile == "eco" || ramlogs) && isLongRunningSubcommand() {
		// If explicitly requested via profile or env, just start it.
		// Auto-detection handover is handled in main() with a countdown.
		// One-shot CLI subcommands print directly to the terminal instead.
		fmt.Fprintf(os.Stderr, "[ramlogs] Logs redirected to RAM — view with: urnet-tools logs\n")
		initSHMLogger()
	}
}

// initSHMLoggerWithHandover performs the auto-detected RAM-log handover
// with a visible countdown so the operator sees the transition.
func initSHMLoggerWithHandover() {
	fmt.Printf("\n[audit] Slow disk detected. Moving all subsequent logs to RAM (/dev/shm) for performance.\n")
	tlog("[audit] >>> Live logs: urnet-tools logs  |  tail -f /dev/shm/urnetwork.log")
	tlog("[audit] Redirecting in 3...")
	time.Sleep(1 * time.Second)
	fmt.Printf(" 2...")
	time.Sleep(1 * time.Second)
	fmt.Printf(" 1...\n")
	time.Sleep(1 * time.Second)
	os.Setenv("URNETWORK_RAMLOGS", "1")
	initSHMLogger()
}

// RunStartupAudit performs a quick disk speed / free-space check.
func RunStartupAudit() (slowDisk bool, lowSpace bool) {
	if os.Getenv("URNETWORK_SKIP_AUDIT") == "1" {
		tlog("[audit] System audit skipped (URNETWORK_SKIP_AUDIT=1)\n")
		return false, false
	}
	tlog("[audit] Running system checks...\n")
	profile := os.Getenv("URNETWORK_PROFILE")
	ramlogs := os.Getenv("URNETWORK_RAMLOGS")

	// If RAM logs are already ON (manually or via profile), skip disk benchmark
	skipDisk := (ramlogs == "1" || profile == "lowmem" || profile == "eco")

	return connect.RunSystemAudit(skipDisk)
}

// sanitizeRootPath filters the PATH environment variable when running as
// root, removing directories that are group/world-writable or have
// writable ancestor directories — a hardening measure against PATH
// hijacking (G-M2).
func sanitizeRootPath() {
	seen := map[string]bool{}
	ancestorSafe := map[string]bool{}
	isDirWritableByOthers := func(path string) bool {
		info, err := os.Stat(path)
		if err != nil {
			return true
		}
		if !info.IsDir() {
			return true
		}
		return info.Mode().Perm()&0o022 != 0
	}
	ancestorWritable := func(abs string) bool {
		p := filepath.Clean(abs)
		var chain []string
		for {
			parent := filepath.Dir(p)
			if parent == p {
				break
			}
			chain = append(chain, p)
			p = parent
		}
		for i := len(chain) - 1; i >= 1; i-- {
			dir := chain[i]
			if v, ok := ancestorSafe[dir]; ok {
				if !v {
					return true
				}
				continue
			}
			unsafe := isDirWritableByOthers(dir)
			ancestorSafe[dir] = !unsafe
			if unsafe {
				return true
			}
		}
		return false
	}
	var filtered []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		abs := dir
		if !filepath.IsAbs(dir) {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			continue
		}
		mode := info.Mode()
		if mode.Perm()&0o022 != 0 {
			continue
		}
		if strings.Contains(abs, "..") {
			continue
		}
		if ancestorWritable(abs) {
			continue
		}
		key := filepath.Clean(abs)
		if !seen[key] {
			seen[key] = true
			filtered = append(filtered, key)
		}
	}
	for _, dir := range []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		if info.Mode().Perm()&0o022 != 0 {
			continue
		}
		if ancestorWritable(dir) {
			continue
		}
		key := filepath.Clean(dir)
		if !seen[key] {
			seen[key] = true
			filtered = append(filtered, key)
		}
	}
	os.Setenv("PATH", strings.Join(filtered, string(os.PathListSeparator)))
}

// readSHMLog reads the last n lines from the shared-memory log file.
// If n <= 0, the entire file is returned.
func readSHMLog(path string, n int) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if n <= 0 {
		return string(b), nil
	}
	s := strings.TrimRight(string(b), "\n")
	lines := strings.Split(s, "\n")
	if n > len(lines) {
		n = len(lines)
	}
	return strings.Join(lines[len(lines)-n:], "\n") + "\n", nil
}

// providerLogs implements `provider logs [-n <lines>]`: tails the RAM log.
func providerLogs(opts docopt.Opts) {
	n, _ := opts.Int("-n")
	out, err := readSHMLog(shmLogPath, n)
	if err != nil {
		shmLogFatal(40, "no ramlogs found at %s — is URNETWORK_RAMLOGS=1 set?", shmLogPath)
	}
	fmt.Print(out)

	// Tail: follow the file from current position.
	f, err := os.Open(shmLogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: could not open log for tailing: %v\n", err)
		return
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		fmt.Fprintf(os.Stderr, "error: seek failed: %v\n", err)
		return
	}

	buf := make([]byte, 4096)
	for {
		nr, readErr := f.Read(buf)
		if nr > 0 {
			os.Stdout.Write(buf[:nr])
		}
		if readErr != nil && readErr != io.EOF {
			f.Close()
			fmt.Fprintf(os.Stderr, "error: read failed: %v\n", readErr)
			return
		}
		if readErr == io.EOF {
			// Detect ramlogs wrap: if the file shrunk behind our position, reopen from start.
			if pos, _ := f.Seek(0, io.SeekCurrent); pos > 0 {
				if fi, statErr := f.Stat(); statErr == nil && fi.Size() < pos {
					f.Close()
					newF, openErr := os.Open(shmLogPath)
					if openErr != nil {
						fmt.Fprintf(os.Stderr, "error: could not reopen log after wrap: %v\n", openErr)
						return
					}
					f = newF
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// printNetworkIdCmd implements `provider print-network-id <file>`:
// extracts and prints the network_id from a JWT file.
func printNetworkIdCmd(opts docopt.Opts) {
	filePath, _ := opts.String("<file>")
	data, err := os.ReadFile(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: cannot read %s: %v\n", filePath, err)
		os.Exit(1)
	}
	byJwt := strings.TrimSpace(string(data))
	networkId, ok := jwtNetworkId(byJwt)
	if !ok {
		fmt.Fprintf(os.Stderr, "ERROR: could not extract network_id from %s\n", filePath)
		os.Exit(1)
	}
	fmt.Println(networkId)
}

// ensure docopt import is used (indirect via opts type)
var _ docopt.Opts


// VersionStamp is an alternative version marker embedded as program data.
var VersionStamp string
