package urnettools

import (
	"context"
	"fmt"

	"github.com/urnetwork/connect"
)

// NetworkRanking mirrors the fork-only connect.NetworkRanking, absent in v2026.
type NetworkRanking struct {
	NetMibCount       float64 `json:"net_mib_count"`
	LeaderboardRank   int     `json:"leaderboard_rank"`
	LeaderboardPublic bool    `json:"leaderboard_public"`
}

type NetworkRankingError struct {
	Message string `json:"message"`
}

type NetworkRankingResult struct {
	NetworkRanking *NetworkRanking      `json:"network_ranking,omitempty"`
	Error          *NetworkRankingError `json:"error,omitempty"`
}

// rankingAPI adds GET /network/ranking to a v2026 BringYourApi.
type rankingAPI struct {
	*connect.BringYourApi
	ctx      context.Context
	strategy *connect.ClientStrategy
	apiUrl   string
	byJwt    string
}

func (a *rankingAPI) SetByJwt(jwt string) {
	a.byJwt = jwt
	a.BringYourApi.SetByJwt(jwt)
}

func (a *rankingAPI) NetworkGetRankingSync() (*NetworkRankingResult, error) {
	return connect.HttpGetWithStrategy(
		a.ctx,
		a.strategy,
		fmt.Sprintf("%s/network/ranking", a.apiUrl),
		a.byJwt,
		&NetworkRankingResult{},
		connect.NewNoopApiCallback[*NetworkRankingResult](),
	)
}
