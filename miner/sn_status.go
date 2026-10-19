package miner

// sn_status.go ports `provider sn-status [--json]` from the 3.23-fix v33
// provider (provider/sn.go snStatusCmd); the sn relay carries the
// import-adapted copy this follows. It prints the node's leaderboard rank,
// bandwidth, coldkey, current epoch and last-finalized payout share, in the
// v33 banner text or as JSON with --json.
//
// The ranking read goes through connect.HttpGetWithStrategy: the pinned
// connect fork dropped the BringYourApi.NetworkGetRankingSync helper the v33
// code used (same as the relay's local adaptation).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/ss58"
)

// SnStatusOutput is the --json document and the source of the banner lines.
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

// networkGetRankingSync fetches the current network's ranking metrics via the
// authenticated GET /network/ranking route.
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
