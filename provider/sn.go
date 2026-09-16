package provider

// sn.go — subnet (bittensor) bridge, RPC, and proxy management for the provider
// (sn/PLAN.md 7.3): `provider wallet set` registers the claim coldkey
// with the platform (decision D-2), and `provider claim` fetches and
// verifies this network's pool payout claim for an epoch (decision
// D-6). Claim recomputes the merkle leaf and checks the inclusion proof
// with sn/merkle, cross-checks the payout root on-chain via a minimal
// eth_call when --rpc is given, and builds the claimMiner calldata with
// the shared sn/stabi packer. With a --key_file it signs and submits the
// transaction through sn/miner/onchain (go-ethereum); without one it
// prints the ready-to-submit calldata for the offline/air-gapped snclaim
// path. The ABI encoding, keccak, merkle and ss58 all come from the
// shared sn packages — this file owns only the flow and the stdlib
// read-side eth_call transport (sn_rpc.go).
//
// ADAPTATION NOTES (v2026 migration):
// - Ported from main to package provider.
// - connect.* symbols checked against v2026 (/home/klets/h3-workspace/connect/):
//   - connect.BringYourApi, connect.ClientStrategy, connect.SnSetWalletArgs,
//     connect.SnPoolClaimArgs, connect.SnEpochResult, connect.NewEventWithContext,
//     connect.NewClientStrategyWithDefaults are present in v2026.
//   - api.NetworkGetRankingSync / connect.NetworkRankingResult from the fork
//     extension are absent in v2026 upstream connect; replaced with local
//     networkGetRankingSync using connect.HttpGetWithStrategy.
// - Bandwidth counting imported from "github.com/urfoundation/sn/provider/bandwidth".
// - State paths resolved via providerStatePath / providerStateDir honoring
//   the URNETWORK_STATE_DIR environment variable and ~/.urnetwork directory.
// - SNProvider struct and lifecycle added to coordinate proxy registration,
//   bandwidth reporting, contract acquisition/denial counters, and proxy grading.
// - Exported aliases provided for public API surface alongside unexported functions.

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/miner/onchain"
	"github.com/urfoundation/sn/provider/bandwidth"
	"github.com/urfoundation/sn/ss58"
	"github.com/urfoundation/sn/stabi"
)

// DefaultApiUrl is the fallback API endpoint when no flag or config is present.
const DefaultApiUrl = "https://api.bringyour.com"

// stSubnet holds the shared abigen packers/unpackers for STSubnet: the
// noCommit / headBindDigest read calldata and the receipt event decoders.
var stSubnet = stabi.NewSTSubnet()

// providerStateDir returns the absolute path of the provider state
// directory, ~/.urnetwork — the one place `jwt`, `.provider.jwt`,
// `network.json`, `.provider.key` and `.provider.cert` live. Does not
// create it.
func providerStateDir() (string, error) {
	if override := strings.TrimSpace(os.Getenv("URNETWORK_STATE_DIR")); override != "" {
		absolute, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		return filepath.Clean(absolute), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork"), nil
}

// providerStatePath returns the absolute filesystem path of a named
// provider state file under ~/.urnetwork (alongside `jwt`). Does not
// create the directory.
func providerStatePath(name string) (string, error) {
	dir, err := providerStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// readProviderClientKeySeed loads the Ed25519 seed for the provider
// client's long-lived identity key from `~/.urnetwork/.provider.key`.
// Returns (nil, nil) when the file does not exist — a fresh install.
// The file is the raw 32-byte seed; no encoding.
func readProviderClientKeySeed() ([]byte, error) {
	p, err := providerStatePath(".provider.key")
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// networkConfig is the on-disk shape of ~/.urnetwork/network.json.
type networkConfig struct {
	ApiUrl     string `json:"api_url"`
	ConnectUrl string `json:"connect_url"`
}

// networkConfigPath returns the absolute path of the saved network config.
func networkConfigPath() (string, error) {
	return providerStatePath("network.json")
}

// readNetworkConfig loads the saved network config. ok is false when the
// file does not exist.
func readNetworkConfig() (cfg networkConfig, ok bool, err error) {
	p, err := networkConfigPath()
	if err != nil {
		return networkConfig{}, false, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return networkConfig{}, false, nil
	}
	if err != nil {
		return networkConfig{}, false, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return networkConfig{}, false, fmt.Errorf("parse %s: %w", p, err)
	}
	return cfg, true, nil
}

// validateApiUrl requires an http or https URL.
func validateApiUrl(rawUrl string) error {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return fmt.Errorf("invalid api_url %q: %w", rawUrl, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid api_url %q: scheme must be http or https, got %q", rawUrl, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid api_url %q: missing host", rawUrl)
	}
	return nil
}

// resolveApiUrl implements the 3-tier precedence for the API URL:
// --api_url flag > saved network config > DefaultApiUrl.
func resolveApiUrl(opts docopt.Opts) (string, error) {
	if apiUrl, err := opts.String("--api_url"); err == nil {
		return apiUrl, nil
	}
	cfg, ok, err := readNetworkConfig()
	if err != nil {
		return "", err
	}
	if ok && cfg.ApiUrl != "" {
		return cfg.ApiUrl, nil
	}
	return DefaultApiUrl, nil
}

// readNetworkJwt loads the network jwt written by `provider auth` from
// ~/.urnetwork/jwt — the same credential provideAuth uses.
func readNetworkJwt() (string, error) {
	jwtPath, err := providerStatePath("jwt")
	if err != nil {
		return "", err
	}
	byJwtBytes, err := os.ReadFile(jwtPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("jwt does not exist at %s. Run `provider auth` first", jwtPath)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(byJwtBytes)), nil
}

// ReadNetworkJwt is the exported version of readNetworkJwt.
func ReadNetworkJwt() (string, error) {
	return readNetworkJwt()
}

// ---------------------------------------------------------------------
// Network Ranking v2026 Local Adaptation
// ---------------------------------------------------------------------

// NetworkRanking holds leaderboard and bandwidth stats for a network.
type NetworkRanking struct {
	NetMibCount       float64 `json:"net_mib_count"`
	LeaderboardRank   int     `json:"leaderboard_rank"`
	LeaderboardPublic bool    `json:"leaderboard_public"`
}

// NetworkRankingError represents an error response from the ranking API.
type NetworkRankingError struct {
	Message string `json:"message"`
}

// NetworkRankingResult is the response shape for GET /network/ranking.
type NetworkRankingResult struct {
	NetworkRanking *NetworkRanking      `json:"network_ranking,omitempty"`
	Error          *NetworkRankingError `json:"error,omitempty"`
}

// networkGetRankingSync fetches the current network's ranking metrics via
// the authenticated GET /network/ranking route using connect.HttpGetWithStrategy.
// Replaces connect.BringYourApi.NetworkGetRankingSync which is absent in upstream v2026 connect.
func networkGetRankingSync(ctx context.Context, clientStrategy *connect.ClientStrategy, apiUrl string, byJwt string) (*NetworkRankingResult, error) {
	return connect.HttpGetWithStrategy(
		ctx,
		clientStrategy,
		fmt.Sprintf("%s/network/ranking", apiUrl),
		byJwt,
		&NetworkRankingResult{},
		connect.NewNoopApiCallback[*NetworkRankingResult](),
	)
}

// NetworkGetRankingSync is the exported version of networkGetRankingSync.
func NetworkGetRankingSync(ctx context.Context, clientStrategy *connect.ClientStrategy, apiUrl string, byJwt string) (*NetworkRankingResult, error) {
	return networkGetRankingSync(ctx, clientStrategy, apiUrl, byJwt)
}

// ---------------------------------------------------------------------
// SNProvider Struct & Lifecycle Management
// ---------------------------------------------------------------------

// SNProxyEntry tracks a registered proxy within the SN bridge.
type SNProxyEntry struct {
	Index        int                       `json:"index"`
	Address      string                    `json:"address"`
	Bandwidth    *bandwidth.ProxyBandwidth `json:"-"`
	RegisteredAt time.Time                 `json:"registered_at"`
	Grade        string                    `json:"grade"`
	Active       bool                      `json:"active"`
}

// SNProviderConfig configures an SNProvider instance.
type SNProviderConfig struct {
	ApiUrl         string
	ClientStrategy *connect.ClientStrategy
	BandwidthReg   *bandwidth.ProxyRegistry
}

// SNProvider coordinates Subnet 25 bridge operations, proxy telemetry,
// bandwidth reporting, contract acquisition/denial counters, and proxy grading.
type SNProvider struct {
	mu             sync.RWMutex
	ctx            context.Context
	cancel         context.CancelFunc
	apiUrl         string
	clientStrategy *connect.ClientStrategy
	bandwidthReg   *bandwidth.ProxyRegistry
	proxies        map[string]*SNProxyEntry
	started        bool
}

// NewSNProvider creates a new Subnet bridge provider.
func NewSNProvider(cfg SNProviderConfig) *SNProvider {
	apiUrl := cfg.ApiUrl
	if apiUrl == "" {
		apiUrl = DefaultApiUrl
	}
	reg := cfg.BandwidthReg
	if reg == nil {
		reg = bandwidth.NewRegistry()
	}
	return &SNProvider{
		apiUrl:         apiUrl,
		clientStrategy: cfg.ClientStrategy,
		bandwidthReg:   reg,
		proxies:        make(map[string]*SNProxyEntry),
	}
}

// Start activates the SNProvider lifecycle.
func (p *SNProvider) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return nil
	}
	p.ctx, p.cancel = context.WithCancel(ctx)
	if p.clientStrategy == nil {
		p.clientStrategy = connect.NewClientStrategyWithDefaults(p.ctx)
	}
	p.started = true
	return nil
}

// Stop gracefully terminates the SNProvider lifecycle.
func (p *SNProvider) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started {
		return nil
	}
	if p.cancel != nil {
		p.cancel()
	}
	p.started = false
	return nil
}

// Running reports whether the SNProvider lifecycle is active.
func (p *SNProvider) Running() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.started
}

// RegisterProxy registers a proxy with bandwidth tracking and lifecycle management.
func (p *SNProvider) RegisterProxy(index int, address string) *SNProxyEntry {
	p.mu.Lock()
	defer p.mu.Unlock()

	bw := p.bandwidthReg.Register(index)
	entry := &SNProxyEntry{
		Index:        index,
		Address:      address,
		Bandwidth:    bw,
		RegisteredAt: time.Now(),
		Grade:        "unrated",
		Active:       true,
	}
	p.proxies[address] = entry
	return entry
}

// UnregisterProxy removes a proxy from the active SNProvider set.
func (p *SNProvider) UnregisterProxy(address string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.proxies[address]; ok {
		entry.Active = false
		delete(p.proxies, address)
	}
}

// GetProxy retrieves a registered proxy entry by address.
func (p *SNProvider) GetProxy(address string) (*SNProxyEntry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry, ok := p.proxies[address]
	return entry, ok
}

// Proxies returns a slice of all currently registered proxy entries.
func (p *SNProvider) Proxies() []*SNProxyEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	res := make([]*SNProxyEntry, 0, len(p.proxies))
	for _, entry := range p.proxies {
		res = append(res, entry)
	}
	return res
}

// ReportBandwidth aggregates and returns bandwidth usage across all registered proxies.
func (p *SNProvider) ReportBandwidth() map[string]*bandwidth.ProxyBandwidth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make(map[string]*bandwidth.ProxyBandwidth, len(p.proxies))
	for addr, entry := range p.proxies {
		result[addr] = entry.Bandwidth
	}
	return result
}

// RecordContractAcquisition records a contract acquisition outcome on the provider node.
func (p *SNProvider) RecordContractAcquisition(proxyAddr string) {
	IncrContractAcquired()
}

// RecordContractDenial records a contract denial outcome on the provider node.
func (p *SNProvider) RecordContractDenial(proxyAddr string) {
	IncrContractDenied()
}

// UpdateProxyGrade records an updated proxy grading evaluation for a proxy.
func (p *SNProvider) UpdateProxyGrade(proxyAddr string, grade string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.proxies[proxyAddr]; ok {
		entry.Grade = grade
	}
}

// ProxyGrade returns the current grade of a proxy.
func (p *SNProvider) ProxyGrade(proxyAddr string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if entry, ok := p.proxies[proxyAddr]; ok {
		return entry.Grade
	}
	return ""
}

// SnapshotProxyHealth returns a live snapshot of proxy statuses and bandwidth.
func (p *SNProvider) SnapshotProxyHealth() (up int, dead []string, degraded []string, bw map[string]*bandwidth.ProxyBandwidth, connecting []string) {
	return ProxyHealthSnapshot()
}

// ---------------------------------------------------------------------
// Subnet Bridge Operations
// ---------------------------------------------------------------------

// snSetWallet validates the ss58 coldkey locally and idempotently sets
// it as the network's subnet claim wallet via the authenticated
// `POST /sn/wallet` route. Prints the result on success.
func snSetWallet(ctx context.Context, clientStrategy *connect.ClientStrategy, apiUrl string, coldkeySs58 string) error {
	pubkey, err := ss58.DecodeWithPrefix(coldkeySs58, ss58.BittensorPrefix)
	if err != nil {
		return fmt.Errorf("invalid ss58 coldkey %q: %s", coldkeySs58, err)
	}
	byJwt, err := readNetworkJwt()
	if err != nil {
		return err
	}
	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)
	result, err := api.SnSetWalletSync(&connect.SnSetWalletArgs{
		ColdkeySs58: coldkeySs58,
	})
	if err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("%s", result.Error.Message)
	}
	fmt.Printf("subnet wallet set to %s (pubkey 0x%x)\n", coldkeySs58, pubkey)
	return nil
}

// SnSetWallet is the exported version of snSetWallet.
func SnSetWallet(ctx context.Context, clientStrategy *connect.ClientStrategy, apiUrl string, coldkeySs58 string) error {
	return snSetWallet(ctx, clientStrategy, apiUrl, coldkeySs58)
}

// SetWallet sets the subnet wallet on the SNProvider instance.
func (p *SNProvider) SetWallet(ctx context.Context, coldkeySs58 string) error {
	p.mu.RLock()
	strategy := p.clientStrategy
	apiUrl := p.apiUrl
	p.mu.RUnlock()
	if strategy == nil {
		strategy = connect.NewClientStrategyWithDefaults(ctx)
	}
	return snSetWallet(ctx, strategy, apiUrl, coldkeySs58)
}

// walletSet implements `provider wallet set <coldkey_ss58>`.
func walletSet(opts docopt.Opts) {
	apiUrl, err := resolveApiUrl(opts)
	if err != nil {
		fmt.Printf("network config error: %s\n", err)
		os.Exit(1)
	}

	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(event.Ctx())
	defer cancel()

	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)

	coldkeySs58, _ := opts.String("<coldkey_ss58>")
	if err := snSetWallet(ctx, clientStrategy, apiUrl, coldkeySs58); err != nil {
		fmt.Printf("subnet wallet not set: %s\n", err)
		os.Exit(1)
	}
}

// WalletSet is the exported entrypoint for `provider wallet set`.
func WalletSet(opts docopt.Opts) {
	walletSet(opts)
}

// claim implements `provider claim [--epoch=<epoch>] [--rpc=<rpc_url>]...
// [--key_file=<key_file>] [--dry-run]`.
//
// Default epoch: the platform reports the current epoch e; the payout
// root for e is only committed and finalized after e ends
// (sn/WHITEPAPER.md 5.2), so the default target is e-1 — the most
// recent epoch that can have a committed root. During the first ~48h of
// e that root may still be inside its dispute window; `claim_open_block`
// in the output says when the claim becomes submittable.
//
// Verification requires the payout roots to agree: the inclusion proof
// walked locally from the recomputed leaf must authenticate the leaf
// against the server-provided root, and (when --rpc is given) that root
// must equal the root read from the contract with eth_call — so a
// verified claim does not rest on trusting the platform (decision D-6).
// Exits nonzero on any mismatch. When a --key_file (and --rpc) is given,
// a verified claim is signed and submitted via sn/miner/onchain;
// otherwise the ready-to-submit calldata is printed for snclaim.
func claim(opts docopt.Opts) {
	apiUrl, err := resolveApiUrl(opts)
	if err != nil {
		fmt.Printf("network config error: %s\n", err)
		os.Exit(1)
	}

	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(event.Ctx())
	defer cancel()

	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)

	dryRun, _ := opts.Bool("--dry-run")
	keyFile, _ := opts.String("--key_file")
	if dryRun && keyFile == "" {
		fmt.Printf("note: --dry-run has no effect without --key_file; claim only verifies\n")
	}

	byJwt, err := readNetworkJwt()
	if err != nil {
		panic(err)
	}
	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)

	var rpcUrls []string
	if rpcAny, ok := opts["--rpc"]; ok && rpcAny != nil {
		rpcUrls = append(rpcUrls, rpcAny.([]string)...)
	}
	// submitting needs an rpc endpoint to broadcast through
	if keyFile != "" && len(rpcUrls) == 0 {
		fmt.Printf("claim: --key_file needs --rpc to submit\n")
		os.Exit(1)
	}

	epoch := uint64(0)
	epochNote := ""
	if epochStr, epochErr := opts.String("--epoch"); epochErr == nil && epochStr != "" {
		epoch, err = strconv.ParseUint(epochStr, 10, 64)
		if err != nil {
			panic(fmt.Errorf("bad --epoch %q: %s", epochStr, err))
		}
	} else {
		epochResult, err := api.SnEpochSync()
		if err != nil {
			panic(err)
		}
		if epochResult.Epoch == 0 {
			panic(fmt.Errorf("current epoch is 0; no finalized epoch to claim yet"))
		}
		epoch = epochResult.Epoch - 1
		epochNote = fmt.Sprintf(" (last finalized; current epoch is %d. Use --epoch to override)", epochResult.Epoch)
	}

	poolClaim, err := api.SnPoolClaimSync(&connect.SnPoolClaimArgs{
		Epoch: epoch,
	})
	if err != nil {
		panic(err)
	}

	// decode and sanity-check the claim fields
	if len(poolClaim.NoId) == 0 {
		panic(fmt.Errorf("claim has no no_id"))
	}
	if 32 < len(poolClaim.NoId) {
		panic(fmt.Errorf("bad no_id length %d; expected <= 32", len(poolClaim.NoId)))
	}
	noId := new(big.Int).SetBytes(poolClaim.NoId)
	if len(poolClaim.Coldkey) != 32 {
		panic(fmt.Errorf("bad coldkey length %d; expected 32", len(poolClaim.Coldkey)))
	}
	var coldkey [32]byte
	copy(coldkey[:], poolClaim.Coldkey)
	if len(poolClaim.PayoutRoot) != 32 {
		panic(fmt.Errorf("bad payout root length %d; expected 32", len(poolClaim.PayoutRoot)))
	}
	var serverRoot [32]byte
	copy(serverRoot[:], poolClaim.PayoutRoot)
	if poolClaim.ShareBps < 0 {
		panic(fmt.Errorf("bad share_bps %d", poolClaim.ShareBps))
	}
	shareBps := uint64(poolClaim.ShareBps)
	shareBpsBig := new(big.Int).SetUint64(shareBps)
	proof := make([][32]byte, len(poolClaim.Proof))
	for i, proofElement := range poolClaim.Proof {
		if len(proofElement) != 32 {
			panic(fmt.Errorf("bad proof element %d length %d; expected 32", i, len(proofElement)))
		}
		copy(proof[i][:], proofElement)
	}

	// recompute the leaf and check the inclusion proof against the server
	// root with sn/merkle — the server root is never trusted blindly
	leaf := merkle.PayoutLeaf(coldkey, shareBpsBig)
	proofVerifiesServer := merkle.Verify(serverRoot, leaf, proof)

	// read the on-chain root, trying each --rpc endpoint in order until one
	// answers both eth_chainId and eth_call. The noCommit read calldata is
	// built with sn/stabi; only the http transport is hand-rolled (sn_rpc.go).
	epochBig := new(big.Int).SetUint64(epoch)
	chainChecked := false
	var chainRoot [32]byte
	var chainId uint64
	var chainRpcUrl string
	if 0 < len(rpcUrls) {
		noCommitCalldata := stSubnet.PackNoCommit(epochBig, noId)
		for _, rpcUrl := range rpcUrls {
			chainIdHex, rpcErr := ethRpcHexResult(ctx, rpcUrl, "eth_chainId", []any{})
			if rpcErr != nil {
				fmt.Printf("rpc %s: %s\n", rpcUrl, rpcErr)
				continue
			}
			rpcChainId, rpcErr := parseEthHexQuantity(chainIdHex)
			if rpcErr != nil {
				fmt.Printf("rpc %s: bad eth_chainId %q\n", rpcUrl, chainIdHex)
				continue
			}
			callHex, rpcErr := ethRpcHexResult(ctx, rpcUrl, "eth_call", []any{
				map[string]any{
					"to":   poolClaim.ContractAddress,
					"data": fmt.Sprintf("0x%x", noCommitCalldata),
				},
				"latest",
			})
			if rpcErr != nil {
				fmt.Printf("rpc %s: %s\n", rpcUrl, rpcErr)
				continue
			}
			returnData, rpcErr := parseEthHexBytes(callHex)
			if rpcErr != nil || len(returnData) < 32 {
				fmt.Printf("rpc %s: noCommit returned %d bytes; expected >= 32 (wrong contract address?)\n", rpcUrl, len(returnData))
				continue
			}
			// noCommit returns (bytes32 payoutRoot, bytes off); the first
			// return word is the committed payout root for the pool.
			copy(chainRoot[:], returnData[:32])
			chainId = rpcChainId
			chainRpcUrl = rpcUrl
			chainChecked = true
			break
		}
		if !chainChecked {
			fmt.Printf("status: UNVERIFIED — no --rpc endpoint answered\n")
			os.Exit(1)
		}
	}

	// all roots must agree: the proof must authenticate the leaf against the
	// server root, and (when --rpc is given) the on-chain root must equal the
	// server root and the chain ids must match
	mismatches := []string{}
	if !proofVerifiesServer {
		mismatches = append(mismatches, "the proof does not verify against the server payout root")
	}
	if chainChecked {
		if chainRoot == ([32]byte{}) {
			mismatches = append(mismatches, "the on-chain payout root is zero (epoch not committed on-chain yet?)")
		} else if chainRoot != serverRoot {
			mismatches = append(mismatches, "the server payout root does not match the on-chain root")
		}
		if chainId != poolClaim.ChainId {
			mismatches = append(mismatches, fmt.Sprintf("chain id mismatch: rpc says %d, server says %d", chainId, poolClaim.ChainId))
		}
	}

	fmt.Printf("epoch: %d%s\n", epoch, epochNote)
	fmt.Printf("no_id: 0x%x\n", poolClaim.NoId)
	fmt.Printf("coldkey: 0x%x\n", coldkey)
	fmt.Printf("share_bps: %d (%.2f%%)\n", shareBps, float64(shareBps)/100.0)
	fmt.Printf("payout_root (server): 0x%x\n", serverRoot)
	if proofVerifiesServer {
		fmt.Printf("payout_root (proof): verifies against the server root (%d-element proof)\n", len(proof))
	} else {
		fmt.Printf("payout_root (proof): DOES NOT verify against the server root (%d-element proof)\n", len(proof))
	}
	if chainChecked {
		fmt.Printf("payout_root (chain): 0x%x (via %s, chain id %d)\n", chainRoot, chainRpcUrl, chainId)
	} else {
		fmt.Printf("payout_root (chain): not checked. Pass --rpc=<rpc_url> to verify against the contract\n")
	}
	fmt.Printf("contract: %s (chain id %d)\n", poolClaim.ContractAddress, poolClaim.ChainId)
	fmt.Printf("claim_open_block: %d\n", poolClaim.ClaimOpenBlock)

	// build the ready-to-submit claimMiner calldata with the shared stabi
	// packer — byte-identical to snclaim's own structured path
	claimCalldata, err := onchain.BuildClaimCalldata(onchain.ClaimIntent{
		E:        epochBig,
		NoID:     noId,
		Coldkey:  coldkey,
		ShareBps: shareBpsBig,
		Proof:    proof,
	})
	if err != nil {
		panic(fmt.Errorf("pack claimMiner: %s", err))
	}

	if 0 < len(mismatches) {
		fmt.Printf("claimMiner calldata:\n0x%x\n", claimCalldata)
		for _, mismatch := range mismatches {
			fmt.Printf("mismatch: %s\n", mismatch)
		}
		fmt.Printf("status: MISMATCH — do not submit\n")
		os.Exit(1)
	}

	// verified. With an EVM key, sign+send through onchain.Submit; otherwise
	// print the calldata for the offline/air-gapped snclaim path.
	if keyFile != "" {
		if !common.IsHexAddress(poolClaim.ContractAddress) {
			fmt.Printf("claim: server contract address %q is not a valid EVM address\n", poolClaim.ContractAddress)
			os.Exit(1)
		}
		contract := common.HexToAddress(poolClaim.ContractAddress)
		key, err := onchain.LoadKeyFile(keyFile)
		if err != nil {
			fmt.Printf("claim: %s\n", err)
			os.Exit(1)
		}
		receipt, err := onchain.Submit(ctx, onchain.SubmitParams{
			Contract: contract,
			Rpcs:     rpcUrls,
			Key:      key,
			Calldata: claimCalldata,
			ChainID:  new(big.Int).SetUint64(poolClaim.ChainId),
			DryRun:   dryRun,
		})
		if err != nil {
			fmt.Printf("claim submit failed: %s\n", err)
			os.Exit(1)
		}
		if receipt == nil {
			return // dry run; onchain.Submit printed the preflight
		}
		printMinerClaimed(receipt, contract)
		return
	}

	fmt.Printf("claimMiner calldata:\n0x%x\n", claimCalldata)
	fmt.Printf("submit with: snclaim submit --rpc=<rpc_url> --contract=%s --calldata=0x%x --key_file=<evm_key_file>\n", poolClaim.ContractAddress, claimCalldata)
	if chainChecked {
		fmt.Printf("status: VERIFIED (proof, server, and on-chain roots agree)\n")
	} else {
		fmt.Printf("status: VERIFIED against the server root only\n")
	}
}

// Claim is the exported entrypoint for `provider claim`.
func Claim(opts docopt.Opts) {
	claim(opts)
}

// printMinerClaimed decodes and prints the MinerClaimed event(s) that the
// contract emitted for this claim receipt.
func printMinerClaimed(receipt *types.Receipt, contract common.Address) {
	decoded := false
	for _, lg := range receipt.Logs {
		if lg.Address != contract {
			continue
		}
		ev, err := stSubnet.UnpackMinerClaimedEvent(lg)
		if err != nil {
			continue
		}
		fmt.Printf("MinerClaimed: epoch %s, noId %s\n", ev.E, ev.NoId)
		fmt.Printf("  coldkey:  0x%x\n", ev.Coldkey)
		fmt.Printf("  shareBps: %s\n", ev.ShareBps)
		fmt.Printf("  paid:     %s rao\n", ev.Amount)
		decoded = true
	}
	if !decoded {
		fmt.Printf("warning: no MinerClaimed event decoded from the receipt\n")
	}
}

// PrintMinerClaimed is the exported version of printMinerClaimed.
func PrintMinerClaimed(receipt *types.Receipt, contract common.Address) {
	printMinerClaimed(receipt, contract)
}

// ---------------------------------------------------------------------
// Head-tier claim — client_id <-> hotkey binding (WHITEPAPER §8.4/§11.4,
// decisions D-6/D-18).
// ---------------------------------------------------------------------

// snBindHeadIntent is the signed-head-binding bundle: everything needed to
// pack and submit bindHead, plus the digest that was signed (for display).
type snBindHeadIntent struct {
	hotkey      [32]byte
	clientId    [32]byte // the provider's client Ed25519 public key (ckey)
	registrant  common.Address
	digest      [32]byte
	clientIdSig []byte // 64-byte Ed25519 signature (R‖S) by clientId over digest
}

// SNBindHeadIntent is the exported version of snBindHeadIntent.
type SNBindHeadIntent = snBindHeadIntent

// snSignBindHead signs the on-chain headBindDigest with the provider's
// client Ed25519 private key (the `.provider.key` identity, the same key
// that produces the `/verify` vpk signatures). ed25519.Sign returns the
// standard 64-byte signature R‖S; the contract splits it r=sig[0:32],
// s=sig[32:64] and verifies via the 0x402 precompile (whose r is "the
// first 32 bytes" and s "the second 32 bytes"), so this byte order maps
// directly with no reordering — exactly as registerValidator's
// ed25519Sig is split (sn/evm/src/STSubnet.sol bindHead).
func snSignBindHead(clientPrivateKey ed25519.PrivateKey, registrant common.Address, hotkey [32]byte, digest [32]byte) *snBindHeadIntent {
	intent := &snBindHeadIntent{
		hotkey:      hotkey,
		registrant:  registrant,
		digest:      digest,
		clientIdSig: ed25519.Sign(clientPrivateKey, digest[:]),
	}
	copy(intent.clientId[:], clientPrivateKey.Public().(ed25519.PublicKey))
	return intent
}

// SNSignBindHead is the exported version of snSignBindHead.
func SNSignBindHead(clientPrivateKey ed25519.PrivateKey, registrant common.Address, hotkey [32]byte, digest [32]byte) *SNBindHeadIntent {
	return snSignBindHead(clientPrivateKey, registrant, hotkey, digest)
}

// snLoadClientKey loads the provider's long-lived Ed25519 identity key
// from ~/.urnetwork/.provider.key (the raw 32-byte seed). This is the
// client_id/ckey used for `/verify`; `provider provide` generates and
// persists it on first run.
func snLoadClientKey() (ed25519.PrivateKey, error) {
	seed, err := readProviderClientKeySeed()
	if err != nil {
		return nil, err
	}
	if len(seed) == 0 {
		p, _ := providerStatePath(".provider.key")
		return nil, fmt.Errorf("provider client key not found at %s. Run `provider provide` once to generate the client identity key", p)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("provider client key seed length %d; expected %d", len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// SNLoadClientKey is the exported version of snLoadClientKey.
func SNLoadClientKey() (ed25519.PrivateKey, error) {
	return snLoadClientKey()
}

// snReadHeadBindDigest reads the exact 32-byte headBindDigest from the
// contract via eth_call, trying each rpc endpoint in order until one
// answers both eth_chainId and eth_call (failover, like `provider
// claim`). Per-endpoint failures are printed; the digest binds
// block.chainid and the contract address internally, so no chain id
// needs to be supplied. The read calldata is packed by the caller with
// sn/stabi (headBindDigest).
func snReadHeadBindDigest(ctx context.Context, rpcUrls []string, contractHex string, calldata []byte) (digest [32]byte, chainId uint64, rpcUrl string, err error) {
	for _, url := range rpcUrls {
		chainIdHex, rpcErr := ethRpcHexResult(ctx, url, "eth_chainId", []any{})
		if rpcErr != nil {
			fmt.Printf("rpc %s: %s\n", url, rpcErr)
			continue
		}
		cid, rpcErr := parseEthHexQuantity(chainIdHex)
		if rpcErr != nil {
			fmt.Printf("rpc %s: bad eth_chainId %q\n", url, chainIdHex)
			continue
		}
		callHex, rpcErr := ethRpcHexResult(ctx, url, "eth_call", []any{
			map[string]any{
				"to":   contractHex,
				"data": fmt.Sprintf("0x%x", calldata),
			},
			"latest",
		})
		if rpcErr != nil {
			fmt.Printf("rpc %s: %s\n", url, rpcErr)
			continue
		}
		returnData, rpcErr := parseEthHexBytes(callHex)
		if rpcErr != nil || len(returnData) < 32 {
			fmt.Printf("rpc %s: headBindDigest returned %d bytes; expected >= 32 (wrong contract address?)\n", url, len(returnData))
			continue
		}
		copy(digest[:], returnData[:32])
		return digest, cid, url, nil
	}
	return digest, 0, "", fmt.Errorf("no --rpc endpoint answered headBindDigest")
}

// SNReadHeadBindDigest is the exported version of snReadHeadBindDigest.
func SNReadHeadBindDigest(ctx context.Context, rpcUrls []string, contractHex string, calldata []byte) (digest [32]byte, chainId uint64, rpcUrl string, err error) {
	return snReadHeadBindDigest(ctx, rpcUrls, contractHex, calldata)
}

// parseBytes32Arg parses a 0x-optional 32-byte hex argument (hotkey or
// client_id).
func parseBytes32Arg(field string, s string) ([32]byte, error) {
	var out [32]byte
	h := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	b, err := hex.DecodeString(h)
	if err != nil {
		return out, fmt.Errorf("%s: %s", field, err)
	}
	if len(b) != 32 {
		return out, fmt.Errorf("%s: %d hex bytes; expected 32", field, len(b))
	}
	copy(out[:], b)
	return out, nil
}

// ParseBytes32Arg is the exported version of parseBytes32Arg.
func ParseBytes32Arg(field string, s string) ([32]byte, error) {
	return parseBytes32Arg(field, s)
}

// parseEvmAddressArg parses a 0x-optional 20-byte hex EVM address.
func parseEvmAddressArg(field string, s string) ([20]byte, error) {
	var out [20]byte
	h := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	b, err := hex.DecodeString(h)
	if err != nil {
		return out, fmt.Errorf("%s: %s", field, err)
	}
	if len(b) != 20 {
		return out, fmt.Errorf("%s: %d hex bytes; expected a 20-byte EVM address", field, len(b))
	}
	copy(out[:], b)
	return out, nil
}

// ParseEvmAddressArg is the exported version of parseEvmAddressArg.
func ParseEvmAddressArg(field string, s string) ([20]byte, error) {
	return parseEvmAddressArg(field, s)
}

// bindHead implements `provider bind-head --hotkey=<hex>
// --registrant=<0xEVMaddr> --contract=<addr> [--rpc=<rpc_url>]...
// [--key_file=<key_file>] [--dry-run]`. It signs the on-chain
// headBindDigest with the provider's client key, packs bindHead with
// sn/stabi, and either submits it via sn/miner/onchain (when --key_file
// is given) or prints the ready-to-submit calldata for snclaim.
func bindHead(opts docopt.Opts) {
	fail := func(err error) {
		fmt.Printf("bind-head failed: %s\n", err)
		os.Exit(1)
	}

	hotkeyStr, _ := opts.String("--hotkey")
	hotkey, err := parseBytes32Arg("--hotkey", hotkeyStr)
	if err != nil {
		fail(err)
	}
	registrantStr, _ := opts.String("--registrant")
	registrantBytes, err := parseEvmAddressArg("--registrant", registrantStr)
	if err != nil {
		fail(err)
	}
	registrant := common.Address(registrantBytes)
	contractStr, _ := opts.String("--contract")
	contractBytes, err := parseEvmAddressArg("--contract", contractStr)
	if err != nil {
		fail(err)
	}
	contract := common.Address(contractBytes)
	var rpcUrls []string
	if rpcAny, ok := opts["--rpc"]; ok && rpcAny != nil {
		rpcUrls = append(rpcUrls, rpcAny.([]string)...)
	}
	if len(rpcUrls) == 0 {
		fail(fmt.Errorf("--rpc: at least one endpoint required to read headBindDigest"))
	}
	dryRun, _ := opts.Bool("--dry-run")
	keyFile, _ := opts.String("--key_file")

	privateKey, err := snLoadClientKey()
	if err != nil {
		fail(err)
	}
	var clientId [32]byte
	copy(clientId[:], privateKey.Public().(ed25519.PublicKey))

	// If we will submit, the EVM key must be the registrant the digest is
	// bound to — catch a mismatch locally before signing/spending gas.
	var key *ecdsa.PrivateKey
	if keyFile != "" {
		key, err = onchain.LoadKeyFile(keyFile)
		if err != nil {
			fail(err)
		}
		if from := crypto.PubkeyToAddress(key.PublicKey); from != registrant {
			fail(fmt.Errorf("--key_file address %s does not equal --registrant %s (the head-bind digest is bound to the registrant)", from.Hex(), registrant.Hex()))
		}
	}

	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(event.Ctx())
	defer cancel()

	contractHex := contract.Hex()
	digestCalldata := stSubnet.PackHeadBindDigest(registrant, hotkey, clientId)
	digest, chainId, rpcUrl, err := snReadHeadBindDigest(ctx, rpcUrls, contractHex, digestCalldata)
	if err != nil {
		fail(err)
	}

	intent := snSignBindHead(privateKey, registrant, hotkey, digest)

	bindCalldata, err := onchain.BuildBindHeadCalldata(intent.hotkey, intent.clientId, intent.clientIdSig)
	if err != nil {
		fail(fmt.Errorf("pack bindHead: %s", err))
	}

	fmt.Printf("head binding intent (bindHead)\n")
	fmt.Printf("hotkey: 0x%x\n", intent.hotkey)
	fmt.Printf("client_id (ckey): 0x%x\n", intent.clientId)
	fmt.Printf("client_id_sig: 0x%x (64-byte Ed25519 R‖S by client_id over the digest)\n", intent.clientIdSig)
	fmt.Printf("digest: 0x%x (headBindDigest, via %s, chain id %d)\n", intent.digest, rpcUrl, chainId)
	fmt.Printf("registrant: %s\n", intent.registrant.Hex())
	fmt.Printf("contract: %s (chain id %d)\n", contractHex, chainId)

	if key != nil {
		receipt, err := onchain.Submit(ctx, onchain.SubmitParams{
			Contract: contract,
			Rpcs:     rpcUrls,
			Key:      key,
			Calldata: bindCalldata,
			ChainID:  new(big.Int).SetUint64(chainId),
			DryRun:   dryRun,
		})
		if err != nil {
			fail(err)
		}
		if receipt == nil {
			return // dry run
		}
		printHeadBound(receipt, contract)
		return
	}

	fmt.Printf("note: registrant MUST equal the snclaim EVM sender. The digest is bound to it, and bindHead reverts unless mirror(sender) equals the hotkey's on-chain coldkey (mirror-gated, like registerValidator).\n")
	fmt.Printf("bindHead calldata:\n0x%x\n", bindCalldata)
	fmt.Printf("submit with: snclaim bind-head --hotkey=0x%x --client_id=0x%x --sig=0x%x --contract=%s --rpc=%s --key_file=<evm_key_file>\n",
		intent.hotkey, intent.clientId, intent.clientIdSig, contractHex, rpcUrl)
}

// BindHead is the exported entrypoint for `provider bind-head`.
func BindHead(opts docopt.Opts) {
	bindHead(opts)
}

// printHeadBound decodes and prints the HeadBound event(s) from a bind receipt.
func printHeadBound(receipt *types.Receipt, contract common.Address) {
	decoded := false
	for _, lg := range receipt.Logs {
		if lg.Address != contract {
			continue
		}
		ev, err := stSubnet.UnpackHeadBoundEvent(lg)
		if err != nil {
			continue
		}
		fmt.Printf("HeadBound: uid %d, registrant %s\n", ev.Uid, ev.Registrant.Hex())
		fmt.Printf("  hotkey:    0x%x\n", ev.Hotkey)
		fmt.Printf("  client_id: 0x%x\n", ev.ClientId)
		decoded = true
	}
	if !decoded {
		fmt.Printf("warning: no HeadBound event decoded from the receipt\n")
	}
}

// PrintHeadBound is the exported version of printHeadBound.
func PrintHeadBound(receipt *types.Receipt, contract common.Address) {
	printHeadBound(receipt, contract)
}

// unbindHead implements `provider unbind-head --hotkey=<hex>
// [--contract=<addr>] [--rpc=<rpc_url>]... [--key_file=<key_file>]
// [--dry-run]`. Unbind is mirror-gated only (no client signature), so this
// packs unbindHead with sn/stabi and either submits it via sn/miner/onchain
// (when --key_file is given) or prints the calldata for snclaim.
func unbindHead(opts docopt.Opts) {
	fail := func(err error) {
		fmt.Printf("unbind-head failed: %s\n", err)
		os.Exit(1)
	}

	hotkeyStr, _ := opts.String("--hotkey")
	hotkey, err := parseBytes32Arg("--hotkey", hotkeyStr)
	if err != nil {
		fail(err)
	}
	var rpcUrls []string
	if rpcAny, ok := opts["--rpc"]; ok && rpcAny != nil {
		rpcUrls = append(rpcUrls, rpcAny.([]string)...)
	}
	dryRun, _ := opts.Bool("--dry-run")
	keyFile, _ := opts.String("--key_file")

	// contract is optional for the offline print, required to submit
	var contract common.Address
	haveContract := false
	if contractStr, _ := opts.String("--contract"); strings.TrimSpace(contractStr) != "" {
		contractBytes, err := parseEvmAddressArg("--contract", contractStr)
		if err != nil {
			fail(err)
		}
		contract = common.Address(contractBytes)
		haveContract = true
	}

	calldata, err := onchain.BuildUnbindHeadCalldata(hotkey)
	if err != nil {
		fail(fmt.Errorf("pack unbindHead: %s", err))
	}

	fmt.Printf("head unbind intent (unbindHead)\n")
	fmt.Printf("hotkey: 0x%x\n", hotkey)
	fmt.Printf("note: unbind needs no signature — it is mirror-gated only. The snclaim EVM sender's mirror must equal the hotkey's on-chain coldkey.\n")
	fmt.Printf("unbindHead calldata:\n0x%x\n", calldata)

	if keyFile != "" {
		if !haveContract {
			fail(fmt.Errorf("--contract required to submit"))
		}
		if len(rpcUrls) == 0 {
			fail(fmt.Errorf("--rpc required to submit"))
		}
		key, err := onchain.LoadKeyFile(keyFile)
		if err != nil {
			fail(err)
		}

		event := connect.NewEventWithContext(context.Background())
		event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
		ctx, cancel := context.WithCancel(event.Ctx())
		defer cancel()

		receipt, err := onchain.Submit(ctx, onchain.SubmitParams{
			Contract: contract,
			Rpcs:     rpcUrls,
			Key:      key,
			Calldata: calldata,
			DryRun:   dryRun,
		})
		if err != nil {
			fail(err)
		}
		if receipt == nil {
			return // dry run
		}
		printHeadUnbound(receipt, contract)
		return
	}

	contractHint := "<contract>"
	if haveContract {
		contractHint = contract.Hex()
	}
	fmt.Printf("submit with: snclaim unbind-head --hotkey=0x%x --contract=%s --rpc=<rpc_url> --key_file=<evm_key_file>\n", hotkey, contractHint)
}

// UnbindHead is the exported entrypoint for `provider unbind-head`.
func UnbindHead(opts docopt.Opts) {
	unbindHead(opts)
}

// printHeadUnbound decodes and prints the HeadUnbound event(s) from an unbind receipt.
func printHeadUnbound(receipt *types.Receipt, contract common.Address) {
	decoded := false
	for _, lg := range receipt.Logs {
		if lg.Address != contract {
			continue
		}
		ev, err := stSubnet.UnpackHeadUnboundEvent(lg)
		if err != nil {
			continue
		}
		fmt.Printf("HeadUnbound: uid %d, registrant %s\n", ev.Uid, ev.Registrant.Hex())
		fmt.Printf("  hotkey:    0x%x\n", ev.Hotkey)
		fmt.Printf("  client_id: 0x%x\n", ev.ClientId)
		decoded = true
	}
	if !decoded {
		fmt.Printf("warning: no HeadUnbound event decoded from the receipt\n")
	}
}

// PrintHeadUnbound is the exported version of printHeadUnbound.
func PrintHeadUnbound(receipt *types.Receipt, contract common.Address) {
	printHeadUnbound(receipt, contract)
}

// SnStatusOutput represents the formatted or JSON status output of `provider sn-status`.
type SnStatusOutput struct {
	LeaderboardRank   int     `json:"leaderboard_rank"`
	LeaderboardPublic bool    `json:"leaderboard_public"`
	NetMibCount       float64 `json:"net_mib_count"`
	Top200Eligible    bool    `json:"top200_eligible"`
	TierDescription   string  `json:"tier_description"`
	ColdkeySs58       string  `json:"coldkey_ss58,omitempty"`
	ColdkeyHex        string  `json:"coldkey_hex,omitempty"`
	CurrentEpoch      uint64  `json:"current_epoch,omitempty"`
	StartBlock        uint64  `json:"start_block,omitempty"`
	FinalizeBlock     uint64  `json:"finalize_block,omitempty"`
	ContractAddress   string  `json:"contract_address,omitempty"`
	ChainID           uint64  `json:"chain_id,omitempty"`
	PayoutShareBps    int     `json:"payout_share_bps,omitempty"`
	ClaimEpoch        uint64  `json:"claim_epoch,omitempty"`
	Error             string  `json:"error,omitempty"`
}

// snStatusCmd implements `provider sn-status [--json]`.
func snStatusCmd(opts docopt.Opts) {
	apiUrl, err := resolveApiUrl(opts)
	if err != nil {
		fmt.Printf("network config error: %s\n", err)
		os.Exit(1)
	}
	jsonMode, _ := opts.Bool("--json")

	byJwt, err := readNetworkJwt()
	if err != nil {
		fmt.Printf("authentication error: %s\n", err)
		os.Exit(1)
	}

	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(event.Ctx(), 15*time.Second)
	defer cancel()

	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)
	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)

	out := SnStatusOutput{}
	var errs []string

	rankingRes, err := networkGetRankingSync(ctx, clientStrategy, apiUrl, byJwt)
	if err != nil {
		errs = append(errs, fmt.Sprintf("network ranking: %v", err))
	} else if rankingRes != nil && rankingRes.Error != nil && rankingRes.Error.Message != "" {
		errs = append(errs, fmt.Sprintf("network ranking: %s", rankingRes.Error.Message))
	} else if rankingRes != nil && rankingRes.NetworkRanking != nil {
		out.LeaderboardRank = rankingRes.NetworkRanking.LeaderboardRank
		out.NetMibCount = rankingRes.NetworkRanking.NetMibCount
		out.LeaderboardPublic = rankingRes.NetworkRanking.LeaderboardPublic

		rank := out.LeaderboardRank
		if rank > 0 && rank <= 10 {
			out.Top200Eligible = true
			out.TierDescription = fmt.Sprintf("Tier 1 Elite (Rank #%d Globally)", rank)
		} else if rank > 10 && rank <= 50 {
			out.Top200Eligible = true
			out.TierDescription = fmt.Sprintf("Tier 2 High-Volume (Rank #%d Globally)", rank)
		} else if rank > 50 && rank <= 200 {
			out.Top200Eligible = true
			out.TierDescription = fmt.Sprintf("Tier 3 Active (Rank #%d Globally)", rank)
		} else if rank > 200 {
			out.Top200Eligible = false
			out.TierDescription = fmt.Sprintf("Rank #%d (%d below Top 200 cutoff)", rank, rank-200)
		} else {
			out.TierDescription = "Unranked / Processing"
		}
	}

	epochRes, err := api.SnEpochSync()
	if err != nil {
		errs = append(errs, fmt.Sprintf("subnet epoch: %v", err))
	} else if epochRes != nil {
		out.CurrentEpoch = epochRes.Epoch
		out.StartBlock = epochRes.StartBlock
		out.FinalizeBlock = epochRes.FinalizeBlock
		out.ContractAddress = epochRes.ContractAddress
		out.ChainID = epochRes.ChainId

		if epochRes.Epoch > 0 {
			targetEpoch := epochRes.Epoch - 1
			if claimRes, err := api.SnPoolClaimSync(&connect.SnPoolClaimArgs{Epoch: targetEpoch}); err == nil && claimRes != nil {
				out.ClaimEpoch = claimRes.Epoch
				out.PayoutShareBps = claimRes.ShareBps
				if len(claimRes.Coldkey) == 32 {
					var ck [32]byte
					copy(ck[:], claimRes.Coldkey)
					claimHex := fmt.Sprintf("0x%x", ck)
					if out.ColdkeyHex == "" {
						out.ColdkeyHex = claimHex
					}
					if encoded, err := ss58.Encode(ck, ss58.BittensorPrefix); err == nil && out.ColdkeySs58 == "" {
						out.ColdkeySs58 = encoded
					}
				}
			}
		}
	}

	if len(errs) > 0 {
		out.Error = strings.Join(errs, "; ")
	}

	if jsonMode {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "json encode error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	fmt.Println("\x1b[1m════════════════════════════════════════════════════════════════════════════════\x1b[0m")
	fmt.Println("  \x1b[1;36mURNetwork Subnet 25 — Node & Miner Status\x1b[0m")
	fmt.Println("\x1b[1m════════════════════════════════════════════════════════════════════════════════\x1b[0m")

	if out.LeaderboardRank > 0 {
		color := "\x1b[32m"
		if !out.Top200Eligible {
			color = "\x1b[33m"
		}
		pubBadge := ""
		if out.LeaderboardPublic {
			pubBadge = " [Public]"
		}
		fmt.Printf("  \x1b[1mGlobal Rank:\x1b[0m      %s#%d%s\x1b[0m — %s\n",
			color, out.LeaderboardRank, pubBadge, out.TierDescription)
	} else {
		fmt.Printf("  \x1b[1mGlobal Rank:\x1b[0m      \x1b[90mUnranked or pending initial telemetry\x1b[0m\n")
	}

	if out.NetMibCount > 0 {
		fmt.Printf("  \x1b[1mNet Bandwidth:\x1b[0m    \x1b[1;32m%.2f MiB\x1b[0m (%.2f GiB) provided\n",
			out.NetMibCount, out.NetMibCount/1024.0)
	} else {
		fmt.Printf("  \x1b[1mNet Bandwidth:\x1b[0m    0.00 MiB\n")
	}

	if out.ColdkeySs58 != "" {
		fmt.Printf("  \x1b[1mColdkey (SS58):\x1b[0m   \x1b[33m%s\x1b[0m\n", out.ColdkeySs58)
		if out.ColdkeyHex != "" {
			fmt.Printf("  \x1b[1mColdkey (Hex):\x1b[0m    \x1b[90m%s\x1b[0m\n", out.ColdkeyHex)
		}
	} else if out.ColdkeyHex != "" {
		fmt.Printf("  \x1b[1mColdkey (Hex):\x1b[0m    \x1b[33m%s\x1b[0m\n", out.ColdkeyHex)
	} else {
		fmt.Printf("  \x1b[1mColdkey:\x1b[0m          \x1b[90m(Not configured — register with 'provider wallet set <coldkey>')\x1b[0m\n")
	}

	if out.CurrentEpoch > 0 {
		fmt.Printf("  \x1b[1mSubnet Epoch:\x1b[0m     #%d (Start: #%d, Finalize: #%d)\n",
			out.CurrentEpoch, out.StartBlock, out.FinalizeBlock)
	}
	if out.ContractAddress != "" {
		fmt.Printf("  \x1b[1mContract:\x1b[0m         %s (Chain ID: %d)\n", out.ContractAddress, out.ChainID)
	}
	if out.PayoutShareBps > 0 {
		fmt.Printf("  \x1b[1mPayout Share:\x1b[0m     \x1b[32m%.2f%%\x1b[0m (%d bps in Epoch #%d)\n",
			float64(out.PayoutShareBps)/100.0, out.PayoutShareBps, out.ClaimEpoch)
	}
	if out.Error != "" {
		fmt.Printf("  \x1b[33;1mWarning:\x1b[0m          \x1b[33m%s\x1b[0m\n", out.Error)
	}
	fmt.Println("\x1b[1m════════════════════════════════════════════════════════════════════════════════\x1b[0m")
}

// SnStatusCmd is the exported entrypoint for `provider sn-status`.
func SnStatusCmd(opts docopt.Opts) {
	snStatusCmd(opts)
}
