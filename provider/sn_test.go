package provider

// sn_test.go — tests for subnet bridge, argument parsing, head signing,
// proxy registration, bandwidth reporting, and contract metrics.
//
// ADAPTATION NOTES (v2026 migration):
// - Ported from main to package provider.
// - Tests byte32/evm address parsing, Ed25519 head bind signing,
//   SNProvider lifecycle, proxy registration/management, bandwidth reporting,
//   contract metrics recording, and network ranking RPC integration.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
	"github.com/ethereum/go-ethereum/common"
	"github.com/urnetwork/connect"
	"github.com/urfoundation/sn/provider/bandwidth"
)

func TestParseBytes32Arg(t *testing.T) {
	valid32 := "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	var wantValid [32]byte
	for i := range wantValid {
		wantValid[i] = byte(i + 1)
	}

	tests := []struct {
		name    string
		field   string
		input   string
		want    [32]byte
		wantErr bool
	}{
		{
			name:  "valid hex without 0x prefix",
			field: "--hotkey",
			input: valid32,
			want:  wantValid,
		},
		{
			name:  "valid hex with 0x prefix",
			field: "--hotkey",
			input: "0x" + valid32,
			want:  wantValid,
		},
		{
			name:  "valid hex with 0X prefix (uppercase)",
			field: "--hotkey",
			input: "0X" + valid32,
			want:  wantValid,
		},
		{
			name:  "valid hex with surrounding whitespace",
			field: "--hotkey",
			input: "  0x" + valid32 + "  ",
			want:  wantValid,
		},
		{
			name:  "uppercase hex digits",
			field: "--hotkey",
			input: "0x" + strings.ToUpper(valid32),
			want:  wantValid,
		},
		{
			name:    "too short",
			field:   "--hotkey",
			input:   "0x" + valid32[:62],
			wantErr: true,
		},
		{
			name:    "too long",
			field:   "--hotkey",
			input:   "0x" + valid32 + "ff",
			wantErr: true,
		},
		{
			name:    "odd number of hex digits",
			field:   "--hotkey",
			input:   "0x" + valid32[:63],
			wantErr: true,
		},
		{
			name:    "non-hex characters",
			field:   "--hotkey",
			input:   "0x" + strings.Repeat("zz", 32),
			wantErr: true,
		},
		{
			name:    "empty string",
			field:   "--hotkey",
			input:   "",
			wantErr: true,
		},
		{
			name:    "bare 0x with nothing else",
			field:   "--hotkey",
			input:   "0x",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBytes32Arg(tt.field, tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseBytes32Arg(%q, %q) = %x, nil; want error", tt.field, tt.input, got)
				}
				if !strings.Contains(err.Error(), tt.field) {
					t.Errorf("parseBytes32Arg error %q does not mention field %q", err.Error(), tt.field)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBytes32Arg(%q, %q) unexpected error: %s", tt.field, tt.input, err)
			}
			if got != tt.want {
				t.Errorf("parseBytes32Arg(%q, %q) = %x; want %x", tt.field, tt.input, got, tt.want)
			}
		})
	}
}

func TestParseEvmAddressArg(t *testing.T) {
	valid20 := "0102030405060708090a0b0c0d0e0f1011121314"
	var wantValid [20]byte
	for i := range wantValid {
		wantValid[i] = byte(i + 1)
	}

	tests := []struct {
		name    string
		field   string
		input   string
		want    [20]byte
		wantErr bool
	}{
		{
			name:  "valid address without 0x prefix",
			field: "--registrant",
			input: valid20,
			want:  wantValid,
		},
		{
			name:  "valid address with 0x prefix",
			field: "--registrant",
			input: "0x" + valid20,
			want:  wantValid,
		},
		{
			name:  "valid address with whitespace",
			field: "--registrant",
			input: " 0x" + valid20 + "\n",
			want:  wantValid,
		},
		{
			name:    "too short (looks like a bytes32 truncated)",
			field:   "--registrant",
			input:   "0x" + valid20[:38],
			wantErr: true,
		},
		{
			name:    "too long (32-byte value passed where 20 expected)",
			field:   "--registrant",
			input:   "0x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
			wantErr: true,
		},
		{
			name:    "non-hex characters",
			field:   "--registrant",
			input:   "0x" + strings.Repeat("gg", 20),
			wantErr: true,
		},
		{
			name:    "odd number of hex digits",
			field:   "--registrant",
			input:   "0x" + valid20[:39],
			wantErr: true,
		},
		{
			name:    "bare 0x with nothing else",
			field:   "--registrant",
			input:   "0x",
			wantErr: true,
		},
		{
			name:    "empty string",
			field:   "--registrant",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseEvmAddressArg(tt.field, tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseEvmAddressArg(%q, %q) = %x, nil; want error", tt.field, tt.input, got)
				}
				if !strings.Contains(err.Error(), tt.field) {
					t.Errorf("parseEvmAddressArg error %q does not mention field %q", err.Error(), tt.field)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEvmAddressArg(%q, %q) unexpected error: %s", tt.field, tt.input, err)
			}
			if got != tt.want {
				t.Errorf("parseEvmAddressArg(%q, %q) = %x; want %x", tt.field, tt.input, got, tt.want)
			}
		})
	}
}

func TestParseEvmAddressArg_UppercasePrefix(t *testing.T) {
	valid20 := "0102030405060708090a0b0c0d0e0f1011121314"
	var want [20]byte
	for i := range want {
		want[i] = byte(i + 1)
	}

	got, err := parseEvmAddressArg("--registrant", "0X"+valid20)
	if err != nil {
		t.Fatalf("parseEvmAddressArg(%q) unexpected error: %s", "0X"+valid20, err)
	}
	if got != want {
		t.Errorf("parseEvmAddressArg(%q) = %x; want %x", "0X"+valid20, got, want)
	}
}

func fixedEvmAddress(b byte) common.Address {
	var out common.Address
	for i := range out {
		out[i] = b
	}
	return out
}

func fixedBytes32(b byte) (out [32]byte) {
	for i := range out {
		out[i] = b
	}
	return out
}

func TestSnSignBindHead_FieldsCopiedAndSignatureVerifies(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %s", err)
	}

	registrant := fixedEvmAddress(0xAB)
	hotkey := fixedBytes32(0xCD)
	digest := fixedBytes32(0xEF)

	intent := snSignBindHead(priv, registrant, hotkey, digest)

	if intent.hotkey != hotkey {
		t.Errorf("intent.hotkey = %x; want %x", intent.hotkey, hotkey)
	}
	if intent.digest != digest {
		t.Errorf("intent.digest = %x; want %x", intent.digest, digest)
	}
	if intent.registrant != registrant {
		t.Errorf("intent.registrant = %x; want %x", intent.registrant, registrant)
	}
	if !bytes.Equal(intent.clientId[:], pub) {
		t.Errorf("intent.clientId = %x; want the ed25519 public key %x", intent.clientId, pub)
	}
	if len(intent.clientIdSig) != ed25519.SignatureSize {
		t.Fatalf("intent.clientIdSig length = %d; want %d", len(intent.clientIdSig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, digest[:], intent.clientIdSig) {
		t.Error("intent.clientIdSig does not verify against digest with the signing key's public key")
	}
}

func TestSnSignBindHead_SignatureIsBoundToDigest(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %s", err)
	}

	registrant := fixedEvmAddress(0x01)
	hotkey := fixedBytes32(0x02)
	digestA := fixedBytes32(0xAA)
	digestB := fixedBytes32(0xBB)

	intentA := snSignBindHead(priv, registrant, hotkey, digestA)
	intentB := snSignBindHead(priv, registrant, hotkey, digestB)

	if bytes.Equal(intentA.clientIdSig, intentB.clientIdSig) {
		t.Error("signatures over two different digests must differ")
	}

	pub := priv.Public().(ed25519.PublicKey)
	if ed25519.Verify(pub, digestB[:], intentA.clientIdSig) {
		t.Error("intentA's signature unexpectedly verifies against digestB")
	}
}

func TestSnSignBindHead_DeterministicForSameInputs(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %s", err)
	}

	registrant := fixedEvmAddress(0x03)
	hotkey := fixedBytes32(0x04)
	digest := fixedBytes32(0x05)

	intent1 := snSignBindHead(priv, registrant, hotkey, digest)
	intent2 := snSignBindHead(priv, registrant, hotkey, digest)

	if !bytes.Equal(intent1.clientIdSig, intent2.clientIdSig) {
		t.Error("snSignBindHead produced different signatures for identical inputs")
	}
}

// ---------------------------------------------------------------------
// SNProvider Tests: Lifecycle, Proxy Registration, Bandwidth & Grading
// ---------------------------------------------------------------------

func TestSNProvider_Lifecycle(t *testing.T) {
	p := NewSNProvider(SNProviderConfig{})
	if p.Running() {
		t.Fatal("new provider should not be running")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if !p.Running() {
		t.Fatal("provider should be running after Start")
	}

	// Calling Start again should be idempotent
	if err := p.Start(ctx); err != nil {
		t.Fatalf("second Start failed: %v", err)
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if p.Running() {
		t.Fatal("provider should not be running after Stop")
	}
}

func TestSNProvider_ProxyManagementAndBandwidth(t *testing.T) {
	reg := bandwidth.NewRegistry()
	p := NewSNProvider(SNProviderConfig{
		BandwidthReg: reg,
	})

	proxy1 := "10.0.0.1:1080"
	proxy2 := "10.0.0.2:1080"

	entry1 := p.RegisterProxy(0, proxy1)
	if entry1.Index != 0 || entry1.Address != proxy1 || !entry1.Active {
		t.Fatalf("unexpected entry1: %+v", entry1)
	}

	entry2 := p.RegisterProxy(1, proxy2)
	if entry2.Index != 1 || entry2.Address != proxy2 || !entry2.Active {
		t.Fatalf("unexpected entry2: %+v", entry2)
	}

	proxies := p.Proxies()
	if len(proxies) != 2 {
		t.Fatalf("expected 2 proxies, got %d", len(proxies))
	}

	got1, ok := p.GetProxy(proxy1)
	if !ok || got1.Address != proxy1 {
		t.Fatalf("expected proxy1, got: %v", got1)
	}

	// Update grade
	p.UpdateProxyGrade(proxy1, "tier-1")
	if grade := p.ProxyGrade(proxy1); grade != "tier-1" {
		t.Fatalf("expected tier-1, got %q", grade)
	}

	// Simulate bandwidth usage
	entry1.Bandwidth.TotalRx.Add(1024)
	entry1.Bandwidth.TotalTx.Add(2048)

	bwMap := p.ReportBandwidth()
	if len(bwMap) != 2 {
		t.Fatalf("expected 2 bandwidth entries, got %d", len(bwMap))
	}
	if bwMap[proxy1].TotalRx.Load() != 1024 || bwMap[proxy1].TotalTx.Load() != 2048 {
		t.Fatalf("unexpected bandwidth values for proxy1: rx=%d tx=%d",
			bwMap[proxy1].TotalRx.Load(), bwMap[proxy1].TotalTx.Load())
	}

	// Record contract metrics
	p.RecordContractAcquisition(proxy1)
	p.RecordContractDenial(proxy1)

	// Unregister
	p.UnregisterProxy(proxy1)
	if _, ok := p.GetProxy(proxy1); ok {
		t.Fatal("proxy1 should be unregistered")
	}
	if len(p.Proxies()) != 1 {
		t.Fatalf("expected 1 proxy remaining, got %d", len(p.Proxies()))
	}
}

func TestResolveApiUrl(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", tempDir)

	// Case 1: Flag provided
	flagOpts := docopt.Opts{"--api_url": "https://custom.api.com"}
	got, err := resolveApiUrl(flagOpts)
	if err != nil {
		t.Fatalf("resolveApiUrl with flag failed: %v", err)
	}
	if got != "https://custom.api.com" {
		t.Errorf("got %q, want https://custom.api.com", got)
	}

	// Case 2: No flag, no saved config -> default
	emptyOpts := docopt.Opts{}
	gotDefault, err := resolveApiUrl(emptyOpts)
	if err != nil {
		t.Fatalf("resolveApiUrl default failed: %v", err)
	}
	if gotDefault != DefaultApiUrl {
		t.Errorf("got %q, want %q", gotDefault, DefaultApiUrl)
	}

	// Case 3: Saved network config
	cfgPath := filepath.Join(tempDir, "network.json")
	if err := os.WriteFile(cfgPath, []byte(`{"api_url":"https://saved.api.com","connect_url":"wss://saved.connect.com"}`), 0600); err != nil {
		t.Fatalf("write network.json: %v", err)
	}

	gotSaved, err := resolveApiUrl(emptyOpts)
	if err != nil {
		t.Fatalf("resolveApiUrl saved config failed: %v", err)
	}
	if gotSaved != "https://saved.api.com" {
		t.Errorf("got %q, want https://saved.api.com", gotSaved)
	}
}

func TestNetworkGetRankingSync(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/network/ranking" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-jwt" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"network_ranking":{"leaderboard_rank":42,"net_mib_count":1234.56,"leaderboard_public":true}}`)
	}))
	defer server.Close()

	ctx := context.Background()
	strategy := connect.NewClientStrategyWithDefaults(ctx)

	res, err := networkGetRankingSync(ctx, strategy, server.URL, "test-jwt")
	if err != nil {
		t.Fatalf("networkGetRankingSync failed: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("unexpected API error: %v", res.Error)
	}
	if res.NetworkRanking == nil {
		t.Fatal("expected NetworkRanking to be non-nil")
	}
	if res.NetworkRanking.LeaderboardRank != 42 {
		t.Errorf("rank = %d, want 42", res.NetworkRanking.LeaderboardRank)
	}
	if res.NetworkRanking.NetMibCount != 1234.56 {
		t.Errorf("net_mib_count = %f, want 1234.56", res.NetworkRanking.NetMibCount)
	}
	if !res.NetworkRanking.LeaderboardPublic {
		t.Error("expected LeaderboardPublic to be true")
	}
}

func TestReadNetworkJwt_And_LoadClientKey(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", tempDir)

	// Missing JWT should error
	_, err := readNetworkJwt()
	if err == nil {
		t.Fatal("expected error for missing jwt")
	}

	// Write JWT
	jwtPath := filepath.Join(tempDir, "jwt")
	if err := os.WriteFile(jwtPath, []byte("  test-jwt-token \n"), 0600); err != nil {
		t.Fatalf("write jwt: %v", err)
	}
	jwt, err := readNetworkJwt()
	if err != nil {
		t.Fatalf("readNetworkJwt failed: %v", err)
	}
	if jwt != "test-jwt-token" {
		t.Errorf("got %q, want test-jwt-token", jwt)
	}

	// Missing client key should error
	_, err = snLoadClientKey()
	if err == nil {
		t.Fatal("expected error for missing client key")
	}

	// Write valid 32-byte seed
	keyPath := filepath.Join(tempDir, ".provider.key")
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	if err := os.WriteFile(keyPath, seed, 0600); err != nil {
		t.Fatalf("write .provider.key: %v", err)
	}

	key, err := snLoadClientKey()
	if err != nil {
		t.Fatalf("snLoadClientKey failed: %v", err)
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Fatalf("expected private key length %d, got %d", ed25519.PrivateKeySize, len(key))
	}
}
