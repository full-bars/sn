package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The active predecessor differs from the reviewed successor. No RPC reader
// exists: loading must select the exact archived approval after a driver
// hotfix and later head observations, without rebuilding a new plan.
func TestReviewedProvisionalSetupUsesArchivedSnapshotAfterHeadDrift(t *testing.T) {
	cfg, prior, stateDir, options, activeBefore := provisionalRuntimePlanFixture(t)
	reviewed := *prior
	reviewed.PriorPlanHashes = []string{prior.PlanHash}
	var err error
	reviewed.PlanHash, err = reviewed.hash()
	if err != nil {
		t.Fatal(err)
	}
	if reviewed.PlanHash == prior.PlanHash {
		t.Fatal("review fixture did not create a distinct successor approval")
	}
	archived, err := archiveReviewedSetupPlan(stateDir, &reviewed)
	if err != nil {
		t.Fatal(err)
	}
	options.PlanHash = archived.PlanHash
	archivePath := filepath.Join(stateDir, "plans", stringsTrim0x(archived.PlanHash)+".json")
	bytesBefore, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	// Both observation heights are intentionally outside the economic hash,
	// but the first archived observation remains the exact reviewed snapshot.
	reviewed.GeneratedAt = time.Unix(2, 0).UTC().Format(time.RFC3339)
	reviewed.LiveFacts.FinalizedBlock += 100
	reviewed.LiveFacts.EVMFinalizedBlock += 100
	if hash, err := reviewed.hash(); err != nil || hash != options.PlanHash {
		t.Fatal("head-only drift changed economic approval", err)
	}
	again, err := archiveReviewedSetupPlan(stateDir, &reviewed)
	if err != nil || !reflect.DeepEqual(again.LiveFacts, archived.LiveFacts) || again.GeneratedAt != archived.GeneratedAt {
		t.Fatal("later planning replaced the first reviewed snapshot", err)
	}
	loaded, err := loadInvocationPlan(cfg, stateDir, "setup", options)
	if err != nil || loaded.PlanHash != options.PlanHash || !reflect.DeepEqual(loaded.LiveFacts, archived.LiveFacts) || loaded.ReleaseLockHash != prior.ReleaseLockHash {
		t.Fatal("provisional setup regenerated the active predecessor or lost reviewed facts", err)
	}
	activeAfter, err := os.ReadFile(filepath.Join(stateDir, "plan.json"))
	if err != nil || !bytes.Equal(activeBefore, activeAfter) {
		t.Fatal("review replaced the active approval", err)
	}
	bytesAfter, err := os.ReadFile(archivePath)
	if err != nil || !bytes.Equal(bytesBefore, bytesAfter) {
		t.Fatal("reviewed bytes changed after head drift", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "journal.jsonl")); !os.IsNotExist(err) {
		t.Fatal("read-only review opened the deployment journal", err)
	}
	changed := *cfg
	changed.MaximumAlphaRao++
	if _, err := loadInvocationPlan(&changed, stateDir, "setup", options); err == nil {
		t.Fatal("archived approval bypassed the current allowance")
	}
	for _, fault := range []string{"missing", "mismatched-hash", "malformed"} {
		switch fault {
		case "missing":
			if err := os.Remove(archivePath); err != nil {
				t.Fatal(err)
			}
		case "mismatched-hash":
			if err := atomicWrite(archivePath, activeBefore, 0o600); err != nil {
				t.Fatal(err)
			}
		case "malformed":
			if err := atomicWrite(archivePath, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := loadInvocationPlan(cfg, stateDir, "setup", options); err == nil {
			t.Fatal("provisional setup rebuilt or accepted an invalid review archive", fault)
		}
		if err := atomicWrite(archivePath, bytesBefore, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReviewedSetupRetirementRejectsTransactionEvidenceAddedAfterReview(t *testing.T) {
	f := reserveRepairPreSignFixture(t)
	reviewed := f.revise(t)
	if err := validateReviewedSetupRepairRetirements(f.prior, reviewed, f.entries); err != nil {
		t.Fatal(err)
	}
	entries := append([]JournalEntry(nil), f.entries...)
	later := entries[len(entries)-1]
	later.Stage = StageBroadcast
	later.TransactionHash = "synthetic-post-review-transaction"
	entries = append(entries, later)
	if err := validateReviewedSetupRepairRetirements(f.prior, reviewed, entries); err == nil {
		t.Fatal("archived review discarded an action signed after review")
	}
}

// Archiving authenticates exactly the bytes it publishes, newline included.
// Every refusal of the former separate pre-publication decode remains: none
// creates state, and a retained first archive is never replaced. A retained
// archive without its final newline was never refused and still wins.
func TestReviewedSetupArchiveRetainsEveryRefusal(t *testing.T) {
	t.Parallel()
	fixture := newFleetRenewalTestFixture(t)
	plan, err := appendFleetRenewalPlan(fixture.base, fixture.renewal)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	wire := append(append([]byte(nil), raw...), '\n')
	foreign, err := json.Marshal(fixture.base)
	if err != nil {
		t.Fatal(err)
	}
	// Each variant owns its maps and, unless stated, carries its own hash.
	changed := func(mutate func(*SetupPlan)) *SetupPlan {
		var copy SetupPlan
		if err := json.Unmarshal(raw, &copy); err != nil {
			t.Fatal(err)
		}
		mutate(&copy)
		hash, err := copy.hash()
		if err != nil {
			t.Fatal(err)
		}
		copy.PlanHash = hash
		return &copy
	}
	stale := changed(func(p *SetupPlan) { p.Limits.TAORao++ })
	stale.PlanHash = plan.PlanHash
	forged := changed(func(p *SetupPlan) {
		members := p.FleetRenewals[0].Fleets[0].Members
		members[0].Binding.HotkeySignature = members[1].Binding.HotkeySignature
	})
	outOfOrder := changed(func(p *SetupPlan) { p.FleetRenewals[0].Round = 2 })
	reordered := changed(func(p *SetupPlan) {
		fleets := p.FleetRenewals[0].Fleets
		fleets[0], fleets[1] = fleets[1], fleets[0]
	})
	overflow := changed(func(p *SetupPlan) {
		total, err := addSpends(p.MaximumSpend, p.SupersededSpend)
		if err != nil || total.TAORao == 0 {
			t.Fatal("renewal fixture has no TAO spend to overflow", err)
		}
		p.Limits.TAORao = total.TAORao - 1
	})
	for _, refusal := range []struct {
		name string
		plan *SetupPlan
		want string
	}{
		{"absent", nil, "reviewed setup plan identity is unavailable"},
		{"unnamed", &SetupPlan{}, "reviewed setup plan identity is unavailable"},
		{"stale-hash", stale, "persisted setup plan hash mismatch"},
		{"forged-round", forged, "renewal binding signatures or generation are invalid"},
		{"out-of-order-round", outOfOrder, "fleet renewal round has no immutable predecessor plan"},
		{"reordered-fleets", reordered, "renewal fleet 2 has invalid identity, members, or UID"},
		{"budget-overflow", overflow, "TAO plan maximum"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			if _, err := archiveReviewedSetupPlan(stateDir, refusal.plan); err == nil || !strings.Contains(err.Error(), refusal.want) {
				t.Fatalf("archive error %v, want %q", err, refusal.want)
			}
			if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
				t.Fatal("refused approval created archive state", err)
			}
		})
	}
	// Hash-excluded review observations can still make a retained archive
	// invalid; only its own full decode can refuse it. A valid retained
	// archive is returned with its own observation, the first bytes winning.
	inconsistent := changed(func(p *SetupPlan) { p.LiveFacts.AlphaTransferableRao++ })
	firstReview := changed(func(p *SetupPlan) { p.GeneratedAt = time.Unix(2, 0).UTC().Format(time.RFC3339) })
	if inconsistent.PlanHash != plan.PlanHash || firstReview.PlanHash != plan.PlanHash || firstReview.GeneratedAt == plan.GeneratedAt {
		t.Fatal("review observation entered the economic approval hash")
	}
	observed, err := json.Marshal(inconsistent)
	if err != nil {
		t.Fatal(err)
	}
	unterminated, err := json.Marshal(firstReview)
	if err != nil {
		t.Fatal(err)
	}
	for _, retained := range []struct {
		name      string
		bytes     []byte
		want      string
		generated string
	}{
		{"malformed", []byte("{}\n"), "existing reviewed setup archive identity differs", ""},
		{"foreign", append(foreign, '\n'), "existing reviewed setup archive identity differs", ""},
		{"inconsistent-observation", append(observed, '\n'), "alpha source capacity is inconsistent", ""},
		{"without-final-newline", unterminated, "", firstReview.GeneratedAt},
		{"fresh", nil, "", plan.GeneratedAt},
	} {
		t.Run(retained.name, func(t *testing.T) {
			stateDir := filepath.Join(t.TempDir(), "state")
			path := filepath.Join(stateDir, "plans", stringsTrim0x(plan.PlanHash)+".json")
			want := wire
			if retained.bytes != nil {
				if err := atomicWrite(path, retained.bytes, 0o600); err != nil {
					t.Fatal(err)
				}
				want = retained.bytes
			}
			archived, err := archiveReviewedSetupPlan(stateDir, plan)
			if retained.want != "" {
				if err == nil || !strings.Contains(err.Error(), retained.want) {
					t.Fatalf("archive error %v, want %q", err, retained.want)
				}
			} else if err != nil || archived.PlanHash != plan.PlanHash || archived.GeneratedAt != retained.generated {
				t.Fatal("archive did not return its first retained approval", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, want) {
				t.Fatal("archive bytes differ from the first exact publication", err)
			}
		})
	}
}
