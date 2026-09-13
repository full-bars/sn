package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type reserveRepairSuccessionFixture struct {
	cfg     *ResolvedConfig
	prior   *SetupPlan
	current SetupFacts
	entries []JournalEntry
	state   string
	repair  Action
}

// Use the real revision renderer, authenticated journal and durable plan
// archive. Only finalized chain facts are supplied by this deterministic test.
func newReserveRepairSuccessionFixture(t *testing.T) reserveRepairSuccessionFixture {
	t.Helper()
	cfg := testResolvedConfig(t)
	cfg.MaximumAlphaRao = 30_000_000_000_000
	cfg.Config.ValidatorBootstrap.MaximumReserveRepairAlphaRao = 6_000_000_000_000
	var err error
	cfg.ConfigHash, err = releaseConfigHash(cfg.Config, cfg.Public, cfg.Hyperparameters)
	if err != nil {
		t.Fatal(err)
	}
	roles, err := derivePublicRoles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	base, err := buildPlan(cfg, testSetupFacts(), roles, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := applySupersededSpend(base, Spend{AlphaRao: 501_693_556_726}); err != nil {
		t.Fatal(err)
	}
	base.PlanHash, err = base.hash()
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	persistFleetCommitmentRecoveryTestPlan(t, state, base)
	j, err := OpenJournal(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alpha.transfer.validator.1", "alpha.transfer.validator.2", "validator.reserve-majority"} {
		action := actionByID(t, base, id)
		path, err := postconditionRelativePath(base.PlanHash, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := j.Append(JournalEntry{DeploymentID: base.DeploymentID, PlanHash: base.PlanHash, ActionID: id, IntentHash: action.IntentHash, Stage: StageVerified, PostconditionHash: base.PlanHash, PostconditionPath: path}); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := readJournalEntries(state)
	if err != nil {
		t.Fatal(err)
	}
	current := *testSetupFacts()
	current.RegisteredAlphaRao = 30_000_000_000_000
	current.ReserveValidatorAlphaRao, err = strconv.ParseUint(actionByID(t, base, "alpha.transfer.validator.1").Parameters["planned_final_stake_rao"], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaximumAlphaRao = base.MaximumSpend.AlphaRao + base.SupersededSpend.AlphaRao + 3_000_000_000_000
	prior, err := buildPlanRevisionFromFacts(cfg, state, base, &current, entries, time.Unix(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	repair := actionByID(t, prior, "alpha.repair.validator.1.2")
	if repair.Spend.AlphaRao != 3_000_000_000_000 {
		t.Fatalf("fixture repair has unexpected fixed credit: %+v", repair)
	}
	private, err := BuildRoleSecrets(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRunInputs(cfg, state, prior, private); err != nil {
		t.Fatal(err)
	}
	// A later reviewed ceiling permits five trillion rao in total for the
	// unstarted repair, but the old three cannot independently reach 65%.
	cfg.MaximumAlphaRao += 2_000_000_000_000
	current.RegisteredAlphaRao = 32_000_000_000_000
	credit, err := alphaTransferMinimumCreditRao(repair.Spend.AlphaRao)
	if err != nil || alphaShareMeets(current.RegisteredAlphaRao, current.ReserveValidatorAlphaRao+credit, 6_500) {
		t.Fatalf("fixture did not reproduce the insufficient unstarted repair: %v", err)
	}
	return reserveRepairSuccessionFixture{cfg: cfg, prior: prior, current: current, entries: entries, state: state, repair: repair}
}

func (f reserveRepairSuccessionFixture) revise(t *testing.T) *SetupPlan {
	t.Helper()
	revised, err := buildPlanRevisionFromFacts(f.cfg, f.state, f.prior, &f.current, f.entries, time.Unix(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	return revised
}

// On the old source the renderer keeps .2 and appends .3 after it. The .2
// pre-signing check is impossible, so that apparently affordable plan cannot
// execute. The correction must reach the unchanged native share check directly.
func TestPlanRevisionReserveRepairSuccessionMakesReplacementExecutable(t *testing.T) {
	t.Parallel()
	f := newReserveRepairSuccessionFixture(t)
	revised := f.revise(t)
	for _, action := range revised.Actions {
		if action.ID == f.repair.ID {
			t.Fatal("insufficient never-started repair still blocks the revised native chain")
		}
	}
	replacement := actionByID(t, revised, "alpha.repair.validator.1.3")
	if replacement.Spend.AlphaRao != 5_000_000_000_000 || !slices.Equal(replacement.DependsOn, f.repair.DependsOn) {
		t.Fatalf("replacement did not reuse only the unspent reservation and proven predecessor: %+v", replacement)
	}
	if replacement.IntentHash == f.repair.IntentHash || revised.PlanHash == f.prior.PlanHash || !slices.Contains(revised.PriorPlanHashes, f.prior.PlanHash) {
		t.Fatal("replacement approval did not retain and distinguish its original source")
	}
	if !slices.Contains(actionByID(t, revised, "validator.reserve-majority").DependsOn, replacement.ID) {
		t.Fatal("live majority barrier does not depend on the replacement")
	}
	if revised.MaximumSpend.AlphaRao+revised.SupersededSpend.AlphaRao != f.cfg.MaximumAlphaRao || revised.SupersededSpend.AlphaRao != f.prior.SupersededSpend.AlphaRao {
		t.Fatal("replacement changed the cumulative ceiling or retired credited spend")
	}
	var source, destination [32]byte
	source[0], destination[0] = 1, 2
	live := liveAlphaTransferEconomics{
		DefaultMinTransferRao: f.current.DefaultMinTransferRao, AlphaPriceQ9: f.current.AlphaPriceQ9,
		SourcePositionRao: f.current.AlphaAvailableRao, SourceTransferableRao: f.current.AlphaTransferableRao,
		Snapshot: RegisteredAlphaSnapshot{TotalAlphaRao: f.current.RegisteredAlphaRao, ByHotkey: map[[32]byte]uint64{
			source: f.current.AlphaAvailableRao, destination: f.current.ReserveValidatorAlphaRao,
		}},
	}
	if err := validateAlphaTransferAtSnapshot(f.repair, source, destination, live, f.prior.AlphaTransferMarginBPS, 6_500, f.prior.MinimumSourceRemainingRao, true); err == nil || !strings.Contains(err.Error(), "does not retain 6500 bps") {
		t.Fatalf("old repair did not reproduce the real native share refusal: %v", err)
	}
	if err := validateAlphaTransferAtSnapshot(replacement, source, destination, live, f.prior.AlphaTransferMarginBPS, 6_500, f.prior.MinimumSourceRemainingRao, true); err != nil {
		t.Fatalf("replacement cannot pass the real unchanged pre-signing boundary: %v", err)
	}
}

func TestPlanRevisionReserveRepairSuccessionPreservesOriginalBytes(t *testing.T) {
	t.Parallel()
	f := newReserveRepairSuccessionFixture(t)
	read := func(path string) []byte {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	priorPath := filepath.Join(f.state, "plan.json")
	// Noncompact original wire makes accidental regeneration observable.
	pretty, err := json.MarshalIndent(f.prior, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	pretty = append(pretty, '\n')
	if err := atomicWrite(priorPath, pretty, 0o600); err != nil {
		t.Fatal(err)
	}
	archived := filepath.Join(f.state, "plans", stringsTrim0x(f.prior.PlanHash)+".json")
	if err := atomicWrite(archived, pretty, 0o600); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(f.state, "journal.jsonl")
	journalBefore := read(journalPath)
	planBefore, err := json.Marshal(f.prior)
	if err != nil {
		t.Fatal(err)
	}
	revised := f.revise(t)
	planAfter, _ := json.Marshal(f.prior)
	if !bytes.Equal(planBefore, planAfter) || !bytes.Equal(pretty, read(priorPath)) || !bytes.Equal(journalBefore, read(journalPath)) {
		t.Fatal("read-only revision mutated original plan or journal custody")
	}
	private, err := BuildRoleSecrets(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRunInputs(f.cfg, f.state, revised, private); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pretty, read(archived)) || !bytes.Equal(journalBefore, read(journalPath)) {
		t.Fatal("adoption changed original archived wire or journal history")
	}
}

func TestPlanRevisionReserveRepairSuccessionRefusesAnyStartedOrAmbiguousIntent(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"intent", "failed", "broadcast", "included", "finalized", "foreign_plan", "different_intent", "aliased_intent"} {
		t.Run(name, func(t *testing.T) {
			f := newReserveRepairSuccessionFixture(t)
			entry := JournalEntry{DeploymentID: f.prior.DeploymentID, PlanHash: f.prior.PlanHash, ActionID: f.repair.ID, IntentHash: f.repair.IntentHash, Stage: StageIntent}
			switch name {
			case "intent", "failed", "broadcast", "included", "finalized":
				entry.Stage = JournalStage(name)
			case "foreign_plan":
				entry.PlanHash = "0x" + strings.Repeat("9", 64)
			case "different_intent":
				entry.IntentHash = "0x" + strings.Repeat("8", 64)
			case "aliased_intent":
				entry.ActionID = "foreign-action"
			}
			if name == "broadcast" || name == "included" || name == "finalized" {
				entry.Signer, entry.Nonce = f.cfg.WalletPublic, "17"
				entry.TransactionHash = "0x" + strings.Repeat("7", 64)
				entry.BlockNumber, entry.RecoveryBlock = 102, 101
				entry.BlockHash, entry.RecoveryBlockHash = "0x"+strings.Repeat("6", 64), "0x"+strings.Repeat("5", 64)
			}
			j, err := OpenJournal(f.state)
			if err != nil {
				t.Fatal(err)
			}
			if err := j.Append(entry); err != nil {
				t.Fatal(err)
			}
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			entries, err := readJournalEntries(f.state)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := buildPlanRevisionFromFacts(f.cfg, f.state, f.prior, &f.current, entries, time.Unix(4, 0)); err == nil || !strings.Contains(err.Error(), "cannot be replaced") {
				t.Fatalf("started or ambiguous repair did not refuse retirement: %v", err)
			}
		})
	}
}

func TestPlanRevisionReserveRepairSuccessionKeepsCreditedLiabilities(t *testing.T) {
	t.Parallel()
	f := newReserveRepairSuccessionFixture(t)
	path, err := postconditionRelativePath(f.prior.PlanHash, f.repair.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.entries = append(f.entries, JournalEntry{DeploymentID: f.prior.DeploymentID, PlanHash: f.prior.PlanHash, ActionID: f.repair.ID, IntentHash: f.repair.IntentHash, Stage: StageVerified, PostconditionHash: f.prior.PlanHash, PostconditionPath: path})
	credit, err := alphaTransferMinimumCreditRao(f.repair.Spend.AlphaRao)
	if err != nil {
		t.Fatal(err)
	}
	f.current.ReserveValidatorAlphaRao += credit
	f.current.RegisteredAlphaRao = 35_000_000_000_000
	f.cfg.MaximumAlphaRao += 1_000_000_000_000
	revised := f.revise(t)
	retained := actionByID(t, revised, f.repair.ID)
	next := actionByID(t, revised, "alpha.repair.validator.1.3")
	if retained.IntentHash != f.repair.IntentHash || retained.Spend.AlphaRao != f.repair.Spend.AlphaRao || !slices.Equal(next.DependsOn, []string{f.repair.ID}) {
		t.Fatal("credited repair was retired or disconnected from its successor")
	}
	if revised.SupersededSpend.AlphaRao != f.prior.SupersededSpend.AlphaRao || revised.MaximumSpend.AlphaRao+revised.SupersededSpend.AlphaRao != f.cfg.MaximumAlphaRao {
		t.Fatal("credited and superseded liabilities were not each charged once")
	}
}

func TestPlanRevisionReserveRepairSuccessionRefusesInsufficientNonterminalRepair(t *testing.T) {
	t.Parallel()
	f := newReserveRepairSuccessionFixture(t)
	// Reproduce the old renderer's already-approved two-pending-repair chain
	// using its existing append primitive, without the new retirement admission.
	legacy := *f.prior
	legacy.Actions = make([]Action, 0, len(f.prior.Actions))
	for _, action := range f.prior.Actions {
		if action.ID == f.repair.ID {
			continue
		}
		if action.ID == "validator.reserve-majority" {
			action.DependsOn = append([]string(nil), action.DependsOn...)
			for index, dependency := range action.DependsOn {
				if dependency == f.repair.ID {
					action.DependsOn[index] = f.repair.DependsOn[0]
				}
			}
			var err error
			action.IntentHash, err = actionIntentHash(action)
			if err != nil {
				t.Fatal(err)
			}
		}
		legacy.Actions = append(legacy.Actions, action)
	}
	var err error
	legacy.MaximumSpend, err = maximumActionSpend(legacy.Actions)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Limits = configuredPlanLimits(f.cfg)
	legacy.ResolvedInputsHash, err = resolvedInputsHash(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyReserveValidatorMajorityRepair(f.cfg, &legacy, f.prior, &f.current, f.entries, []Action{f.repair}); err != nil {
		t.Fatal(err)
	}
	legacy.PlanHash, err = legacy.hash()
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePlanBudget(&legacy); err != nil {
		t.Fatalf("old pending-chain control is not an authentic plan: %v", err)
	}
	if _, err := buildPlanRevisionFromFacts(f.cfg, f.state, &legacy, &f.current, f.entries, time.Unix(4, 0)); err == nil || !strings.Contains(err.Error(), "has a later repair") {
		t.Fatalf("insufficient nonterminal repair chain was rewritten: %v", err)
	}
}

func TestPlanRevisionReserveRepairSuccessionNeverReusesRetiredSequence(t *testing.T) {
	t.Parallel()
	f := newReserveRepairSuccessionFixture(t)
	first := f.revise(t)
	persistFleetCommitmentRecoveryTestPlan(t, f.state, first)
	f.prior = first
	f.current.RegisteredAlphaRao = 35_000_000_000_000
	f.cfg.MaximumAlphaRao += 1_000_000_000_000
	second := f.revise(t)
	for _, action := range second.Actions {
		if action.ID == "alpha.repair.validator.1.2" || action.ID == "alpha.repair.validator.1.3" {
			t.Fatalf("retired action id was carried or reused: %s", action.ID)
		}
	}
	replacement := actionByID(t, second, "alpha.repair.validator.1.4")
	if replacement.Spend.AlphaRao != 6_000_000_000_000 || !slices.Equal(replacement.DependsOn, f.repair.DependsOn) {
		t.Fatalf("later replacement lost its bounded amount or proven predecessor: %+v", replacement)
	}
}

func TestPlanRevisionReserveRepairSuccessionCannotCreateBudgetOrTranche(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"lifetime", "tranche"} {
		t.Run(name, func(t *testing.T) {
			f := newReserveRepairSuccessionFixture(t)
			if name == "lifetime" {
				f.cfg.MaximumAlphaRao -= 2_000_000_000_000
			} else {
				f.cfg.Config.ValidatorBootstrap.MaximumReserveRepairAlphaRao = 3_000_000_000_000
			}
			if _, err := buildPlanRevisionFromFacts(f.cfg, f.state, f.prior, &f.current, f.entries, time.Unix(4, 0)); err == nil || !strings.Contains(err.Error(), "repair tranche") {
				t.Fatalf("replacement escaped its existing %s bound: %v", name, err)
			}
		})
	}
}

func TestPlanRevisionReserveRepairSuccessionEmptyChainKeepsOriginalAdmission(t *testing.T) {
	t.Parallel()
	cfg := testResolvedConfig(t)
	roles, err := derivePublicRoles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := buildPlan(cfg, testSetupFacts(), roles, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	current := *testSetupFacts()
	current.RegisteredAlphaRao = 0
	// With no verified bootstrap and no repair chain, succession must remain
	// a no-op. The original final affordability check owns missing live facts.
	_, err = buildPlanRevisionFromFacts(cfg, t.TempDir(), prior, &current, nil, time.Unix(2, 0))
	if err == nil || !strings.Contains(err.Error(), "revised plan is not affordable from current finalized state: current alpha-transfer price or registered-alpha total is unavailable") {
		t.Fatalf("empty repair chain changed the original fact-admission boundary: %v", err)
	}
}

func TestPlanRevisionReserveRepairSuccessionRequiresDurableIntentBeforeDispatch(t *testing.T) {
	t.Parallel()
	f := newReserveRepairSuccessionFixture(t)
	j, err := OpenJournal(f.state)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err := j.file.Close(); err != nil {
		t.Fatal(err)
	}
	j.file, err = os.Open(filepath.Join(f.state, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{cfg: f.cfg, plan: f.prior, journal: j}
	// No native manager exists: reaching dispatch instead of stopping at the
	// failed durable intent append would fail this control immediately.
	if err := e.Execute(context.Background(), f.repair); err == nil || !strings.Contains(err.Error(), "bad file descriptor") {
		t.Fatalf("native execution did not stop at the failed intent write: %v", err)
	}
	if len(j.Entries()) != len(f.entries) {
		t.Fatal("failed intent append changed the authenticated in-memory journal")
	}
}
