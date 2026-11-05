package urnettools

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newExtenderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "extender",
		Short:   "extender role status and configuration",
		Long:    "Inspect the live extender role or configure host privileges once.",
		Example: "  urnet-tools extender status\n  urnet-tools extender setup",
	}
	cmd.AddCommand(newExtenderStatusCmd(), newExtenderSetupCmd())
	return cmd
}

func newExtenderStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "status",
		Short:              "show the live extender observation from the control socket",
		Long:               "Show the current extender observation from the provider control socket. This command takes no flags.",
		Example:            "  urnet-tools extender status",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
				return cmd.Help()
			}
			if len(args) != 0 {
				return fmt.Errorf("unknown argument: %s", args[0])
			}
			provs, err := extenderStatusProviders()
			if err != nil {
				return err
			}
			return runExtenderStatus(os.Stdout, provs)
		},
	}
	cmd.SetHelpTemplate(extenderLeafHelp)
	return cmd
}

// discoverProvidersFn is Discover behind a seam so the status resolution is
// testable without a real host inventory.
var discoverProvidersFn = Discover

// extenderStatusProviders names the providers whose control sockets `extender
// status` asks. The command takes no flags, so the target is whatever the host
// makes unambiguous: the sole (or sole accessible, or default) provider, else
// every discovered provider, else the invoking user's own state directory.
func extenderStatusProviders() ([]Provider, error) {
	providers := discoverProvidersFn()
	if len(providers) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return []Provider{{StateDir: filepath.Join(home, ".urnetwork")}}, nil
	}
	if p, _, err := selectTargetOrSoleAccessible(providers, Target{}, false); err == nil {
		return []Provider{p}, nil
	}
	return providers, nil
}

// runExtenderStatus prints one extender block per provider. A provider whose
// socket gives no observation is reported as unavailable and makes the command
// exit non-zero: nothing is inferred from state files or logs.
func runExtenderStatus(w io.Writer, provs []Provider) error {
	var firstErr error
	unavailable := 0
	for _, p := range provs {
		reading, err := socketExtenderFn(p)
		if err == nil && reading == nil {
			err = fmt.Errorf("extender observation unavailable on control socket")
		}
		if err != nil {
			unavailable++
			if firstErr == nil {
				firstErr = err
			}
			if len(provs) > 1 {
				fmt.Fprintf(w, "\n%s: extender unavailable: %v\n", providerLabel(p), err)
			}
			continue
		}
		if len(provs) > 1 {
			fmt.Fprintf(w, "\n%s", providerLabel(p))
		}
		formatExtenderBlock(w, *reading)
	}
	switch {
	case unavailable == 0:
		return nil
	case len(provs) == 1:
		return firstErr
	}
	return fmt.Errorf("extender unavailable on %d of %d providers: %w", unavailable, len(provs), firstErr)
}

const extenderLeafHelp = "{{.Long}}\n\nUsage:\n  {{.CommandPath}}\n\nExamples:\n{{.Example}}\n"

func newExtenderSetupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "setup",
		Short:              "one-time privileged configuration for extender role",
		Long:               "Perform one-time privileged host configuration for the extender role. This command takes no flags.",
		Example:            "  urnet-tools extender setup",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
				return cmd.Help()
			}
			if len(args) != 0 {
				return fmt.Errorf("unknown argument: %s", args[0])
			}
			return RunExtenderSetup(ExtenderSetupOptions{Stdout: os.Stdout, Stderr: os.Stderr})
		},
	}
	cmd.SetHelpTemplate(extenderLeafHelp)
	return cmd
}
