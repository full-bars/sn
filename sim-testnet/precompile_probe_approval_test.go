// Synthetic signed checkpoints keep preview/apply approval stable while fresh
// custody checks still prevent retirement of a newly funded probe.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Uses the live descriptor constructor, action binder, plan hash and apply guard.
func TestPrecompileProbeApprovalSurvivesAdvancingFinality(t *testing.T) {
	fixture := newArchivedPrecompileProbeSuccessorFixture(t)
	var approved string
	for index, head := range []ChainHead{{Number: 300, Hash: common.Hash{70}.Hex()}, {Number: 301, Hash: common.Hash{71}.Hex()}, {Number: 400, Hash: common.Hash{72}.Hex()}} {
		successor, err := newPrecompileProbeSuccessor(fixture.source, fixture.payloads, fixture.entries, &fixture.plan.PrecompileProbeSuccessor.Evidence, head)
		if err != nil { t.Fatal(err) }
		plan := clonePrecompileProbeSuccessorPlan(t, fixture.plan)
		plan.GeneratedAt = time.Unix(int64(head.Number), 0).UTC().Format(time.RFC3339)
		plan.LiveFacts.FinalizedBlock, plan.LiveFacts.FinalizedBlockHash = head.Number, common.Hash{byte(80+index)}.Hex()
		plan.LiveFacts.EVMFinalizedBlock, plan.LiveFacts.EVMFinalizedBlockHash = head.Number, head.Hash
		if err := rebindPrecompileProbeSuccessor(plan, fixture.source, fixture.payloads, successor); err != nil { t.Fatal(err) }
		plan.PlanHash, err = plan.hash()
		if err != nil { t.Fatal(err) }
		if index == 0 { approved = plan.PlanHash }
		if err := requireApproved(true, approved, plan.PlanHash); err != nil { t.Fatalf("advancing finality changed preview/apply approval: %v", err) }
		encoded, err := json.Marshal(plan)
		if err != nil { t.Fatal(err) }
		persisted, err := persistedSetupPlanHash(encoded, plan.Schema)
		if err != nil || persisted != approved { t.Fatalf("persisted successor approval differs from preview: %s %v", persisted, err) }
		if index > 0 && successor.FinalizedHead != fixture.source.CoordinatorRepairCarry.Result.Result.ObservedHead { t.Fatal("stable successor lost the signed original repair checkpoint") }
	}
}

// A changed result checkpoint needs its original owner signature, not a new head.
func TestPrecompileProbeApprovalRejectsChangedSignedAnchor(t *testing.T) {
	fixture := newArchivedPrecompileProbeSuccessorFixture(t)
	for _, change := range []string{"block", "hash", "signature", "missing carry", "future anchor", "equal foreign head", "nonce", "already adopted"} {
		prior := clonePrecompileProbeSuccessorPlan(t, fixture.source)
		built := *fixture.payloads
		head := ChainHead{Number: 300, Hash: common.Hash{70}.Hex()}
		switch change {
		case "block": prior.CoordinatorRepairCarry.Result.Result.ObservedHead.Number++
		case "hash": prior.CoordinatorRepairCarry.Result.Result.ObservedHead.Hash = common.Hash{99}.Hex()
		case "signature": prior.CoordinatorRepairCarry.Result.Signature = "changed"
		case "missing carry": prior.CoordinatorRepairCarry = nil
		case "future anchor": head.Number = prior.CoordinatorRepairCarry.Result.Result.ObservedHead.Number-1
		case "equal foreign head": head.Number = prior.CoordinatorRepairCarry.Result.Result.ObservedHead.Number
		case "nonce": built.PrecompileProbeNonce++
		case "already adopted": prior.PrecompileProbeSuccessor = fixture.plan.PrecompileProbeSuccessor
		}
		if _, err := newPrecompileProbeSuccessor(prior, &built, fixture.entries, &fixture.plan.PrecompileProbeSuccessor.Evidence, head); err == nil { t.Fatalf("%s escaped signed successor anchor admission", change) }
	}
}

// The anchor remains approved content; only the fresh observation may advance.
func TestPrecompileProbeApprovalHashBindsAnchor(t *testing.T) {
	fixture := newArchivedPrecompileProbeSuccessorFixture(t)
	successor, err := newPrecompileProbeSuccessor(fixture.source, fixture.payloads, fixture.entries, &fixture.plan.PrecompileProbeSuccessor.Evidence, ChainHead{Number: 300, Hash: common.Hash{70}.Hex()})
	if err != nil { t.Fatal(err) }
	plan := clonePrecompileProbeSuccessorPlan(t, fixture.plan)
	if err := rebindPrecompileProbeSuccessor(plan, fixture.source, fixture.payloads, successor); err != nil { t.Fatal(err) }
	approved, err := plan.hash()
	if err != nil { t.Fatal(err) }
	for _, change := range []string{"anchor block", "anchor hash", "original receipt", "journal", "runtime"} {
		changed := clonePrecompileProbeSuccessorPlan(t, plan)
		switch change {
		case "anchor block": changed.PrecompileProbeSuccessor.FinalizedHead.Number++
		case "anchor hash": changed.PrecompileProbeSuccessor.FinalizedHead.Hash = common.Hash{99}.Hex()
		case "original receipt": changed.PrecompileProbeSuccessor.Restore.PostconditionHash = common.Hash{99}.Hex()
		case "journal": changed.PrecompileProbeSuccessor.JournalHash = common.Hash{99}.Hex()
		case "runtime": changed.PrecompileProbeSuccessor.RuntimeHash = common.Hash{99}.Hex()
		}
		current, err := changed.hash()
		if err != nil { t.Fatal(err) }
		if err := requireApproved(true, approved, current); err == nil { t.Fatalf("changed %s was normalized out of successor approval", change) }
		changed.PlanHash = current
		encoded, err := json.Marshal(changed)
		if err != nil { t.Fatal(err) }
		persisted, err := persistedSetupPlanHash(encoded, changed.Schema)
		if err != nil || persisted != current || persisted == approved { t.Fatalf("persisted hash lost changed %s: %v", change, err) }
	}
}

// Current funding controls cover evm and both approved alpha positions.
func TestPrecompileProbeApprovalRechecksCurrentCustody(t *testing.T) {
	fixture := newPrecompileProbeSuccessorFixture(t)
	successor := fixture.plan.PrecompileProbeSuccessor
	current := ChainHead{Number: successor.FinalizedHead.Number+10, Hash: common.Hash{71}.Hex()}
	retired := common.HexToAddress(successor.RetiredProbe)
	sample, err := decodeHex32("fixture sample", successor.Evidence.SampleHotkey)
	if err != nil { t.Fatal(err) }
	move, err := decodeHex32("fixture move", successor.Evidence.MoveHotkey)
	if err != nil { t.Fatal(err) }
	for _, change := range []string{"empty", "current EVM", "current sample", "current move", "anchor EVM", "anchor sample", "anchor move", "current balance error", "current stake error"} {
		balanceReads, stakeReads := map[uint64]int{}, map[uint64]int{}
		balanceAt := func(_ context.Context, address common.Address, block *big.Int) (*big.Int, error) {
			if address != retired || block == nil || !block.IsUint64() { t.Fatal("custody read changed approved retired identity") }
			number := block.Uint64()
			balanceReads[number]++
			if change == "current balance error" && number == current.Number { return nil, errors.New("synthetic current balance refusal") }
			if change == "current EVM" && number == current.Number || change == "anchor EVM" && number == successor.FinalizedHead.Number { return big.NewInt(1), nil }
			return new(big.Int), nil
		}
		stakeAt := func(_ context.Context, block uint64, hotkey, coldkey [32]byte) (uint64, error) {
			if coldkey != ss58Mirror(retired) || hotkey != sample && hotkey != move { t.Fatal("custody read changed an approved stake position") }
			stakeReads[block]++
			if change == "current stake error" && block == current.Number { return 0, errors.New("synthetic current stake refusal") }
			if change == "current sample" && block == current.Number && hotkey == sample || change == "current move" && block == current.Number && hotkey == move || change == "anchor sample" && block == successor.FinalizedHead.Number && hotkey == sample || change == "anchor move" && block == successor.FinalizedHead.Number && hotkey == move { return 1, nil }
			return 0, nil
		}
		err := verifyPrecompileProbeSuccessorUnfunded(t.Context(), successor, current, successor.DeployerNonce, balanceAt, stakeAt)
		if change == "empty" {
			if err != nil || balanceReads[current.Number] != 1 || balanceReads[successor.FinalizedHead.Number] != 1 || stakeReads[current.Number] != 2 || stakeReads[successor.FinalizedHead.Number] != 2 { t.Fatalf("admission lost its current or signed custody checkpoint: %v", err) }
		} else if err == nil { t.Fatalf("%s custody escaped successor admission", change) }
	}
}

// A finalized replacement's existing exact transaction prefix remains resumable.
func TestPrecompileProbeApprovalKeepsConsumedCreateCustodyScope(t *testing.T) {
	fixture := newPrecompileProbeSuccessorFixture(t)
	successor := fixture.plan.PrecompileProbeSuccessor
	current := ChainHead{Number: successor.FinalizedHead.Number+10, Hash: common.Hash{71}.Hex()}
	for _, consumed := range []uint64{1, 2, 6} {
		balanceReads, stakeReads := 0, 0
		balanceAt := func(_ context.Context, _ common.Address, block *big.Int) (*big.Int, error) {
			if block.Uint64() != successor.FinalizedHead.Number { t.Fatal("consumed CREATE unexpectedly repeated initial current custody admission") }
			balanceReads++
			return new(big.Int), nil
		}
		stakeAt := func(_ context.Context, block uint64, _, _ [32]byte) (uint64, error) {
			if block != successor.FinalizedHead.Number { t.Fatal("consumed CREATE changed its historical stake checkpoint") }
			stakeReads++
			return 0, nil
		}
		if err := verifyPrecompileProbeSuccessorUnfunded(t.Context(), successor, current, successor.DeployerNonce+consumed, balanceAt, stakeAt); err != nil || balanceReads != 1 || stakeReads != 2 { t.Fatalf("consumed CREATE custody scope changed: %v", err) }
	}
}
