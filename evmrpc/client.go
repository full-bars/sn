// Shared dial boundary for direct EVM clients. HTTP response admission is
// local to each client; WebSocket and IPC keep geth's existing transports.
package evmrpc

import (
	"context"
	"net/http"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Construct a client without probing, changing endpoints or retrying calls.
// The caller retains ownership of deadlines, chain identity and signed work.
func DialContext(ctx context.Context, endpoint string) (*ethclient.Client, error) {
	return dialContext(ctx, endpoint, http.DefaultTransport)
}

// The immutable transport is shared only with this client's HTTP requests.
func dialContext(ctx context.Context, endpoint string, base http.RoundTripper) (*ethclient.Client, error) {
	client, err := rpc.DialOptions(ctx, endpoint, rpc.WithHTTPClient(&http.Client{
		Transport: &responseTransport{base: base, maximumBytes: maximumResponseBytes},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}))
	if err != nil {
		return nil, err
	}
	return ethclient.NewClient(client), nil
}
