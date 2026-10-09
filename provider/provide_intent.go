package provider

import (
	"os"

	"github.com/urnetwork/connect"
)

// provideIntentEnabled reports whether this provider declares provide intent.
// The platform limits how many clients one network may have connected, and
// exempts a client only while it declares intent and qualifies as a public
// provider. On by default; URNETWORK_PROVIDE_INTENT=0 turns it off.
func provideIntentEnabled() bool {
	return os.Getenv("URNETWORK_PROVIDE_INTENT") != "0"
}

// newProviderClientAuth builds the ClientAuth every platform transport of this
// provider presents, so the first dial and every renewal declare the same
// intent. The connect transport carries it on the H1 header and on the Auth
// frame of the H3 and legacy H1 modes.
func newProviderClientAuth(byClientJwt string, instanceId connect.Id) *connect.ClientAuth {
	return &connect.ClientAuth{
		ByJwt:         byClientJwt,
		InstanceId:    instanceId,
		AppVersion:    RequireVersion(),
		ProvideIntent: provideIntentEnabled(),
	}
}

// newProviderAuthClientArgsForMint builds the request that creates a new client.
// The platform records provider status only when a client is created, so this
// is the request that matters; renewal repeats it for consistency.
func newProviderAuthClientArgsForMint(description string) *connect.AuthNetworkClientArgs {
	return &connect.AuthNetworkClientArgs{
		Description:   description,
		DeviceSpec:    "",
		ProvideIntent: provideIntentEnabled(),
	}
}
