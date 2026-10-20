package miner

// network_cmd.go — the `provider choose_network` command handler.
// Saving/resetting logic lives in network.go; this file owns the CLI
// glue (argument extraction, user-facing output).

import (
	"fmt"
	"path/filepath"

	"github.com/docopt/docopt-go"
)

// chooseNetworkCmd implements `provider choose_network` against the real
// provider state directory.
func chooseNetworkCmd(opts docopt.Opts) error {
	dir, err := providerStateDir()
	if err != nil {
		return err
	}
	return chooseNetworkCmdIn(opts, dir)
}

// chooseNetworkCmdIn implements `provider choose_network <api_url>
// <connect_url>`, `provider choose_network --reset` and
// `provider choose_network --show` against an explicit state directory.
//
// It takes the directory (rather than resolving it) so the tests never
// touch the real ~/.urnetwork, and returns errors rather than calling
// os.Exit so they can drive it without taking the test binary down with
// them. Run turns a returned error into the nonzero exit.
func chooseNetworkCmdIn(opts docopt.Opts, dir string) error {
	if show, _ := opts.Bool("--show"); show {
		cfg, ok, err := readNetworkConfig(dir)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Printf(
				"no custom network saved; using the main network\n    api_url: %s\n    connect_url: %s\n",
				DefaultApiUrl, DefaultConnectUrl,
			)
			return nil
		}
		fmt.Printf(
			"custom network saved at %s\n    api_url: %s\n    connect_url: %s\n",
			filepath.Join(dir, networkConfigFileName), cfg.ApiUrl, cfg.ConnectUrl,
		)
		if warning := insecureNetworkWarning(cfg.ApiUrl, cfg.ConnectUrl); warning != "" {
			fmt.Println(warning)
		}
		return nil
	}

	// The switch notice is about credentials issued by a DIFFERENT network, so
	// it only belongs on a command that actually moves the effective api_url.
	// Each branch compares before it writes or resets, while the previous
	// selection is still on disk: resetting with nothing saved and re-saving
	// the same URL pair leave the network where it was, and a false notice
	// costs a single-use auth code.
	if reset, _ := opts.Bool("--reset"); reset {
		changed := apiUrlChanged(dir, DefaultApiUrl)
		if err := resetNetworkConfig(dir); err != nil {
			return fmt.Errorf("failed to reset network: %w", err)
		}
		fmt.Println("network reset to the main network")
		if notice := networkSwitchNotice(dir, changed); notice != "" {
			fmt.Println(notice)
		}
		return nil
	}

	apiUrl, err := opts.String("<api_url>")
	if err != nil {
		return fmt.Errorf("missing <api_url>: %w", err)
	}
	connectUrl, err := opts.String("<connect_url>")
	if err != nil {
		return fmt.Errorf("missing <connect_url>: %w", err)
	}

	changed := apiUrlChanged(dir, apiUrl)
	if err := writeNetworkConfig(dir, apiUrl, connectUrl); err != nil {
		return fmt.Errorf("network not saved: %w", err)
	}
	fmt.Printf("network saved: api_url=%s connect_url=%s\n", apiUrl, connectUrl)
	if warning := insecureNetworkWarning(apiUrl, connectUrl); warning != "" {
		fmt.Println(warning)
	}
	if notice := networkSwitchNotice(dir, changed); notice != "" {
		fmt.Println(notice)
	}
	return nil
}
