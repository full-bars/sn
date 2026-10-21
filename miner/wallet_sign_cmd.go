package miner

// wallet_sign_cmd.go -- the command wiring and the read-only status of
// `provider wallet sign` and `provider wallet status`. Both use the network
// credential `provider auth` saved (~/.urnetwork/jwt): the network wallet
// consent is the network owner's, and the server refuses a client token for it.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/sdk"

	"github.com/urfoundation/sn/walletsign"
)

// walletSign implements `provider wallet sign <coldkey_ss58> [--port=<port>]
// [--no-open]`: one local page where a browser wallet extension signs the
// network wallet consent. Run it on your own computer, never on a fleet node.
func walletSign(opts docopt.Opts) {
	apiUrl, ctx, cancel, clientStrategy := walletCommandContext(opts)
	defer cancel()
	defer clientStrategy.Close()

	byJwt, err := readNetworkJwt()
	if err != nil {
		fmt.Printf("wallet sign failed: %s\n", err)
		os.Exit(1)
	}
	coldkeySs58, _ := opts.String("<coldkey_ss58>")
	port := walletsign.DefaultPort
	if text, err := opts.String("--port"); err == nil {
		if value, err := strconv.Atoi(strings.TrimSpace(text)); err == nil && 0 < value && value < 65536 {
			port = value
		}
	}
	noOpen, _ := opts.Bool("--no-open")

	api := sdk.NewApi(ctx, clientStrategy, apiUrl)
	defer func() {
		_ = api.CloseAndWait(context.Background())
	}()
	api.SetByJwt(byJwt)
	if err := walletsign.Sign(ctx, os.Stdout, api, apiUrl, coldkeySs58, port, !noOpen); err != nil {
		fmt.Printf("wallet sign failed: %s\n", err)
		os.Exit(1)
	}
}

// walletStatus implements `provider wallet status`.
func walletStatus(opts docopt.Opts) {
	apiUrl, ctx, cancel, clientStrategy := walletCommandContext(opts)
	defer cancel()
	defer clientStrategy.Close()
	ctx, cancelTimeout := context.WithTimeout(ctx, 30*time.Second)
	defer cancelTimeout()

	byJwt, err := readNetworkJwt()
	if err != nil {
		fmt.Printf("%s\n", err)
		os.Exit(1)
	}
	api := sdk.NewApi(ctx, clientStrategy, apiUrl)
	defer func() {
		_ = api.CloseAndWait(context.Background())
	}()
	api.SetByJwt(byJwt)
	if err := walletsign.Status(ctx, os.Stdout, api); err != nil {
		fmt.Printf("%s\n", err)
		os.Exit(1)
	}
}
