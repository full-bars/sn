package urnettools

import (
	"fmt"
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
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			reading, err := socketExtenderFn(Provider{StateDir: filepath.Join(home, ".urnetwork")})
			if err != nil {
				return err
			}
			if reading == nil {
				return fmt.Errorf("extender observation unavailable on control socket")
			}
			formatExtenderBlock(os.Stdout, *reading)
			return nil
		},
	}
	cmd.SetHelpTemplate(extenderLeafHelp)
	return cmd
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
