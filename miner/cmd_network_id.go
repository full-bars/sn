package miner

// cmd_network_id.go ports `provider print-network-id <file>` from the 3.23-fix
// v33 provider (provider/main.go printNetworkIdCmd and jwtNetworkId). It reads
// a JWT file and prints the network_id claim, so a deployment can wire an
// account's network id without decoding the token by hand.
//
// jwtNetworkId itself is shared with the hot-restart machinery and lives in
// client_jwt_hotrestart.go; this file keeps only the command entry point.

import (
	"fmt"
	"os"
	"strings"

	"github.com/docopt/docopt-go"
)

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
