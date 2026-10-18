package miner

// cmd_network_id.go ports `provider print-network-id <file>` from the 3.23-fix
// v33 provider (provider/main.go printNetworkIdCmd and jwtNetworkId). It reads
// a JWT file and prints the network_id claim, so a deployment can wire an
// account's network id without decoding the token by hand.

import (
	"fmt"
	"os"
	"strings"

	"github.com/docopt/docopt-go"
	gojwt "github.com/golang-jwt/jwt/v5"
)

// jwtNetworkId extracts the network_id claim from a JWT without verifying the
// signature: the caller only compares it.
func jwtNetworkId(byJwt string) (string, bool) {
	parser := gojwt.NewParser()
	tok, _, err := parser.ParseUnverified(byJwt, gojwt.MapClaims{})
	if err != nil {
		return "", false
	}
	claims, ok := tok.Claims.(gojwt.MapClaims)
	if !ok {
		return "", false
	}
	networkId, ok := claims["network_id"].(string)
	return networkId, ok
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
