package provider

// wallet_sign_cmd.go -- `provider wallet sign` and `provider wallet status` in
// the release provider, so an operator signs the network wallet consent with
// the same binary they run. The page and the status live in walletsign, shared
// with the miner. Both use the network credential `provider auth` saved
// (~/.urnetwork/jwt): the consent is the network owner's, and the server
// refuses a client token for it.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"

	"github.com/urfoundation/sn/walletsign"
)

func walletCommandContext(opts docopt.Opts) (string, context.Context, context.CancelFunc, *connect.ClientStrategy) {
	apiUrl, err := resolveApiUrl(opts)
	if err != nil {
		fmt.Printf("network config error: %s\n", err)
		os.Exit(1)
	}

	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(event.Ctx())
	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)
	return apiUrl, ctx, cancel, clientStrategy
}

// walletSign implements `provider wallet sign <coldkey_ss58> [--port=<port>]
// [--no-open]`. Run it on your own computer, never on a fleet node.
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
	// --port is shared with `provide`, where its default is 0
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

// walletStatus implements `provider wallet status`. Read-only.
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
