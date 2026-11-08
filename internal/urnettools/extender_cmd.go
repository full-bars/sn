package urnettools

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func newExtenderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "extender",
		Short:   "extender role status and configuration",
		Long:    "Inspect the live extender role or configure host privileges once.",
		Example: "  urnet-tools extender status\n  urnet-tools extender setup",
	}
	cmd.AddCommand(newExtenderStatusCmd(), newExtenderSetupCmd(), newExtenderAboutCmd())
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

const extenderSetupUsage = "usage: urnet-tools extender setup [-y|--yes] [-f|--force] [-n|--dry-run] [target]"

// parseExtenderSetupArgs parses flags specific to `urnet-tools extender setup`.
func parseExtenderSetupArgs(args []string) (yes, dryRun bool, target Target, err error) {
	var filtered []string
	for _, tok := range args {
		switch tok {
		case "-y", "--yes", "-f", "--force":
			yes = true
		case "-n", "--dry-run":
			dryRun = true
		default:
			filtered = append(filtered, tok)
		}
	}
	target, rest, err := parseTargetFlags(filtered)
	if err != nil {
		return false, false, Target{}, err
	}
	for _, tok := range rest {
		if strings.EqualFold(strings.TrimSpace(tok), "setup") {
			continue
		}
		return false, false, Target{}, fmt.Errorf("%s (unexpected %q)", extenderSetupUsage, tok)
	}
	return yes, dryRun, target, nil
}

var runExtenderSetupFn = RunExtenderSetup

func cmdExtenderSetup(args []string) error {
	yes, dryRun, target, err := parseExtenderSetupArgs(args)
	if err != nil {
		return err
	}
	argv := append([]string{"extender", "setup"}, args...)
	if !yes {
		argv = append(argv, "--yes")
	}
	return runExtenderSetupFn(ExtenderSetupOptions{
		Yes:    yes,
		Force:  yes,
		DryRun: dryRun,
		Target: target,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Argv:   argv,
	})
}

func newExtenderSetupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "setup [-y|--yes] [-f|--force] [-n|--dry-run] [target]",
		Short:              "one-time privileged configuration for extender role",
		Long:               "Perform one-time privileged host configuration for the extender role. Inspects port 443 occupancy, binary bind capability and host firewall rules, prints the exact commands needed, and asks for confirmation before changing anything (use -y/--yes for non-interactive runs).",
		Example:            "  urnet-tools extender setup\n  urnet-tools extender setup --yes\n  urnet-tools extender setup --dry-run",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
				return cmd.Help()
			}
			return cmdExtenderSetup(args)
		},
	}
	cmd.SetHelpTemplate(extenderLeafHelp)
	return cmd
}
