package main

import (
	"os"
	"strings"
	"testing"
)

// A catalog suffix check cannot replace the index's real EXPLAIN checks or
// prove that its unpublished migration follows the published automation tail.
func releaseFeedbackIndexGateGroup(t *testing.T) releaseEvidenceV2GateGroup {
	t.Helper()
	return releaseEvidenceV2GateGroup{
		phase: "server_db", job: "server-db", variable: "evidence_source_tests",
		alternatives:     "AccountFeedbackNetwork|FeedbackIndexFollowsPublishedAutomationMigrations",
		requiredSelector: "^Test(AccountFeedbackNetwork|FeedbackIndexFollowsPublishedAutomationMigrations)",
		packages:         []string{"."},
		commands: []string{
			`go test . -run "$evidence_source_tests" -count=1`,
			`go test -race . -run "$evidence_source_tests" -count=1`,
		},
		sources: map[string][]string{
			".": releaseEvidenceV2GateSources(t, []string{"../../server/db_migrations_test.go"}),
		},
	}
}

func TestProducerGateStateSelectionLocalFeedbackIndexMigrationAndReadPlans(t *testing.T) {
	encoded, err := os.ReadFile("../scripts/test-release-1.0-local.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyReleaseEvidenceV2GateGroup(string(encoded), releaseFeedbackIndexGateGroup(t)); err != nil {
		t.Fatal(err)
	}
}

func TestProducerGateStateSelectionLocalFeedbackIndexRejectsOmissions(t *testing.T) {
	encoded, err := os.ReadFile("../scripts/test-release-1.0-local.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(encoded)
	group := releaseFeedbackIndexGateGroup(t)
	if err := verifyReleaseEvidenceV2GateGroup(script, group); err != nil {
		t.Fatal(err)
	}
	selector, err := releaseConnectPolicySelectorAssignment(script, group.variable)
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range strings.Split(group.alternatives, "|") {
		changed := strings.Replace(script, group.variable+"='"+selector+"'", group.variable+"='"+strings.Replace(selector, "|"+family, "", 1)+"'", 1)
		if changed == script {
			t.Fatalf("omission fixture did not remove %s", family)
		}
		if err := verifyReleaseEvidenceV2GateGroup(changed, group); err == nil {
			t.Fatalf("gate accepted omitted %s", family)
		}
	}
	for _, command := range append(append([]string(nil), group.commands...), "release_gate_start server-db release_phase_server_db") {
		changed := strings.Replace(script, command, "# omitted: "+command, 1)
		if err := verifyReleaseEvidenceV2GateGroup(changed, group); err == nil {
			t.Fatalf("gate accepted omitted executable command %s", command)
		}
	}
}
