package urnettools

import "github.com/spf13/cobra"

// Keep this description in step with docs/Extender.md and the SDK's role.
const extenderAboutText = `The extender helps clients reach URnetwork when its platform addresses are blocked.
This direct provider listens for clients and relays their traffic. It is a public
service, not a source of earnings.

  urnet-tools extender status   show the live role status
  urnet-tools extender setup    configure host privileges and firewall access

PORTS
  TCP 443   required for activation. Another process holding it prevents the
            role from starting. Non-root providers need permission to bind it.
  UDP 443   optional QUIC carrier.
  UDP 4053  optional DNS carrier.
  UDP 53    optional DNS carrier, tried when available.

The platform checks each address family before listing it. A public address and
inbound access are needed. A provider using proxies cannot be the direct role.
A container also needs the ports published and the host firewall configured.
For setup, activation and troubleshooting, see docs/Extender.md in
https://github.com/full-bars/sn.
`

func newExtenderAboutCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "about",
		Short:   "explain the extender role",
		Long:    "Explain the extender role, its ports and how to prepare the host.",
		Example: "  urnet-tools extender about",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write([]byte(extenderAboutText))
			return err
		},
	}
}
