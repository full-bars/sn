// Package walletsign is `provider wallet sign` and `provider wallet status`,
// shared by the release provider (cmd/provider) and the miner (cli/miner), so
// every sn build can sign the network wallet consent. Callers resolve the api
// url and the network credential `provider auth` saved; this package never
// reads provider state itself.
package walletsign

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// DefaultPort is the local port the signing page tries first.
const DefaultPort = snSignDefaultPort

// defaultAPIHost is the main network api; the coldkey signs only its
// challenges (see snCheckSigningApiUrl). Same value as provider and miner.
const defaultAPIHost = "api.bringyour.com"

// SignApi is the part of the sdk api the signing page uses.
type SignApi = snSignApi

// StatusApi is the part of the sdk api the status reads.
type StatusApi = snWalletStatusApi

// Sign serves the local signing page on 127.0.0.1 and stores the consent the
// wallet extension signs.
func Sign(ctx context.Context, out io.Writer, api SignApi, apiUrl string, coldkeySs58 string, port int, openBrowser bool) error {
	return snWalletSignUi(ctx, out, api, apiUrl, coldkeySs58, port, openBrowser)
}

// Status prints the wallet consent the platform holds for this network.
func Status(ctx context.Context, out io.Writer, api StatusApi) error {
	return snWalletStatus(ctx, out, api)
}

func snNormalizeSignatureHex(signature string) (string, error) {
	clean := strings.TrimSpace(signature)
	clean = strings.TrimPrefix(strings.TrimPrefix(clean, "0x"), "0X")
	raw, err := hex.DecodeString(clean)
	if err != nil {
		return "", fmt.Errorf("--signature must be hex: %w", err)
	}
	if len(raw) != 64 {
		return "", fmt.Errorf("--signature must be a 64-byte sr25519 signature, got %d bytes", len(raw))
	}
	return "0x" + hex.EncodeToString(raw), nil
}
