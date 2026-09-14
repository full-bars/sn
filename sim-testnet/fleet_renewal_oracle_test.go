package main

import (
	"context"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/stabi"
)

// The retained deployment restored its original oracle at epoch 273. At epoch
// 356 all three address getters still name that oracle: the contract never
// clears the effective schedule. Exercise the same pinned reader used by both
// renewal planning and live identity checks.
func TestFleetRenewalOriginalOracleAcceptsCompletedRestore(t *testing.T) {
	parsed, err := abi.JSON(strings.NewReader(CoordinatorABI))
	if err != nil {
		t.Fatal(err)
	}
	coordinator := stabi.NewSTCoordinator()
	original := common.HexToAddress("0x0422aC5e1EA3997D2A06f8268D4834380609738C")
	outputs := map[string]string{
		"0x" + hex.EncodeToString(coordinator.PackCurrentEpoch()):                 fleetRefreshTestOutput(t, parsed, "currentEpoch", big.NewInt(356)),
		"0x" + hex.EncodeToString(coordinator.PackCommitmentOracle()):             fleetRefreshTestOutput(t, parsed, "commitmentOracle", original),
		"0x" + hex.EncodeToString(coordinator.PackActiveCommitmentOracle()):       fleetRefreshTestOutput(t, parsed, "activeCommitmentOracle", original),
		"0x" + hex.EncodeToString(coordinator.PackPendingCommitmentOracle()):      fleetRefreshTestOutput(t, parsed, "pendingCommitmentOracle", original),
		"0x" + hex.EncodeToString(coordinator.PackPendingCommitmentOracleEpoch()): fleetRefreshTestOutput(t, parsed, "pendingCommitmentOracleEpoch", uint64(273)),
	}
	server, recorder := newFleetRefreshRPCServer(t, outputs)
	defer server.Close()
	state, err := readFleetRefreshOracleStateAt(context.Background(), fleetRefreshTestManager(t, server), common.HexToAddress("0x8e7d2F9A77feC95C7e4875b0Bd858D5DE2B6def8"), coordinator, 8002200)
	if err != nil {
		t.Fatal(err)
	}
	if !fleetRenewalOriginalOracleReady(state, original) {
		t.Fatal("completed original-oracle restore was rejected as a pending reroute")
	}
	requests, sizes, blocks := recorder.snapshot()
	if requests != 1 || len(sizes) != 1 || sizes[0] != 5 || len(blocks) != 5 {
		t.Fatalf("oracle observation was not one complete pinned batch: %d/%v/%v", requests, sizes, blocks)
	}
	for _, block := range blocks {
		if block != "0x"+new(big.Int).SetUint64(8002200).Text(16) {
			t.Fatalf("oracle observation used unexpected block %s", block)
		}
	}
	state.CurrentEpoch = state.PendingEpoch
	if !fleetRenewalOriginalOracleReady(state, original) {
		t.Fatal("original-oracle restore was rejected at its effective epoch")
	}
	state.Pending, state.PendingEpoch = common.Address{}, 0
	if !fleetRenewalOriginalOracleReady(state, original) {
		t.Fatal("original oracle without a schedule was rejected")
	}
}

func TestFleetRenewalOriginalOracleRejectsChangedRouting(t *testing.T) {
	original := common.HexToAddress("0x0422aC5e1EA3997D2A06f8268D4834380609738C")
	foreign := common.HexToAddress("0x2fb7f30faCb6A9c69b864fc8050bE8bE7eCAf2Cd")
	base := fleetRefreshOracleState{CurrentEpoch: 356, Immutable: original, Active: original, Pending: original, PendingEpoch: 273}
	for _, test := range []struct {
		name   string
		change func(*fleetRefreshOracleState)
	}{
		{"foreign immutable", func(s *fleetRefreshOracleState) { s.Immutable = foreign }},
		{"foreign active", func(s *fleetRefreshOracleState) { s.Active = foreign }},
		{"future original schedule", func(s *fleetRefreshOracleState) { s.PendingEpoch = 357 }},
		{"future foreign reroute", func(s *fleetRefreshOracleState) { s.Pending, s.PendingEpoch = foreign, 357 }},
		{"effective foreign reroute", func(s *fleetRefreshOracleState) { s.Pending = foreign }},
		{"missing pending epoch", func(s *fleetRefreshOracleState) { s.PendingEpoch = 0 }},
		{"missing pending address", func(s *fleetRefreshOracleState) { s.Pending = common.Address{} }},
		{"missing immutable", func(s *fleetRefreshOracleState) { s.Immutable = common.Address{} }},
		{"missing active", func(s *fleetRefreshOracleState) { s.Active = common.Address{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := base
			test.change(&state)
			if fleetRenewalOriginalOracleReady(state, original) {
				t.Fatal("changed or inconsistent oracle route was accepted")
			}
		})
	}
	if fleetRenewalOriginalOracleReady(base, foreign) {
		t.Fatal("a different retained oracle identity was accepted")
	}
	if fleetRenewalOriginalOracleReady(fleetRefreshOracleState{}, common.Address{}) {
		t.Fatal("an empty oracle identity was accepted")
	}
}
