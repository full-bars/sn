package miner

// `provider claim` carries the provider token from its first read, so the
// endpoint must be secured once before the SDK owns a request. A dialer that
// sends every destination to a local listener makes a request that should never
// happen visible without DNS or a remote host.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// claimEndpointProbe counts every request that reaches the transport, and
// redirects every dial to itself, so a read against a name that would not
// resolve is still observable.
func claimEndpointProbe(t *testing.T) (*httptest.Server, *atomic.Int32, finiteClaimHooks) {
	t.Helper()
	requests := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hello" {
			return
		}
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(&sdk.SnEpochResult{Epoch: 2})
	}))
	t.Cleanup(server.Close)
	hooks := finiteClaimTestHooks()
	listener := server.Listener.Addr().String()
	hooks.strategySettings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", listener)
		},
	}
	return server, requests, hooks
}

// The relay rule: a plaintext http endpoint that is not a loopback literal is
// refused before the token request, whichever way the url was chosen.
func TestFiniteClaimRefusesCleartextRemoteApiUrlBeforeAnyRequest(t *testing.T) {
	for _, test := range []struct {
		name  string
		opts  docopt.Opts
		saved string
	}{
		{name: "flag", opts: docopt.Opts{"--api_url": "http://claim.invalid"}},
		{name: "saved network", saved: "http://claim.invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			setTestProviderJwt(t)
			_, requests, hooks := claimEndpointProbe(t)
			if test.saved != "" {
				raw := []byte(`{"api_url":"` + test.saved + `","connect_url":"wss://claim.invalid"}`)
				if err := os.WriteFile(filepath.Join(os.Getenv("URNETWORK_STATE_DIR"), networkConfigFileName), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := runFiniteClaim(t.Context(), test.opts, hooks)
			if err == nil {
				t.Fatal("cleartext claim endpoint was accepted")
			}
			if !strings.Contains(err.Error(), "https") {
				t.Fatalf("refusal does not name the transport rule: %v", err)
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("claim sent %d request(s) to a cleartext endpoint before the refusal", got)
			}
		})
	}
}

// A url whose visible prefix names the main network but whose real host follows
// the @ would receive the claim token. The shared token rule refuses any
// credentials, query or fragment, so the command must refuse this before the
// first read.
func TestFiniteClaimRefusesApiUrlWithCredentialsBeforeAnyRequest(t *testing.T) {
	setTestProviderJwt(t)
	server, requests, hooks := claimEndpointProbe(t)
	apiUrl := "http://api.bringyour.com@" + server.Listener.Addr().String()
	err := runFiniteClaim(t.Context(), docopt.Opts{"--api_url": apiUrl}, hooks)
	if err == nil {
		t.Fatal("claim api url with embedded credentials was accepted")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("claim sent %d request(s) to the url's real host before the refusal: %v", got, err)
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("refusal does not name the url rule: %v", err)
	}
}
