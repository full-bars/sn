package urnettools

import (
	"os"

	"github.com/spf13/cobra"
)

// newExtenderCmd creates the `urnet-tools extender` parent command.
func newExtenderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "extender",
		Short: "extender role status and configuration",
		Long:  "Manage and inspect the provider extender role: check port occupancy, platform bind requirements, firewall status, and execute the one-time privileged setup.",
		Example: `  urnet-tools extender status
  urnet-tools extender status --json
  urnet-tools extender setup --enable
  urnet-tools extender setup --non-interactive`,
	}

	cmd.AddCommand(
		newExtenderStatusCmd(),
		newExtenderSetupCmd(),
	)

	return cmd
}

func newExtenderStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "read-only status of extender role and port bindings",
		Long:  "Read-only check of the extender role: reports platform and user ability to bind TCP 443, port occupancy (TCP 443, UDP 443, UDP 4053), role preference, whether the role is bound and activated, and platform-specific remedies when something is wrong. Does not change any host or provider state. Supports --json for machines.",
		Example: `  urnet-tools extender status
  urnet-tools extender status --json
  urnet-tools extender status --network mynetwork`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return parseGlobal(args, func(force, dryRun bool, rest []string) error {
				t, restNoTarget, err := parseTargetFlagsLenient(rest)
				if err != nil {
					return err
				}
				jsonFlag := asJSON
				for _, a := range restNoTarget {
					if a == "--json" || a == "-j" {
						jsonFlag = true
					}
				}

				providers := Discover()
				var target Provider
				if len(providers) > 0 {
					target, _, _ = selectTargetOrSoleAccessible(providers, t, false)
				}

				report, err := GatherExtenderStatus("", target, nil, nil, nil)
				if err != nil {
					return err
				}
				return FormatExtenderStatus(os.Stdout, report, jsonFlag)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "format output as JSON")
	return cmd
}

func newExtenderSetupCmd() *cobra.Command {
	var enable bool
	var nonInteractive bool

	cmd := &cobra.Command{
		Use:   "setup",
		Short: "one-time privileged configuration for extender role",
		Long:  "Perform the one-time privileged host configuration for the extender role: grant cap_net_bind_service=+ep on Linux, set and persist net.inet.ip.portrange.reservedhigh=0 on FreeBSD, configure Windows Firewall rules on Windows, or configure a root LaunchDaemon on macOS. Verifies ports before acting and with a real bind after. Idempotent: repeated runs report 'already configured' and make no changes. Pass --enable to also set the role preference for the current provider.",
		Example: `  urnet-tools extender setup
  urnet-tools extender setup --enable
  urnet-tools extender setup --enable --non-interactive`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return parseGlobal(args, func(force, dryRun bool, rest []string) error {
				t, restNoTarget, err := parseTargetFlagsLenient(rest)
				if err != nil {
					return err
				}

				enableFlag := enable
				nonInteractiveFlag := nonInteractive

				for _, a := range restNoTarget {
					switch a {
					case "--enable", "-e":
						enableFlag = true
					case "--non-interactive":
						nonInteractiveFlag = true
					}
				}

				opts := ExtenderSetupOptions{
					Enable:         enableFlag,
					NonInteractive: nonInteractiveFlag,
					Force:          force,
					DryRun:         dryRun,
					Target:         t,
					Stdout:         os.Stdout,
					Stderr:         os.Stderr,
					Argv:           args,
				}
				return RunExtenderSetup(opts)
			})
		},
	}
	cmd.Flags().BoolVarP(&enable, "enable", "e", false, "enable extender role preference for provider")
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "fail instead of prompting if root is required")
	return cmd
}
