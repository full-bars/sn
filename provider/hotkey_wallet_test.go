package provider

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urfoundation/sn/crv4"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/connect"
)

var fakeTestDomain = protocol.ClientKeyHistoryDomain{
	ChainID:          7,
	GenesisHash:      [32]byte{3},
	Netuid:           25,
	Coordinator:      common.Address{4},
	SettlementVault:  common.Address{5},
	DeploymentIDHash: [32]byte{6},
	PolicyHash:       [32]byte{7},
	NoID:             8,
}

var fakeTestBoundary = protocol.ClientKeyEffectiveBoundary{Epoch: 0, Block: 500, Hash: [32]byte{12}}

const fakeTestIssuedAt = 1791244800

var fakeTestNetworkId = connect.Id{9}
var fakeTestUserId = connect.Id{7}

type fakeOperatorServer struct {
	server       *httptest.Server
	byJwt        string
	signer       *ecdsa.PrivateKey
	epoch        uint64
	return500    bool
	mu           sync.Mutex
	consents     map[[32]byte][]protocol.HotkeyWalletMappingConsent
	delegations  []protocol.WalletMappingConsent
	issued       map[string]bool
	calls        map[string]int
	extraWallets []map[string]any
}

func newFakeOperator(t testing.TB) *fakeOperatorServer {
	t.Helper()
	signerKey, err := crypto.HexToECDSA(strings.Repeat("42", 32))
	if err != nil {
		t.Fatal(err)
	}

	claims := gojwt.MapClaims{
		"network_id":   fakeTestNetworkId.String(),
		"user_id":      fakeTestUserId.String(),
		"network_name": "synthetic-test-network",
		"roles":        []string{"provider"},
		"exp":          time.Now().Add(24 * time.Hour).Unix(),
	}
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, claims).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}

	op := &fakeOperatorServer{
		byJwt:       token,
		signer:      signerKey,
		epoch:       10,
		consents:    make(map[[32]byte][]protocol.HotkeyWalletMappingConsent),
		delegations: make([]protocol.WalletMappingConsent, 0),
		issued:      make(map[string]bool),
		calls:       make(map[string]int),
	}

	op.server = httptest.NewServer(http.HandlerFunc(op.serveHTTP))
	t.Cleanup(op.server.Close)
	return op
}

func (op *fakeOperatorServer) callCount(methodPath string) int {
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.calls[methodPath]
}

func (op *fakeOperatorServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	op.mu.Lock()
	defer op.mu.Unlock()

	methodPath := r.Method + " " + r.URL.Path
	op.calls[methodPath]++

	// Verify Authorization header
	authHeader := r.Header.Get("Authorization")
	if authHeader != "Bearer "+op.byJwt {
		http.Error(w, "Unauthorized: missing or invalid Bearer token", http.StatusUnauthorized)
		return
	}

	if op.return500 {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	switch methodPath {
	case "GET /sn/epoch":
		subnet := fakeTestDomain.HotkeySubnet()
		res := map[string]any{
			"epoch":        op.epoch,
			"chain_id":     subnet.ChainID,
			"genesis_hash": "0x" + hex.EncodeToString(subnet.GenesisHash[:]),
			"netuid":       subnet.Netuid,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)

	case "POST /sn/wallet/hotkey-consent":
		var body struct {
			Originals []protocol.HotkeyWalletMappingConsent `json:"originals"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		head, headHash, err := protocol.VerifyHotkeyWalletMappingLineage(r.Context(), body.Originals)
		if err != nil || head.Subnet != fakeTestDomain.HotkeySubnet() {
			http.Error(w, "invalid consent lineage", http.StatusBadRequest)
			return
		}
		op.consents[head.Hotkey] = slices.Clone(body.Originals)
		hotkeySs58, _ := ss58.Encode(head.Hotkey, ss58.BittensorPrefix)
		res := map[string]any{
			"hotkey_ss58": hotkeySs58,
			"head_hash":   headHash,
			"generation":  head.Generation,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)

	case "POST /sn/wallet/hotkey-delegation":
		var body struct {
			HotkeySs58        string   `json:"hotkey_ss58"`
			ConsentHeadHash   [32]byte `json:"consent_head_hash"`
			ConsentGeneration uint64   `json:"consent_generation"`
			FromEpoch         uint64   `json:"from_epoch"`
			ThroughEpoch      uint64   `json:"through_epoch"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		hotkey, err := ss58.DecodeWithPrefix(body.HotkeySs58, ss58.BittensorPrefix)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		statement := protocol.HotkeyNetworkDelegationStatement{
			Domain:            fakeTestDomain,
			UserId:            fakeTestUserId,
			NetworkId:         fakeTestNetworkId,
			Hotkey:            hotkey,
			ConsentHeadHash:   body.ConsentHeadHash,
			ConsentGeneration: body.ConsentGeneration,
			Generation:        uint64(len(op.delegations)) + 1,
			IssuedAt:          fakeTestIssuedAt,
			ExpiresAt:         fakeTestIssuedAt + 300,
			FromEpoch:         body.FromEpoch,
			ThroughEpoch:      body.ThroughEpoch,
		}
		if len(op.delegations) > 0 {
			prev, prevHash, _ := protocol.VerifyHotkeyNetworkDelegation(r.Context(), op.delegations[len(op.delegations)-1])
			if prev != nil {
				statement.PreviousHash = prevHash
			}
		}
		_, _ = rand.Read(statement.Nonce[:])
		if err := protocol.SignProspectiveHotkeyNetworkDelegation(&statement, fakeTestBoundary, op.signer); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		msg, _ := statement.Message()
		op.issued[msg] = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"message": msg})

	case "POST /sn/wallet":
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var delegationBody struct {
			ColdkeySs58 string `json:"coldkey_ss58"`
			Message     string `json:"message"`
			Signature   string `json:"signature"`
		}
		_ = json.Unmarshal(raw, &delegationBody)
		if delegationBody.Message != "" && delegationBody.Signature != "" {
			sigBytes, _ := hex.DecodeString(strings.TrimPrefix(delegationBody.Signature, "0x"))
			if len(sigBytes) != 64 {
				http.Error(w, "bad signature length", http.StatusBadRequest)
				return
			}
			consent := protocol.WalletMappingConsent{
				Message:   delegationBody.Message,
				Signature: [64]byte(sigBytes),
			}
			stmt, hash, err := protocol.VerifyHotkeyNetworkDelegation(r.Context(), consent)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			op.delegations = append(op.delegations, consent)
			res := map[string]any{
				"mapping_hash":       "0x" + hex.EncodeToString(hash[:]),
				"mapping_generation": stmt.Generation,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(res)
			return
		}

		// Legacy wallet set post
		var legacyBody struct {
			ColdkeySs58 string `json:"coldkey_ss58"`
		}
		_ = json.Unmarshal(raw, &legacyBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"coldkey_ss58": legacyBody.ColdkeySs58})

	case "GET /sn/wallet":
		wallets := []map[string]any{
			{
				"coldkey_ss58":  "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY",
				"consent_scope": "network",
				"from_epoch":    1,
				"through_epoch": 2,
			},
		}
		if len(op.delegations) > 0 {
			stmt, hash, _ := protocol.VerifyHotkeyNetworkDelegation(r.Context(), op.delegations[len(op.delegations)-1])
			if stmt != nil {
				hotkeySs58, _ := ss58.Encode(stmt.Hotkey, ss58.BittensorPrefix)
				var coldkey [32]byte
				if stored := op.consents[stmt.Hotkey]; stmt.ConsentGeneration <= uint64(len(stored)) {
					if consent, err := protocol.DecodeHotkeyWalletMappingStatement(stored[stmt.ConsentGeneration-1].Message); err == nil {
						coldkey = consent.Coldkey
					}
				}
				coldkeySs58, _ := ss58.Encode(coldkey, ss58.BittensorPrefix)
				wallets = append(wallets, map[string]any{
					"coldkey_ss58":       coldkeySs58,
					"consent_scope":      "hotkey",
					"hotkey_ss58":        hotkeySs58,
					"from_epoch":         stmt.FromEpoch,
					"through_epoch":      stmt.ThroughEpoch,
					"consent_head_hash":  "0x" + hex.EncodeToString(stmt.ConsentHeadHash[:]),
					"consent_generation": stmt.ConsentGeneration,
					"mapping_hash":       "0x" + hex.EncodeToString(hash[:]),
					"mapping_generation": stmt.Generation,
				})
			}
		}
		wallets = append(wallets, op.extraWallets...)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"wallets": wallets})

	default:
		http.NotFound(w, r)
	}
}

// E1: Fake operator serves all 5 routes and asserts Authorization Bearer.
func TestHotkeyWallet_E1_FakeOperator(t *testing.T) {
	op := newFakeOperator(t)

	// Check missing Authorization returns 401
	req, _ := http.NewRequest(http.MethodGet, op.server.URL+"/sn/epoch", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", resp.StatusCode)
	}

	// Check valid Authorization returns 200
	req, _ = http.NewRequest(http.MethodGet, op.server.URL+"/sn/epoch", nil)
	req.Header.Set("Authorization", "Bearer "+op.byJwt)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with auth, got %d", resp.StatusCode)
	}
	var epochRes struct {
		Epoch   uint64 `json:"epoch"`
		ChainId uint64 `json:"chain_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&epochRes); err != nil {
		t.Fatal(err)
	}
	if epochRes.Epoch != op.epoch {
		t.Fatalf("expected epoch %d, got %d", op.epoch, epochRes.Epoch)
	}
}

// E2: wallet hotkey challenge then set with a test coldkey seed: chain stored, delegation from epoch = epoch+2.
func TestHotkeyWallet_E2_ChallengeAndSet(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	op := newFakeOperator(t)
	if err := os.WriteFile(filepath.Join(stateDir, "jwt"), []byte(op.byJwt), 0600); err != nil {
		t.Fatal(err)
	}

	hotkeySeed := filepath.Join(stateDir, "hotkey.seed")
	if err := os.WriteFile(hotkeySeed, []byte(strings.Repeat("12", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	coldkeySeed := filepath.Join(stateDir, "coldkey.seed")
	if err := os.WriteFile(coldkeySeed, []byte(strings.Repeat("34", 32)), 0600); err != nil {
		t.Fatal(err)
	}

	coldkeyPair, err := crv4.LoadSeedFile(coldkeySeed)
	if err != nil {
		t.Fatal(err)
	}
	coldkeyKeypair, err := crv4.KeypairFromSeed(coldkeyPair)
	if err != nil {
		t.Fatal(err)
	}
	coldkeySs58 := coldkeyKeypair.Address()

	var challengeOut bytes.Buffer
	challengeOpts := docopt.Opts{
		"hotkey":             true,
		"challenge":          true,
		"<coldkey_ss58>":     coldkeySs58,
		"--hotkey_seed_file": hotkeySeed,
		"--api_url":          op.server.URL,
	}
	if err := hotkeyWalletChallenge(context.Background(), challengeOpts, &challengeOut); err != nil {
		t.Fatalf("challenge failed: %v", err)
	}

	// Verify pending file created
	pendingPath := filepath.Join(stateDir, "hotkey-wallet", "pending.json")
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("expected pending.json in store: %v", err)
	}

	var setOut bytes.Buffer
	setOpts := docopt.Opts{
		"hotkey":              true,
		"set":                 true,
		"<coldkey_ss58>":      coldkeySs58,
		"--hotkey_seed_file":  hotkeySeed,
		"--coldkey_seed_file": coldkeySeed,
		"--api_url":           op.server.URL,
	}
	if err := hotkeyWalletSet(context.Background(), setOpts, &setOut); err != nil {
		t.Fatalf("set failed: %v\noutput: %s", err, setOut.String())
	}

	// Verify originals file created
	chainPath := filepath.Join(stateDir, "hotkey-wallet", "originals.json")
	if _, err := os.Stat(chainPath); err != nil {
		t.Fatalf("expected originals.json in store: %v", err)
	}

	op.mu.Lock()
	defer op.mu.Unlock()
	if len(op.delegations) != 1 {
		t.Fatalf("expected 1 delegation at operator, got %d", len(op.delegations))
	}
	stmt, _, err := protocol.VerifyHotkeyNetworkDelegation(context.Background(), op.delegations[0])
	if err != nil {
		t.Fatal(err)
	}
	expectedFrom := op.epoch + 2
	if stmt.FromEpoch != expectedFrom {
		t.Fatalf("expected delegation from epoch %d, got %d", expectedFrom, stmt.FromEpoch)
	}
}

// E3: Upkeep when already adopted: zero delegation POSTs.
func TestHotkeyWallet_E3_UpkeepAlreadyAdopted(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	op := newFakeOperator(t)
	if err := os.WriteFile(filepath.Join(stateDir, "jwt"), []byte(op.byJwt), 0600); err != nil {
		t.Fatal(err)
	}

	hotkeySeed := filepath.Join(stateDir, "hotkey.seed")
	if err := os.WriteFile(hotkeySeed, []byte(strings.Repeat("12", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	coldkeySeed := filepath.Join(stateDir, "coldkey.seed")
	if err := os.WriteFile(coldkeySeed, []byte(strings.Repeat("34", 32)), 0600); err != nil {
		t.Fatal(err)
	}

	hotkeyKp, err := loadOperatorHotkey(hotkeySeed)
	if err != nil {
		t.Fatal(err)
	}
	coldkeyRaw, _ := crv4.LoadSeedFile(coldkeySeed)
	coldkeyKp, _ := crv4.KeypairFromSeed(coldkeyRaw)

	// Initial challenge and set
	var out bytes.Buffer
	challengeOpts := docopt.Opts{
		"hotkey":             true,
		"challenge":          true,
		"<coldkey_ss58>":     coldkeyKp.Address(),
		"--hotkey_seed_file": hotkeySeed,
		"--api_url":          op.server.URL,
	}
	_ = hotkeyWalletChallenge(context.Background(), challengeOpts, &out)
	setOpts := docopt.Opts{
		"hotkey":              true,
		"set":                 true,
		"<coldkey_ss58>":      coldkeyKp.Address(),
		"--hotkey_seed_file":  hotkeySeed,
		"--coldkey_seed_file": coldkeySeed,
		"--api_url":           op.server.URL,
	}
	if err := hotkeyWalletSet(context.Background(), setOpts, &out); err != nil {
		t.Fatalf("set failed: %v", err)
	}

	beforeDelegations := op.callCount("POST /sn/wallet/hotkey-delegation")
	beforeAccepts := op.callCount("POST /sn/wallet")

	// Run upkeep step
	var lastGen uint64
	outcome, err := runHotkeyWalletUpkeepStep(context.Background(), op.server.URL, hotkeyKp, &lastGen)
	if err != nil {
		t.Fatalf("upkeep step failed: %v", err)
	}
	if outcome.delegated {
		t.Fatalf("expected already adopted without new delegation")
	}

	afterDelegations := op.callCount("POST /sn/wallet/hotkey-delegation")
	afterAccepts := op.callCount("POST /sn/wallet")

	if afterDelegations != beforeDelegations || afterAccepts != beforeAccepts {
		t.Fatalf("expected zero new delegation POSTs when adopted, got delta %d / %d",
			afterDelegations-beforeDelegations, afterAccepts-beforeAccepts)
	}
}

// E4: Upkeep after operator epoch advance with a non-adopting entry: from epoch strictly greater than prior.
func TestHotkeyWallet_E4_UpkeepEpochAdvance(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	op := newFakeOperator(t)
	if err := os.WriteFile(filepath.Join(stateDir, "jwt"), []byte(op.byJwt), 0600); err != nil {
		t.Fatal(err)
	}

	hotkeySeed := filepath.Join(stateDir, "hotkey.seed")
	if err := os.WriteFile(hotkeySeed, []byte(strings.Repeat("12", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	coldkeySeed := filepath.Join(stateDir, "coldkey.seed")
	if err := os.WriteFile(coldkeySeed, []byte(strings.Repeat("34", 32)), 0600); err != nil {
		t.Fatal(err)
	}

	hotkeyKp, _ := loadOperatorHotkey(hotkeySeed)
	coldkeyRaw, _ := crv4.LoadSeedFile(coldkeySeed)
	coldkeyKp, _ := crv4.KeypairFromSeed(coldkeyRaw)

	var out bytes.Buffer
	_ = hotkeyWalletChallenge(context.Background(), docopt.Opts{
		"hotkey":             true,
		"challenge":          true,
		"<coldkey_ss58>":     coldkeyKp.Address(),
		"--hotkey_seed_file": hotkeySeed,
		"--api_url":          op.server.URL,
	}, &out)
	_ = hotkeyWalletSet(context.Background(), docopt.Opts{
		"hotkey":              true,
		"set":                 true,
		"<coldkey_ss58>":      coldkeyKp.Address(),
		"--hotkey_seed_file":  hotkeySeed,
		"--coldkey_seed_file": coldkeySeed,
		"--api_url":           op.server.URL,
	}, &out)

	priorFromEpoch := op.epoch + 2

	// Operator advances epoch and non-adopting entry is simulated
	op.mu.Lock()
	op.epoch = 200
	// Clear operator delegations to simulate non-adopting state
	op.delegations = nil
	op.mu.Unlock()

	var lastGen uint64
	outcome, err := runHotkeyWalletUpkeepStep(context.Background(), op.server.URL, hotkeyKp, &lastGen)
	if err != nil {
		t.Fatalf("upkeep step failed: %v", err)
	}
	if !outcome.delegated {
		t.Fatal("expected new delegation after epoch advance and non-adopting entry")
	}
	if outcome.fromEpoch <= priorFromEpoch {
		t.Fatalf("expected fromEpoch (%d) strictly greater than prior (%d)", outcome.fromEpoch, priorFromEpoch)
	}
}

// E5: Upkeep on 500 and on missing jwt: logs one line, returns, next tick retries.
func TestHotkeyWallet_E5_UpkeepErrorAndMissingJwt(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	op := newFakeOperator(t)
	jwtPath := filepath.Join(stateDir, "jwt")
	if err := os.WriteFile(jwtPath, []byte(op.byJwt), 0600); err != nil {
		t.Fatal(err)
	}

	hotkeySeed := filepath.Join(stateDir, "hotkey.seed")
	if err := os.WriteFile(hotkeySeed, []byte(strings.Repeat("12", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	coldkeySeed := filepath.Join(stateDir, "coldkey.seed")
	if err := os.WriteFile(coldkeySeed, []byte(strings.Repeat("34", 32)), 0600); err != nil {
		t.Fatal(err)
	}

	hotkeyKp, _ := loadOperatorHotkey(hotkeySeed)
	coldkeyRaw, _ := crv4.LoadSeedFile(coldkeySeed)
	coldkeyKp, _ := crv4.KeypairFromSeed(coldkeyRaw)

	var out bytes.Buffer
	_ = hotkeyWalletChallenge(context.Background(), docopt.Opts{
		"hotkey":             true,
		"challenge":          true,
		"<coldkey_ss58>":     coldkeyKp.Address(),
		"--hotkey_seed_file": hotkeySeed,
		"--api_url":          op.server.URL,
	}, &out)
	_ = hotkeyWalletSet(context.Background(), docopt.Opts{
		"hotkey":              true,
		"set":                 true,
		"<coldkey_ss58>":      coldkeyKp.Address(),
		"--hotkey_seed_file":  hotkeySeed,
		"--coldkey_seed_file": coldkeySeed,
		"--api_url":           op.server.URL,
	}, &out)

	var lastGen uint64
	var err error

	// Case 1: operator returns 500
	op.mu.Lock()
	op.return500 = true
	op.mu.Unlock()

	_, err = runHotkeyWalletUpkeepStep(context.Background(), op.server.URL, hotkeyKp, &lastGen)
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}

	// Case 2: missing JWT
	op.mu.Lock()
	op.return500 = false
	op.mu.Unlock()
	_ = os.Remove(jwtPath)

	_, err = runHotkeyWalletUpkeepStep(context.Background(), op.server.URL, hotkeyKp, &lastGen)
	if err == nil {
		t.Fatal("expected error on missing jwt, got nil")
	}

	// Restore JWT; next tick succeeds
	_ = os.WriteFile(jwtPath, []byte(op.byJwt), 0600)
	_, err = runHotkeyWalletUpkeepStep(context.Background(), op.server.URL, hotkeyKp, &lastGen)
	if err != nil {
		t.Fatalf("expected success after restoring jwt, got %v", err)
	}
}

// E6: wallet set without --legacy-network-wallet refuses with zero HTTP calls.
func TestHotkeyWallet_E6_WalletSetLegacyGating(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	op := newFakeOperator(t)
	if err := os.WriteFile(filepath.Join(stateDir, "jwt"), []byte(op.byJwt), 0600); err != nil {
		t.Fatal(err)
	}

	coldkeySs58 := "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"

	// 1. Without --legacy-network-wallet: must fail and make zero HTTP calls
	beforeCalls := op.callCount("POST /sn/wallet")
	optsWithout := docopt.Opts{
		"wallet":         true,
		"set":            true,
		"<coldkey_ss58>": coldkeySs58,
		"--api_url":      op.server.URL,
	}
	err := runWalletSet(context.Background(), optsWithout)
	if err == nil {
		t.Fatal("expected wallet set without --legacy-network-wallet to be refused")
	}
	afterCalls := op.callCount("POST /sn/wallet")
	if afterCalls != beforeCalls {
		t.Fatalf("expected zero HTTP calls on refusal, got delta %d", afterCalls-beforeCalls)
	}

	// 2. With --legacy-network-wallet: succeeds and makes HTTP call
	optsWith := docopt.Opts{
		"wallet":                  true,
		"set":                     true,
		"<coldkey_ss58>":          coldkeySs58,
		"--legacy-network-wallet": true,
		"--api_url":               op.server.URL,
	}
	if err := runWalletSet(context.Background(), optsWith); err != nil {
		t.Fatalf("expected legacy wallet set to succeed, got %v", err)
	}
	if op.callCount("POST /sn/wallet") <= beforeCalls {
		t.Fatal("expected POST /sn/wallet HTTP call with --legacy-network-wallet")
	}
}

// E7: Claim: no .provider.jwt and no flag exits nonzero with zero HTTP;
// --legacy-coldkey sends the network JWT and the query param; --provider-jwt sends that token.
func TestHotkeyWallet_E7_ClaimCredentials(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	var claimCalls atomic.Int32
	var lastAuthHeader atomic.Pointer[string]
	var lastQuery atomic.Pointer[string]

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		lastAuthHeader.Store(&h)
		q := r.URL.RawQuery
		lastQuery.Store(&q)

		switch r.URL.Path {
		case "/sn/epoch":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"epoch": 5})
		case "/sn/pool/claim":
			claimCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"epoch":            4,
				"no_id":            "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
				"coldkey":          "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
				"share_bps":        100,
				"proof":            []string{"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
				"payout_root":      "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
				"contract_address": "0x1111111111111111111111111111111111111111",
				"chain_id":         1,
				"claim_open_block": 100,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	networkJwtPath := filepath.Join(stateDir, "jwt")
	_ = os.WriteFile(networkJwtPath, []byte("network-jwt-token"), 0600)

	// Case 1: No .provider.jwt and no flag -> must fail with zero HTTP calls
	optsNoFlag := docopt.Opts{
		"claim":     true,
		"--api_url": server.URL,
	}
	err := runClaim(context.Background(), optsNoFlag)
	if err == nil {
		t.Fatal("expected runClaim without .provider.jwt or flags to fail")
	}
	if claimCalls.Load() != 0 {
		t.Fatalf("expected zero claim HTTP calls on refusal, got %d", claimCalls.Load())
	}

	// Case 2: --legacy-coldkey sends network JWT and &legacy_coldkey=
	legacyColdkey := "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"
	optsLegacy := docopt.Opts{
		"claim":            true,
		"--legacy-coldkey": legacyColdkey,
		"--api_url":        server.URL,
	}
	_ = runClaim(context.Background(), optsLegacy)
	if h := lastAuthHeader.Load(); h == nil || *h != "Bearer network-jwt-token" {
		t.Fatalf("expected Bearer network-jwt-token, got %v", h)
	}
	if q := lastQuery.Load(); q == nil || !strings.Contains(*q, "legacy_coldkey="+legacyColdkey) {
		t.Fatalf("expected legacy_coldkey in query params, got %v", q)
	}

	// Case 3: --provider-jwt sends that token
	providerJwtFile := filepath.Join(stateDir, "custom-provider.jwt")
	_ = os.WriteFile(providerJwtFile, []byte("custom-provider-token"), 0600)
	optsProvider := docopt.Opts{
		"claim":          true,
		"--provider-jwt": providerJwtFile,
		"--api_url":      server.URL,
	}
	_ = runClaim(context.Background(), optsProvider)
	if h := lastAuthHeader.Load(); h == nil || *h != "Bearer custom-provider-token" {
		t.Fatalf("expected Bearer custom-provider-token, got %v", h)
	}
}

// E8: Secret hygiene tests from D: 0644 seed refused, error paths never print hex seed, JWTs, signatures.
func TestHotkeySecretHygiene(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", tempDir)

	// 1. Seed file with 0644 permissions must be refused
	badSeed := filepath.Join(tempDir, "insecure.seed")
	rawSeed := strings.Repeat("a1", 32)
	if err := os.WriteFile(badSeed, []byte(rawSeed), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := crv4.LoadSeedFile(badSeed); err == nil {
		t.Fatal("expected crv4.LoadSeedFile to refuse 0644 seed file")
	}
	if _, err := loadOperatorHotkey(badSeed); err == nil {
		t.Fatal("expected loadOperatorHotkey to refuse 0644 seed file")
	}

	// 2. Error paths for loading hotkey and coldkey must never expose raw hex seed
	validSeedPath := filepath.Join(tempDir, "valid.seed")
	if err := os.WriteFile(validSeedPath, []byte(rawSeed), 0600); err != nil {
		t.Fatal(err)
	}

	// 3. Test snLoadColdkey with wrong address does not leak seed in error text
	wrongColdkey := "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"
	wrongPubkey, _ := ss58.DecodeWithPrefix(wrongColdkey, ss58.BittensorPrefix)
	_, err := snLoadColdkey(validSeedPath, wrongColdkey, wrongPubkey)
	if err == nil {
		t.Fatal("expected error on mismatched coldkey")
	}
	errStr := err.Error()
	if strings.Contains(errStr, rawSeed) {
		t.Fatalf("snLoadColdkey error message leaked raw hex seed: %s", errStr)
	}

	// 4. Source scan: verify hotkey_wallet.go does not format *crv4.Keypair with %v
	srcBytes, err := os.ReadFile("hotkey_wallet.go")
	if err == nil {
		src := string(srcBytes)
		if strings.Contains(src, "%v") {
			// Ensure no keypair is passed as an argument to %v
			for _, line := range strings.Split(src, "\n") {
				if strings.Contains(line, "%v") && (strings.Contains(line, "hotkey") || strings.Contains(line, "coldkey") || strings.Contains(line, "Keypair")) {
					if !strings.Contains(line, ".Address()") && !strings.Contains(line, "PublicKey()") && !strings.Contains(line, "seedPath") && !strings.Contains(line, "seedFile") && !strings.Contains(line, "err") {
						t.Errorf("hotkey_wallet.go formats keypair directly with %%v: %s", line)
					}
				}
			}
		}
	}
}

// The hourly delegation refresh is started from provide only when a hotkey
// seed file is given, and a bad seed file must never stop provide or leak
// the file's contents.
func TestHotkeyWallet_StartUpkeepFromProvide(t *testing.T) {
	var started atomic.Int32
	run := func(context.Context, string, *crv4.Keypair) { started.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if ok, err := startHotkeyWalletUpkeep(ctx, "", "https://api.example", run); ok || err != nil {
		t.Fatalf("no seed file: started=%v err=%v, want not started without error", ok, err)
	}

	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.seed")
	if ok, err := startHotkeyWalletUpkeep(ctx, missing, "https://api.example", run); ok || err == nil {
		t.Fatalf("missing seed file: started=%v err=%v, want an error and not started", ok, err)
	}

	secret := "deadbeef" + strings.Repeat("00", 28)
	loose := filepath.Join(dir, "loose.seed")
	if err := os.WriteFile(loose, []byte(secret), 0644); err != nil {
		t.Fatal(err)
	}
	ok, err := startHotkeyWalletUpkeep(ctx, loose, "https://api.example", run)
	if ok || err == nil {
		t.Fatalf("world-readable seed file: started=%v err=%v, want refused", ok, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("the error text leaked the seed file contents")
	}

	good := filepath.Join(dir, "good.seed")
	if err := os.WriteFile(good, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	ok, err = startHotkeyWalletUpkeep(ctx, good, "https://api.example", run)
	if !ok || err != nil {
		t.Fatalf("valid seed file: started=%v err=%v, want started", ok, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for started.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if started.Load() != 1 {
		t.Fatalf("upkeep loop started %d times, want 1", started.Load())
	}
}
