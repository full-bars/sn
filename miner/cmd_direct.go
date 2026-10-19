package miner

// cmd_direct.go ports the operator `provider direct [<state>]` command from the
// 3.23-fix v33 provider (provider/direct.go), matching its output and exit
// behavior. The read side (directControlPath/readDirectOverride/isDirectEnabled)
// already lives in proxy_manager_direct_grade.go; this adds the writer and the
// command.
//
// The command writes the persisted toggle <state>/direct and, when the provider
// is running (proxy.state records a start time), pokes the reload trigger so a
// running reload watcher applies the change without a restart — the same
// cross-process contract the other proxy subcommands use.

import (
	"fmt"
	"os"
	"strings"

	"github.com/docopt/docopt-go"
)

// writeDirectEnabled writes the direct toggle state atomically so a reload
// reading concurrently never sees a partial file.
func writeDirectEnabled(enabled bool) error {
	path, err := directControlPath()
	if err != nil {
		return err
	}
	val := "on"
	if !enabled {
		val = "off"
	}
	return atomicWriteFile(path, []byte(val+"\n"), 0600)
}

// clearDirectToggle removes the control file, restoring the default (on).
func clearDirectToggle() error {
	path, err := directControlPath()
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// cmdDirect implements `provider direct [<state>]`: toggles the runtime
// direct-IP toggle and triggers a reload of a running provider, or reports
// the current direct state if called without arguments or with "status".
func cmdDirect(opts docopt.Opts) {
	arg, _ := opts.String("<state>")
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "on":
		if err := writeDirectEnabled(true); err != nil {
			minerCmdFatal(72, "could not enable direct providing: %v", err)
		}
		fmt.Println("direct: providing on direct/local IP is now ENABLED")
	case "off":
		if err := writeDirectEnabled(false); err != nil {
			minerCmdFatal(72, "could not disable direct providing: %v", err)
		}
		fmt.Println("direct: providing on direct/local IP is now DISABLED (proxies only)")
	case "", "status":
		enabled, exists := readDirectOverride()
		if !exists {
			if os.Getenv("DISABLE_DIRECT_IP") == "1" {
				fmt.Println("direct: off (disabled by DISABLE_DIRECT_IP env var)")
			} else {
				fmt.Println("direct: on (default — no override file)")
			}
		} else if enabled {
			fmt.Println("direct: on (direct/local IP providing enabled)")
		} else {
			fmt.Println("direct: off (direct/local IP providing disabled)")
		}
		return
	default:
		fmt.Fprintf(os.Stderr, "Usage: provider direct [<state>] (on|off|status)\n")
		os.Exit(2)
	}

	// If the provider is running, trigger an immediate reload so the
	// change takes effect without a restart. Hold proxyStateMu for the read
	// so this cannot interleave with a reload's state write.
	proxyStateMu.Lock()
	state, err := readProxyState()
	proxyStateMu.Unlock()
	if err != nil {
		return
	}
	if state != nil && !state.StartedAt.IsZero() {
		triggerProxyReload()
	}
}
