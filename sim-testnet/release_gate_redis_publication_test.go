// Keep both release gates tied to the Redis publication regression sources.
package main

import (
	"os"
	"strings"
	"testing"
)

// Publication cleanup must keep its actual cancellation, acknowledged commit
// and retained-custody regressions in both private-service gate modes.
func releaseRedisPublicationGateGroup(t *testing.T) releaseEvidenceV2GateGroup {
	t.Helper()
	return releaseEvidenceV2GateGroup{
		phase: "server_db", job: "server-db", variable: "evidence_source_tests",
		alternatives:     "RedisCanceled|RedisHealthyPublication|RedisConfirmedPublication",
		requiredSelector: "^Test(RedisCanceled|RedisHealthyPublication|RedisConfirmedPublication)",
		packages:         []string{"./model"},
		commands: []string{
			`go test ./model -run "$evidence_source_tests" -count=1`,
			`go test -race ./model -run "$evidence_source_tests" -count=1`,
		},
		sources: map[string][]string{
			"./model": releaseEvidenceV2GateSources(t, []string{
				"../../server/model/subscription_redis_publication_cancel_test.go",
				"../../server/model/subscription_redis_publication_custody_test.go",
				"../../server/model/subscription_redis_publication_ack_test.go",
			}),
		},
	}
}

// Both gates must execute the publication source families in each model mode.
func TestProducerGateStateSelectionCoversRedisPublicationSourcesInBothGates(t *testing.T) {
	t.Parallel()
	group := releaseRedisPublicationGateGroup(t)
	for _, name := range []string{"local", "producer-gate"} {
		encoded, err := os.ReadFile("../scripts/test-release-1.0-" + name + ".sh")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := verifyReleaseEvidenceV2GateGroup(string(encoded), group); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Each family, both real Go invocations and their admitted owner are required;
// comments or an otherwise complete selector cannot replace execution.
func TestProducerGateStateSelectionRejectsRedisPublicationCoverageOmissions(t *testing.T) {
	t.Parallel()
	group := releaseRedisPublicationGateGroup(t)
	for _, name := range []string{"local", "producer-gate"} {
		encoded, err := os.ReadFile("../scripts/test-release-1.0-" + name + ".sh")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		script := string(encoded)
		if err := verifyReleaseEvidenceV2GateGroup(script, group); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		selector, err := releaseConnectPolicySelectorAssignment(script, group.variable)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, omitted := range strings.Split(group.alternatives, "|") {
			var retained []string
			for _, family := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(selector, "^Test("), ")"), "|") {
				if family != omitted {
					retained = append(retained, family)
				}
			}
			changed := strings.Replace(script, group.variable+"='"+selector+"'", group.variable+"='^Test("+strings.Join(retained, "|")+")'", 1)
			if changed == script {
				t.Fatalf("%s: omission fixture did not remove %s", name, omitted)
			}
			if err := verifyReleaseEvidenceV2GateGroup(changed, group); err == nil {
				t.Fatalf("%s: gate accepted omitted %s", name, omitted)
			}
		}
		for _, command := range append(append([]string(nil), group.commands...), "release_gate_start server-db release_phase_server_db") {
			changed := strings.Replace(script, command, "# omitted: "+command, 1)
			if err := verifyReleaseEvidenceV2GateGroup(changed, group); err == nil {
				t.Fatalf("%s: gate accepted omitted executable command %s", name, command)
			}
		}
	}
}
