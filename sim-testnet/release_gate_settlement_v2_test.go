package main

// Settlement v2 in the aggregate gate (VERSION2-FINAL.md §14 gates 1, 3 and
// 4, §15): sn's v2 suites, the end-to-end suite, the scale shard, the
// Solidity coverage bar, the deep invariant campaign, and the operator's and
// the SDK's v2 suites, each executed by an admitted phase.

import (
	"errors"
	"fmt"
	"go/build"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const releaseGateSolidityCoverageCommand = `FOUNDRY_OUT="$TMPDIR/coverage-out" FOUNDRY_CACHE_PATH="$TMPDIR/coverage-cache" forge coverage --ir-minimum --report lcov --skip STValidatorEvidence.sol --skip ValidatorEvidenceStorage.t.sol --skip ValidatorEvidenceActivation.t.sol --report-file "$TMPDIR/lcov.info" || coverage_status=$?`

const releaseGateV2InvariantCommand = "FOUNDRY_PROFILE=v2_invariant forge test --match-path test/V2Invariant.t.sol"

// The server's scale shard selects its roots by this prefix.
const releaseGateServerScaleSelector = "^TestStV2Scale"

// The E suite needs the server's database (E-1, E-3), so the aggregate job,
// which has none, skips it by this prefix, and sn-e2e runs it on the gate's
// private services.
const releaseGateAggregateE2ESkip = "^TestSettlementV2E2E"

// server/stmonitor/testfixture's databases need an admin URL of the gate's
// private PostgreSQL, so the aggregate job skips these mainnet roots by name
// and sn-e2e runs them, normal and race, beside the E suite.
var releaseGateStmonitorFixtureRoots = []string{
	"TestMonitorOperatorCredentialFenceAndPublicationAmbiguity",
	"TestMonitorOperatorCommandReadsProductionJournalAndRestartsOutage",
	"TestMonitorStorageStoppedOperatorReleasesOwnersBeforePeerStops",
	"TestMonitorStorageOperatorLostAckRetainsIncident",
}

const releaseGateStmonitorFixtureSelector = "^(TestMonitorOperatorCredentialFenceAndPublicationAmbiguity|TestMonitorOperatorCommandReadsProductionJournalAndRestartsOutage|TestMonitorStorageStoppedOperatorReleasesOwnersBeforePeerStops|TestMonitorStorageOperatorLostAckRetainsIncident)$"

// The only admin URL: the private PostgreSQL's endpoint and superuser.
const releaseGateStmonitorAdminURL = `export STMONITOR_TEST_ADMIN_URL="postgres://postgres:urnetwork-local-test@$WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY/postgres?sslmode=disable"`

// sn-e2e's commands after the E suite's.
var releaseGateStmonitorFixtureLines = []string{
	"stmonitor_fixture_tests='" + releaseGateStmonitorFixtureSelector + "'",
	releaseGateStmonitorAdminURL,
	`go test ./mainnet -run "$stmonitor_fixture_tests" -count=1`,
	`go test -race ./mainnet -run "$stmonitor_fixture_tests" -count=1`,
}

// The aggregate job's skip: the E suite and the stmonitor fixture's roots.
const releaseGateAggregatePinnedSkip = "^(TestSettlementV2E2E|TestMonitorOperatorCredentialFenceAndPublicationAmbiguity$|TestMonitorOperatorCommandReadsProductionJournalAndRestartsOutage$|TestMonitorStorageStoppedOperatorReleasesOwnersBeforePeerStops$|TestMonitorStorageOperatorLostAckRetainsIncident$)"

// Local dependency mode also skips its two named sim-testnet tests: the
// release-lock comparison, which only a sealed run's checkouts can pass, and
// the discovery test that stats the workspace vault.
const releaseGateAggregateLocalSkip = "^(TestSettlementV2E2E|TestMonitorOperatorCredentialFenceAndPublicationAmbiguity$|TestMonitorOperatorCommandReadsProductionJournalAndRestartsOutage$|TestMonitorStorageStoppedOperatorReleasesOwnersBeforePeerStops$|TestMonitorStorageOperatorLostAckRetainsIncident$|TestReleaseLockMatchesCheckout$|TestRepositoryDiscoveryUsesModuleIdentity$)"

// The aggregate job: every package of sn but ./mainnet, ./validator and
// ./sim-testnet, whose shards run them with the same skip, in normal mode,
// without the E suite and, in local mode, without its named sim-testnet tests.
const releaseGateAggregateNormalCommand = `go test -parallel=4 -timeout 90m "${sn_packages[@]}" -count=1 -skip "$release_gate_aggregate_skip"`

// The aggregate job's whole body: sn's package list without the sharded
// packages, then the run.
var releaseGateAggregateNormalBody = []string{
	`cd "$sn_repo"`,
	`sn_package_list="$(go list ./... | grep -Evx 'github[.]com/urfoundation/sn/(mainnet|validator|sim-testnet)')"`,
	`[[ -n "$sn_package_list" ]]`,
	`mapfile -t sn_packages <<<"$sn_package_list"`,
	releaseGateAggregateNormalCommand,
}

// Each mode's skip, assigned once before the job's definition.
const releaseGateAggregateSkipAssignment = "release_gate_aggregate_skip='" + releaseGateAggregatePinnedSkip + "'\n" +
	"if [[ \"$release_gate_dependency_mode\" == local ]]; then\n" +
	"  release_gate_aggregate_skip='" + releaseGateAggregateLocalSkip + "'\n" +
	"fi\n" +
	"export release_gate_aggregate_skip\n" +
	"release_phase_sn_all_normal() {\n"

// The database half of the scale shard and the end-to-end suite have the same
// service-ready test as the database suites, with the unavailable owner as
// each job's only alternative.
const releaseGateServiceShardsAdmission = `if [[ "$release_gate_services_ready" != 1 ]]; then
release_gate_start server-scale release_gate_unavailable_services
release_gate_start sn-e2e release_gate_unavailable_services
else
release_gate_start server-scale release_phase_server_scale
release_gate_start sn-e2e release_phase_sn_e2e
fi`

// The preamble of a phase on the gate's private services: the server's test
// environment over this gate's services, with the suite resources checked.
var releaseGateServicePreamble = []string{
	`cd "$workspace/server"`,
	"export WARP_ENV=local", "export WARP_SERVICE=test", "export WARP_DOMAIN=bringyour.com",
	"export WARP_BLOCK=test", "export WARP_VERSION=0.0.0",
	`source "$RELEASE_GATE_SERVICE_ENV"`, `source "$workspace/server/test-env.sh"`,
	`test_env_validate_suite_resource_manifest "$TEST_ENV_SUITE_RESOURCE_MANIFEST" "$WARP_VAULT_HOME" "$WARP_CONFIG_HOME"`,
}

func readReleaseGateLocalScript(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../scripts/test-release-1.0-local.sh")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The executable lines of one phase definition, without comments.
func releaseGatePhaseCommands(script, phase string) ([]string, []int, error) {
	pattern := regexp.MustCompile("(?ms)^[\\t ]*" + regexp.QuoteMeta("release_phase_"+phase) + "\\(\\) \\{\\n(.*?)^[\\t ]*\\}[\\t ]*$")
	definitions := pattern.FindAllStringSubmatchIndex(script, -1)
	if len(definitions) != 1 {
		return nil, nil, fmt.Errorf("gate has %d release_phase_%s definitions", len(definitions), phase)
	}
	var commands []string
	for _, line := range strings.Split(script[definitions[0][2]:definitions[0][3]], "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			commands = append(commands, line)
		}
	}
	return commands, definitions[0], nil
}

// The gate's lines outside phase definitions, trimmed.
func releaseGateRegistryLines(script string) string {
	registry := regexp.MustCompile(`(?ms)^[\t ]*release_phase_[a-z0-9_]+\(\) \{\n.*?^[\t ]*\}[\t ]*$`).ReplaceAllString(script, "")
	var lines []string
	for _, line := range strings.Split(registry, "\n") {
		lines = append(lines, strings.TrimSpace(line))
	}
	return strings.Join(lines, "\n")
}

// One unconditional admission of the phase, after its definition.
func verifyReleaseGateAdmission(script, job, phase string, definition []int) error {
	start := "release_gate_start " + job + " release_phase_" + phase
	invocations := regexp.MustCompile("(?m)^" + regexp.QuoteMeta(start) + "[\\t ]*$")
	calls := invocations.FindAllStringIndex(script, -1)
	if len(calls) != 1 || len(invocations.FindAllString(releaseGateRegistryLines(script), -1)) != 1 || calls[0][0] < definition[1] {
		return fmt.Errorf("gate does not admit %s exactly once after its definition", job)
	}
	conditions, err := releaseGateRegistrationConditions(script, start)
	if err != nil || len(conditions) != 0 {
		return fmt.Errorf("gate admits %s conditionally: %v %v", job, conditions, err)
	}
	return nil
}

// A phase whose whole body is exactly these commands, admitted once.
func verifyReleaseGateExactPhase(script, job, phase string, want []string) error {
	commands, definition, err := releaseGatePhaseCommands(script, phase)
	if err != nil {
		return err
	}
	if !slices.Equal(commands, want) {
		return fmt.Errorf("release_phase_%s runs %q, want %q", phase, commands, want)
	}
	return verifyReleaseGateAdmission(script, job, phase, definition)
}

// releaseGateScaleCensus maps each package under root, as ./dir, with a Go
// file that only the scale build tag builds, to those files' sources. Like
// go, it skips testdata, hidden and underscore directories and nested modules.
func releaseGateScaleCensus(root string) (map[string][]string, error) {
	scale := build.Default
	scale.BuildTags = append(slices.Clone(scale.BuildTags), "scale")
	census := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path == root {
				return nil
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		plain, err := build.Default.MatchFile(filepath.Dir(path), name)
		if err != nil {
			return err
		}
		tagged, err := scale.MatchFile(filepath.Dir(path), name)
		if err != nil {
			return err
		}
		if tagged && !plain {
			relative, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			source, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			packagePath := "./" + filepath.ToSlash(relative)
			census[packagePath] = append(census[packagePath], string(source))
		}
		return nil
	})
	return census, err
}

func readReleaseGateScaleCensus(t *testing.T) (map[string][]string, map[string][]string) {
	t.Helper()
	sn, err := releaseGateScaleCensus("..")
	if err != nil {
		t.Fatal(err)
	}
	server, err := releaseGateScaleCensus("../../server")
	if err != nil {
		t.Fatal(err)
	}
	return sn, server
}

// A phase on the gate's private services whose whole body is exactly these
// commands, admitted once, after its definition, in the services block beside
// the database suites' own admission.
func verifyReleaseGateServicePhase(script, job, phase string, want []string) error {
	commands, definition, err := releaseGatePhaseCommands(script, phase)
	if err != nil {
		return err
	}
	if !slices.Equal(commands, want) {
		return fmt.Errorf("release_phase_%s runs %q, want %q", phase, commands, want)
	}
	registry := releaseGateRegistryLines(script)
	if strings.Count(registry, releaseGateServiceShardsAdmission) != 1 {
		return fmt.Errorf("%s lacks its exact service-gated admission", job)
	}
	for _, function := range []string{"release_phase_" + phase, "release_gate_unavailable_services"} {
		start := regexp.MustCompile("(?m)^" + regexp.QuoteMeta("release_gate_start "+job+" "+function) + "$")
		if len(start.FindAllString(registry, -1)) != 1 {
			return fmt.Errorf("%s has duplicated or foreign admission", job)
		}
	}
	if strings.Index(script, "release_gate_start "+job+" release_phase_"+phase) < definition[1] {
		return fmt.Errorf("%s is admitted before its definition", job)
	}
	database, err := releaseGateRegistrationConditions(script, "release_gate_start server-db release_phase_server_db")
	if err != nil {
		return err
	}
	services, err := releaseGateRegistrationConditions(script, strings.SplitN(releaseGateServiceShardsAdmission, "\n", 2)[0])
	if err != nil || len(database) == 0 || database[len(database)-1] != `if [[ "$release_gate_services_ready" == 1 ]]; then` || !slices.Equal(services, database[:len(database)-1]) {
		return fmt.Errorf("%s is admitted apart from the database suites: %q %q %v", job, services, database, err)
	}
	return nil
}

// GL-10 and SV-6 (§15 scale tests): the sn shard runs every sn package of the
// census, verbose so that each root's time and memory are in its log, under
// one explicit deadline, and is admitted unconditionally. Its database half
// runs every server package of the census on the gate's private services,
// selecting every root of their scale-tagged files, whenever the database
// suites run. No other gate command builds with a tag: the runtime rehearsal
// stays beside the gate.
func verifyReleaseGateScaleShards(script string, sn, server map[string][]string) error {
	if len(sn) == 0 || len(server) == 0 {
		return errors.New("a repository has no scale-tagged package")
	}
	snCommand := "go test -tags scale -v -timeout 90m " + strings.Join(slices.Sorted(maps.Keys(sn)), " ") + " -count=1"
	if err := verifyReleaseGateExactPhase(script, "sn-scale", "sn_scale", []string{`cd "$sn_repo"`, snCommand}); err != nil {
		return err
	}
	serverCommand := "go test -tags scale " + strings.Join(slices.Sorted(maps.Keys(server)), " ") + " -run '" + releaseGateServerScaleSelector + "' -count=1 -p 1 -v -timeout 180m"
	if err := verifyReleaseGateServicePhase(script, "server-scale", "server_scale", append(slices.Clone(releaseGateServicePreamble), serverCommand)); err != nil {
		return err
	}
	for _, packagePath := range slices.Sorted(maps.Keys(server)) {
		if err := verifyReleaseSourceTestCoverage(releaseGateServerScaleSelector, "^Test", server[packagePath]); err != nil {
			return fmt.Errorf("server scale package %s: %w", packagePath, err)
		}
	}
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") && strings.Contains(line, "-tags") && line != snCommand && line != serverCommand {
			return fmt.Errorf("gate builds with a tag outside the scale shard: %s", line)
		}
	}
	return nil
}

// §15 (GT, GA, GL, CL, VA) and gate 1's checker: sn's v2 suites in one job,
// normal and race. The ledger package and the checker run whole; every root
// of a v2 test file of the other packages is selected, under an explicit
// 30-minute deadline.
func releaseGateSnV2Group(t *testing.T) releaseEvidenceV2GateGroup {
	t.Helper()
	whole := "./ledger ./scripts/solidity-coverage"
	packages := "./merkle ./protocol ./miner ./miner/onchain ./validator"
	return releaseEvidenceV2GateGroup{
		phase: "sn_v2", job: "sn-v2", variable: "settlement_v2_tests",
		alternatives: "Ledger|ClaimV2|MainUsageClaimDaemonV2|V2|Withdraw|SubmitCarriesValue|SettlementV2|ProductionSettlementV2|ProductionRenderedSuccessorKeepsSettlementV2|ReleaseRuntimeRunsLedgerAudit",
		packages:     strings.Fields(packages),
		sources: map[string][]string{
			"./merkle":        releaseEvidenceV2GateSources(t, []string{"../merkle/ledger_*_test.go"}),
			"./protocol":      releaseEvidenceV2GateSources(t, []string{"../protocol/ledger_*_test.go"}),
			"./miner":         releaseEvidenceV2GateSources(t, []string{"../miner/claim_v2_*_test.go"}),
			"./miner/onchain": releaseEvidenceV2GateSources(t, []string{"../miner/onchain/v2*_test.go", "../miner/onchain/withdraw*_test.go", "../miner/onchain/submit_value*_test.go"}),
			"./validator":     releaseEvidenceV2GateSources(t, []string{"../validator/settlement_v2*_test.go", "../validator/production_settlement_v2*_test.go"}),
		},
		commands: []string{
			"go test " + whole + " -count=1",
			"go test -race " + whole + " -count=1",
			"go test " + packages + ` -run "$settlement_v2_tests" -count=1 -timeout 30m`,
			"go test -race " + packages + ` -run "$settlement_v2_tests" -count=1 -timeout 30m`,
		},
	}
}

// The job's whole body is its selector and its four commands, admitted once
// and unconditionally.
func verifyReleaseGateSnV2(script string, group releaseEvidenceV2GateGroup) error {
	selector, err := releaseConnectPolicySelectorAssignment(script, group.variable)
	if err != nil {
		return err
	}
	body := []string{`cd "$sn_repo"`, group.commands[0], group.commands[1], group.variable + "='" + selector + "'", group.commands[2], group.commands[3]}
	if err := verifyReleaseGateExactPhase(script, group.job, group.phase, body); err != nil {
		return err
	}
	return verifyReleaseEvidenceV2GateGroup(script, group)
}

// E-1 to E-7 (§15.6): the end-to-end suite in its own job, normal and race;
// every root of its test files is selected.
func releaseGateSnE2EGroup(t *testing.T) releaseEvidenceV2GateGroup {
	t.Helper()
	return releaseEvidenceV2GateGroup{
		phase: "sn_e2e", job: "sn-e2e", variable: "settlement_v2_e2e_tests",
		alternatives: "SettlementV2E2E",
		packages:     []string{"./mainnet"},
		sources:      map[string][]string{"./mainnet": releaseEvidenceV2GateSources(t, []string{"../mainnet/settlement_v2_e2e_*_test.go"})},
		commands: []string{
			`go test ./mainnet -run "$settlement_v2_e2e_tests" -count=1 -timeout 30m`,
			`go test -race ./mainnet -run "$settlement_v2_e2e_tests" -count=1 -timeout 30m`,
		},
	}
}

// The job's whole body is the services preamble, then its selector and its two
// commands in sn, then the stmonitor fixture's selector, admin URL and two
// commands, admitted once beside the database suites (§15: the E suite runs in
// the server-database shard).
func verifyReleaseGateSnE2E(script string, group releaseEvidenceV2GateGroup) error {
	selector, err := releaseConnectPolicySelectorAssignment(script, group.variable)
	if err != nil {
		return err
	}
	body := append(slices.Clone(releaseGateServicePreamble), `cd "$sn_repo"`, group.variable+"='"+selector+"'", group.commands[0], group.commands[1])
	body = append(body, releaseGateStmonitorFixtureLines...)
	if err := verifyReleaseGateServicePhase(script, group.job, group.phase, body); err != nil {
		return err
	}
	return verifyReleaseEvidenceV2GateGroup(script, group)
}

// Gate 1 (§15 coverage): the pinned command in a private build, then the bar,
// checked even when a test fails, whose status the phase still returns.
func verifyReleaseGateSolidityCoverage(script string) error {
	return verifyReleaseGateExactPhase(script, "solidity-coverage", "solidity_coverage", []string{
		`cd "$sn_repo/evm"`, "coverage_status=0", releaseGateSolidityCoverageCommand,
		`cd "$sn_repo"`, `go run ./scripts/solidity-coverage evm "$TMPDIR/lcov.info"`, `return "$coverage_status"`,
	})
}

// I-1 to I-6 (§15.5): the deep profile exists, the test file sizes its
// campaign, and the Solidity phase runs it once, right after the plain suite
// on the release build.
func verifyReleaseGateV2InvariantCampaign(script, foundry, invariant string) error {
	if !regexp.MustCompile(`(?m)^\[profile\.v2_invariant\.invariant\]$`).MatchString(foundry) {
		return errors.New("foundry.toml has no v2_invariant invariant profile")
	}
	for _, key := range []string{"runs", "depth"} {
		if !regexp.MustCompile(`(?m)^/// forge-config: v2_invariant\.invariant\.` + key + ` = [1-9][0-9]*$`).MatchString(invariant) {
			return fmt.Errorf("V2Invariant.t.sol sizes no deep campaign %s", key)
		}
	}
	commands, definition, err := releaseGatePhaseCommands(script, "solidity")
	if err != nil {
		return err
	}
	prefix := []string{`cd "$sn_repo/evm"`, "forge fmt --check", "forge build --deny warnings --sizes", "forge test --summary", releaseGateV2InvariantCommand}
	if len(commands) < len(prefix) || !slices.Equal(commands[:len(prefix)], prefix) {
		return errors.New("the Solidity phase does not run the deep v2 invariant campaign after the plain suite")
	}
	count := 0
	for _, line := range strings.Split(script, "\n") {
		if line = strings.TrimSpace(line); !strings.HasPrefix(line, "#") && strings.Contains(line, "v2_invariant") {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("gate runs the v2_invariant profile %d times", count)
	}
	return verifyReleaseGateAdmission(script, "solidity", "solidity", definition)
}

// §15.7 (SV-1 to SV-8): the operator's v2 suites, normal and race, each under
// an explicit 30-minute deadline, on the gate's private services; every root
// of a v2 test file, and of the gas-floor files of the v1 closes and of the
// deposit key's stake moves (SV-4), is selected. The deposit key's retries
// (SV-1) are selected by prefix, since their files hold other roots that this
// selector leaves out: a reserved deposit whose deadline has passed is signed
// as reserved, so it reverts and frees the key.
func releaseGateLedgerV2ServerGroup(t *testing.T) releaseEvidenceV2GateGroup {
	t.Helper()
	packages := ". ./api ./bringyourctl ./controller ./model ./startifact ./taskworker ./taskworker/work"
	return releaseEvidenceV2GateGroup{
		phase: "server_db", job: "server-db", variable: "ledger_v2_tests",
		alternatives: "StLedgerV2|LedgerV2|LedgerObject|StConfigSelectsLedgerV2|StConfigRefusesMalformedLedgerV2|StSyncEventsMirrorsLedgerV2|StV1DriverStopsAtFirstV2Epoch|StSyncChainKeepsV1BelowFirstV2Epoch|WorkloadsRunLedgerV2|SnLedger|StSubtensorStorageKeys|StConfigSelectsPruneAlertPositions|StV1FinalizationOfAV2EpochNotifiesNothing|StEpochEarningsKeepV1ForV1Epochs|EpochEarningsTemplateWithAV2Amount|RouterInstallsTheLedgerBuilder|StV2EpochCommands|ProviderWalletEpochRetainedRefusesUnusableSubnet|StV1Close|StV1GasFloor|StV1Deposit|StV1PendingReservation|StOperatorGasRemainingLiability|StOperatorGasPending|StOperatorGasRevertOfAPendingExpiredDeposit|StDepositReservedAttemptAdmitsOnlyAnExpiredDeposit",
		packages:     strings.Fields(packages),
		sources: map[string][]string{
			".":                 releaseEvidenceV2GateSources(t, []string{"../../server/*ledger_v2*_test.go"}),
			"./api":             releaseEvidenceV2GateSources(t, []string{"../../server/api/*ledger_v2*_test.go", "../../server/api/sn_ledger*_test.go"}),
			"./bringyourctl":    releaseEvidenceV2GateSources(t, []string{"../../server/bringyourctl/*ledger_v2*_test.go"}),
			"./controller":      releaseEvidenceV2GateSources(t, []string{"../../server/controller/*ledger_v2*_test.go", "../../server/controller/st_v1_close_gas_test.go", "../../server/controller/st_v1_deposit_gas_test.go"}),
			"./model":           releaseEvidenceV2GateSources(t, []string{"../../server/model/*ledger_v2*_test.go"}),
			"./startifact":      releaseEvidenceV2GateSources(t, []string{"../../server/startifact/*ledger_v2*_test.go"}),
			"./taskworker":      releaseEvidenceV2GateSources(t, []string{"../../server/taskworker/*ledger_v2*_test.go"}),
			"./taskworker/work": releaseEvidenceV2GateSources(t, []string{"../../server/taskworker/work/*ledger_v2*_test.go"}),
		},
		commands: []string{
			"go test " + packages + ` -run "$ledger_v2_tests" -count=1 -timeout 30m`,
			"go test -race " + packages + ` -run "$ledger_v2_tests" -count=1 -timeout 30m`,
		},
	}
}

// The monitor's contracts of the published migration suffix, to which
// migrations 821 and 822, settlement v2's, add their schema, on the gate's
// private services in both modes.
func releaseGateMigrationSuffixGroup(t *testing.T) releaseEvidenceV2GateGroup {
	t.Helper()
	return releaseEvidenceV2GateGroup{
		phase: "server_db", job: "server-db", variable: "migration_suffix_tests",
		alternatives: "MigrationsCurrentSuffix|MigrationsDeviceClientVersionPublicationBoundary",
		packages:     []string{"./monitor"},
		sources:      map[string][]string{"./monitor": releaseEvidenceV2GateSources(t, []string{"../../server/monitor/migration_current_suffix_test.go"})},
		commands: []string{
			`go test ./monitor -run "$migration_suffix_tests" -count=1`,
			`go test -race ./monitor -run "$migration_suffix_tests" -count=1`,
		},
	}
}

// Migration 822 and SV-6's database contracts: bulk wallet selection, batched
// resolutions, the FleetBound scan and head exclusion, in the database
// phase's controller and model selectors, normal and race.
func releaseGateServerScaleDatabaseGroups(t *testing.T) []releaseEvidenceV2GateGroup {
	t.Helper()
	group := func(variable, packagePath string, files ...string) releaseEvidenceV2GateGroup {
		return releaseEvidenceV2GateGroup{
			phase: "server_db", job: "server-db", variable: variable,
			packages: []string{packagePath},
			sources:  map[string][]string{packagePath: releaseEvidenceV2GateSources(t, files)},
			commands: []string{
				"go test " + packagePath + ` -run "$` + variable + `" -count=1`,
				"go test -race " + packagePath + ` -run "$` + variable + `" -count=1`,
			},
		}
	}
	return []releaseEvidenceV2GateGroup{
		group("controller_db_tests", "./controller", "../../server/controller/st_head_exclusion_test.go"),
		group("model_db_tests", "./model", "../../server/model/st_fleet_bound_test.go",
			"../../server/model/st_payout_wallet_resolution_batch_test.go", "../../server/model/st_provider_wallet_bulk_test.go"),
	}
}

func releaseGateOperatorV2Groups(t *testing.T) []releaseEvidenceV2GateGroup {
	t.Helper()
	return append([]releaseEvidenceV2GateGroup{releaseGateLedgerV2ServerGroup(t), releaseGateMigrationSuffixGroup(t)}, releaseGateServerScaleDatabaseGroups(t)...)
}

// CL-2 and CL-4 (§11, §15.7): the SDK's v2 ledger display and claims, with
// their vault2 bindings and ledger proofs, normal and race; every root of a
// v2 test file is selected.
func releaseGateSdkLedgerV2Group(t *testing.T) releaseEvidenceV2GateGroup {
	t.Helper()
	packages := ". ./sn/evm ./sn/merkle"
	return releaseEvidenceV2GateGroup{
		phase: "sdk", variable: "sdk_ledger_v2_tests",
		alternatives: "SnLedger|BuildTestLedgerTreeMatchesTheVectors|SnChainSettingsVault2Address|Ledger|PackLedger|DecodeLedger|EthCallFrom|TransactionReceiptLogs",
		packages:     strings.Fields(packages),
		sources: map[string][]string{
			".":           releaseEvidenceV2GateSources(t, []string{"../../sdk/sn_ledger*_test.go"}),
			"./sn/evm":    releaseEvidenceV2GateSources(t, []string{"../../sdk/sn/evm/vault2_test.go", "../../sdk/sn/evm/rpc_test.go"}),
			"./sn/merkle": releaseEvidenceV2GateSources(t, []string{"../../sdk/sn/merkle/ledger_*_test.go"}),
		},
		commands: []string{
			"go test " + packages + ` -run "$sdk_ledger_v2_tests" -count=1`,
			"go test -race " + packages + ` -run "$sdk_ledger_v2_tests" -count=1`,
		},
	}
}

// Removing a family, a mode or the admitted phase fails, as does a root of a
// group's test files that escapes its selector.
func verifyReleaseGateGroupRejectsOmissions(t *testing.T, script string, group releaseEvidenceV2GateGroup) {
	t.Helper()
	selector, err := releaseConnectPolicySelectorAssignment(script, group.variable)
	if err != nil {
		t.Fatal(err)
	}
	if group.alternatives != "" {
		for _, omitted := range strings.Split(group.alternatives, "|") {
			retained := slices.DeleteFunc(strings.Split(group.alternatives, "|"), func(value string) bool { return value == omitted })
			changed := strings.Replace(script, group.variable+"='"+selector+"'", group.variable+"='^Test("+strings.Join(retained, "|")+")'", 1)
			if err := verifyReleaseEvidenceV2GateGroup(changed, group); err == nil {
				t.Fatalf("%s accepted the removed family %s", group.variable, omitted)
			}
		}
	}
	job := group.job
	if job == "" {
		job = group.phase
	}
	for _, command := range append(slices.Clone(group.commands), "release_gate_start "+job+" release_phase_"+group.phase) {
		if strings.Count(script, command) != 1 {
			t.Fatalf("%s: the mutation names no unique text: %s", group.variable, command)
		}
		if err := verifyReleaseEvidenceV2GateGroup(strings.Replace(script, command, "# omitted: "+command, 1), group); err == nil {
			t.Fatalf("%s accepted the removed command %s", group.variable, command)
		}
	}
	escaped := group
	escaped.sources = maps.Clone(group.sources)
	packagePath := group.packages[len(group.packages)-1]
	escaped.sources[packagePath] = append(slices.Clone(group.sources[packagePath]), "package fixture\nfunc TestOmittedSettlementV2Root(t *testing.T) {}\n")
	if err := verifyReleaseEvidenceV2GateGroup(script, escaped); err == nil {
		t.Fatalf("%s accepted a root outside its selector", group.variable)
	}
}

// GL-10 and SV-6: the scale shards run every package of both censuses.
func TestReleaseGateSettlementV2RunsEveryScalePackage(t *testing.T) {
	t.Parallel()
	sn, server := readReleaseGateScaleCensus(t)
	if sn["./ledger"] == nil || server["./controller"] == nil || server["./model"] == nil {
		t.Fatalf("scale census %q and %q lacks GL-10's ./ledger or SV-6's ./controller and ./model", slices.Sorted(maps.Keys(sn)), slices.Sorted(maps.Keys(server)))
	}
	if err := verifyReleaseGateScaleShards(readReleaseGateLocalScript(t), sn, server); err != nil {
		t.Fatal(err)
	}
}

// GL-10: the census reads build constraints as go does: a file every build
// includes or excludes is not scale-tagged, and testdata, hidden and
// underscore directories and nested modules are not packages of this module.
func TestReleaseGateSettlementV2ScaleCensusFollowsBuildConstraints(t *testing.T) {
	t.Parallel()
	foreign := "windows"
	if runtime.GOOS == foreign {
		foreign = "linux"
	}
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                      "module example\n",
		"plain/plain_test.go":         "package plain\n",
		"tagged/tagged.go":            "package tagged\n",
		"tagged/scale_test.go":        "//go:build scale\n\npackage tagged\n",
		"either/either_test.go":       "//go:build scale || !scale\n\npackage either\n",
		"negated/negated_test.go":     "//go:build !scale\n\npackage negated\n",
		"rehearsal/node_test.go":      "//go:build runtime_rehearsal\n\npackage rehearsal\n",
		"host/deeper/scale_test.go":   "//go:build scale && " + runtime.GOOS + "\n\npackage deeper\n",
		"foreign/scale_test.go":       "//go:build scale && " + foreign + "\n\npackage foreign\n",
		"tagged/testdata/scale.go":    "//go:build scale\n\npackage testdata\n",
		"_ignored/scale_test.go":      "//go:build scale\n\npackage ignored\n",
		".hidden/scale_test.go":       "//go:build scale\n\npackage hidden\n",
		"module/go.mod":               "module other\n",
		"module/scale_test.go":        "//go:build scale\n\npackage module\n",
		"tagged/notes_scale_test.txt": "//go:build scale\n",
	}
	for path, source := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	census, err := releaseGateScaleCensus(root)
	if err != nil || !slices.Equal(slices.Sorted(maps.Keys(census)), []string{"./host/deeper", "./tagged"}) ||
		!slices.Equal(census["./tagged"], []string{files["tagged/scale_test.go"]}) || !slices.Equal(census["./host/deeper"], []string{files["host/deeper/scale_test.go"]}) {
		t.Fatalf("scale census %q: %v", census, err)
	}
	if _, err := releaseGateScaleCensus(filepath.Join(root, "absent")); err == nil {
		t.Fatal("an unreadable tree gave a census")
	}
}

// GL-10 and SV-6: each loss of a tag, the log, a deadline, an uncached run, a
// package, a root, the private services or the exact admission fails, as does
// a tagged command elsewhere.
func TestReleaseGateSettlementV2ScaleShardRejectsDrift(t *testing.T) {
	t.Parallel()
	script := readReleaseGateLocalScript(t)
	sn, server := readReleaseGateScaleCensus(t)
	if err := verifyReleaseGateScaleShards(script, sn, server); err != nil {
		t.Fatal(err)
	}
	const command = "go test -tags scale -v -timeout 90m ./ledger -count=1"
	const start = "release_gate_start sn-scale release_phase_sn_scale"
	const serverCommand = "go test -tags scale ./controller ./model -run '^TestStV2Scale' -count=1 -p 1 -v -timeout 180m"
	const services = `    source "$RELEASE_GATE_SERVICE_ENV"` + "\n" + `    source "$workspace/server/test-env.sh"` + "\n" + `    test_env_validate_suite_resource_manifest "$TEST_ENV_SUITE_RESOURCE_MANIFEST" "$WARP_VAULT_HOME" "$WARP_CONFIG_HOME"` + "\n" + "    " + serverCommand
	const unavailable = "release_gate_start server-scale release_gate_unavailable_services"
	const admitted = "release_gate_start server-scale release_phase_server_scale"
	const ready = `if [[ "$release_gate_services_ready" != 1 ]]; then`
	for _, change := range []struct{ name, old, new string }{
		{"untagged", command, "go test -v -timeout 90m ./ledger -count=1"},
		{"another tag", command, strings.Replace(command, "scale", "runtime_rehearsal", 1)},
		{"unlogged measurements", command, strings.Replace(command, " -v", "", 1)},
		{"implicit deadline", command, strings.Replace(command, " -timeout 90m", "", 1)},
		{"cached run", command, strings.Replace(command, " -count=1", "", 1)},
		{"no test bodies", command, command + " -run '^$'"},
		{"hidden failure", command, command + " || true"},
		{"commented run", command, "# " + command},
		{"extra package", command, strings.Replace(command, "./ledger", "./ledger ./merkle", 1)},
		{"commented admission", start, "# " + start},
		{"duplicate admission", start, start + "\n" + start},
		{"conditional admission", start, "if false; then\n" + start + "\nfi"},
		{"nested admission", start, "release_phase_unused() {\n" + start + "\n}"},
		{"rehearsal in the gate", start, start + "\nrelease_phase_rehearsal() {\n  go test -tags runtime_rehearsal ./rehearsal/ -count=1\n}\nrelease_gate_start rehearsal release_phase_rehearsal"},
		{"untagged server run", serverCommand, strings.Replace(serverCommand, "-tags scale ", "", 1)},
		{"another server tag", serverCommand, strings.Replace(serverCommand, "-tags scale", "-tags runtime_rehearsal", 1)},
		{"dropped server package", serverCommand, strings.Replace(serverCommand, " ./controller", "", 1)},
		{"narrowed server roots", serverCommand, strings.Replace(serverCommand, "^TestStV2Scale", "^TestStV2ScaleWallets", 1)},
		{"unlogged server measurements", serverCommand, strings.Replace(serverCommand, " -v", "", 1)},
		{"implicit server deadline", serverCommand, strings.Replace(serverCommand, " -timeout 180m", "", 1)},
		{"cached server run", serverCommand, strings.Replace(serverCommand, " -count=1", "", 1)},
		{"hidden server failure", serverCommand, serverCommand + " || true"},
		{"shared services", services, strings.Replace(services, `    source "$RELEASE_GATE_SERVICE_ENV"`+"\n", "", 1)},
		{"unchecked resources", services, strings.Replace(services, `    test_env_validate_suite_resource_manifest "$TEST_ENV_SUITE_RESOURCE_MANIFEST" "$WARP_VAULT_HOME" "$WARP_CONFIG_HOME"`+"\n", "", 1)},
		{"opt-in admission", ready, `if [[ "${RUN_SERVER_SCALE_TESTS:-0}" != "1" || "$release_gate_services_ready" != 1 ]]; then`},
		{"admission without services", ready, `if [[ "$release_gate_services_ready" == 1 ]]; then`},
		{"no unavailable owner", unavailable, ":"},
		{"commented server admission", admitted, "# " + admitted},
		{"duplicate server admission", admitted, admitted + "\n    " + admitted},
		{"nested opt-in", admitted, `if [[ "${RUN_SERVER_SCALE_TESTS:-0}" == "1" ]]; then` + "\n" + admitted + "\nfi"},
	} {
		if strings.Count(script, change.old) != 1 {
			t.Fatalf("%s: the mutation names no unique text", change.name)
		}
		if err := verifyReleaseGateScaleShards(strings.Replace(script, change.old, change.new, 1), sn, server); err == nil {
			t.Fatalf("scale shards accepted %s", change.name)
		}
	}
	early := strings.Replace(script, start, "# admitted before its definition", 1)
	early = strings.Replace(early, "release_phase_sn_scale() {", start+"\nrelease_phase_sn_scale() {", 1)
	if err := verifyReleaseGateScaleShards(early, sn, server); err == nil {
		t.Fatal("scale shard admitted an undefined phase")
	}
	block := "  " + strings.ReplaceAll(releaseGateServiceShardsAdmission, "\n", "\n  ") + "\n"
	block = strings.ReplaceAll(block, "\n  release_gate_start", "\n    release_gate_start")
	if strings.Count(script, block) != 1 {
		t.Fatal("the server scale admission names no unique text")
	}
	const infrastructure = `echo "[release-1.0] Subtensor infrastructure regressions"`
	if strings.Count(script, infrastructure) != 1 {
		t.Fatal("the database block's end names no unique text")
	}
	for name, moved := range map[string]string{
		"admission before the definition":       strings.Replace(strings.Replace(script, block, "", 1), "  release_phase_server_scale() {", block+"  release_phase_server_scale() {", 1),
		"admission outside the database suites": strings.Replace(strings.Replace(script, block, "", 1), infrastructure, releaseGateServiceShardsAdmission+"\n"+infrastructure, 1),
	} {
		if err := verifyReleaseGateScaleShards(moved, sn, server); err == nil {
			t.Fatalf("scale shards accepted the %s", name)
		}
	}
	escaped := maps.Clone(server)
	escaped["./model"] = append(slices.Clone(server["./model"]), "package fixture\nfunc TestOmittedScaleRoot(t *testing.T) {}\n")
	for name, census := range map[string][2]map[string][]string{
		"no sn package":            {nil, server},
		"no server package":        {sn, nil},
		"another sn package":       {map[string][]string{"./ledger": nil, "./validator": nil}, server},
		"another server package":   {sn, map[string][]string{"./api": {"package api\nfunc TestStV2ScaleApi(t *testing.T) {}\n"}, "./controller": server["./controller"], "./model": server["./model"]}},
		"a server root unselected": {sn, escaped},
	} {
		if err := verifyReleaseGateScaleShards(script, census[0], census[1]); err == nil {
			t.Fatalf("scale shards accepted %s", name)
		}
	}
}

// §15 (GT, GA, GL, CL, VA): the sn v2 job runs as pinned.
func TestReleaseGateSettlementV2RunsTheSnSuites(t *testing.T) {
	t.Parallel()
	if err := verifyReleaseGateSnV2(readReleaseGateLocalScript(t), releaseGateSnV2Group(t)); err != nil {
		t.Fatal(err)
	}
}

// §15 (GT, GA, GL, CL, VA): removing a family, a mode, a whole package or the
// unconditional admission fails, as does a root of a v2 test file that
// escapes the selector.
func TestReleaseGateSettlementV2SnSuitesRejectDrift(t *testing.T) {
	t.Parallel()
	script := readReleaseGateLocalScript(t)
	group := releaseGateSnV2Group(t)
	selector, err := releaseConnectPolicySelectorAssignment(script, group.variable)
	if err != nil {
		t.Fatal(err)
	}
	for _, omitted := range strings.Split(group.alternatives, "|") {
		retained := slices.DeleteFunc(strings.Split(group.alternatives, "|"), func(value string) bool { return value == omitted })
		changed := strings.Replace(script, group.variable+"='"+selector+"'", group.variable+"='^Test("+strings.Join(retained, "|")+")'", 1)
		if err := verifyReleaseGateSnV2(changed, group); err == nil {
			t.Fatalf("sn v2 job accepted the removed family %s", omitted)
		}
	}
	const start = "release_gate_start sn-v2 release_phase_sn_v2"
	for _, command := range group.commands {
		for _, replacement := range []string{"# " + command, command + " || true", "if false; then\n  " + command + "\n  fi", strings.Replace(command, " -count=1", "", 1)} {
			if err := verifyReleaseGateSnV2(strings.Replace(script, command, replacement, 1), group); err == nil {
				t.Fatalf("sn v2 job accepted %q", replacement)
			}
		}
	}
	for _, replacement := range []string{
		strings.Replace(group.commands[0], "./ledger ", "", 1),
		strings.Replace(group.commands[0], " ./scripts/solidity-coverage", "", 1),
		strings.Replace(group.commands[0], " -count=1", " -run '^TestLedger' -count=1", 1),
	} {
		if err := verifyReleaseGateSnV2(strings.Replace(script, group.commands[0], replacement, 1), group); err == nil {
			t.Fatalf("sn v2 job accepted %q", replacement)
		}
	}
	for _, replacement := range []string{"# " + start, start + "\n" + start, "if false; then\n" + start + "\nfi"} {
		if err := verifyReleaseGateSnV2(strings.Replace(script, start, replacement, 1), group); err == nil {
			t.Fatalf("sn v2 job accepted the admission %q", replacement)
		}
	}
	for _, packagePath := range group.packages {
		escaped := group
		escaped.sources = maps.Clone(group.sources)
		escaped.sources[packagePath] = append(slices.Clone(group.sources[packagePath]), "package fixture\nfunc TestOmittedSettlementV2Root(t *testing.T) {}\n")
		if err := verifyReleaseGateSnV2(script, escaped); err == nil {
			t.Fatalf("sn v2 job accepted a %s root outside its selector", packagePath)
		}
	}
}

// §15 (GT, GA, CL, VA): at load 27 to 46 the selection's ./miner took 592
// seconds under race and 566 without, and a later race run timed out at Go's
// implicit ten minutes, so both modes keep their explicit 30-minute deadline;
// an implicit, shorter or disabled one fails.
func TestReleaseGateSettlementV2SnSuitesKeepTheirDeadline(t *testing.T) {
	t.Parallel()
	script := readReleaseGateLocalScript(t)
	group := releaseGateSnV2Group(t)
	if err := verifyReleaseGateSnV2(script, group); err != nil {
		t.Fatal(err)
	}
	for _, command := range group.commands[2:] {
		if !strings.HasSuffix(command, " -timeout 30m") || strings.Count(script, command) != 1 {
			t.Fatalf("the gate has no one command %s with its deadline", command)
		}
		for _, replacement := range []string{
			strings.TrimSuffix(command, " -timeout 30m"),
			strings.Replace(command, " -timeout 30m", " -timeout 10m", 1),
			command + " -timeout 0",
		} {
			if err := verifyReleaseGateSnV2(strings.Replace(script, command, replacement, 1), group); err == nil {
				t.Fatalf("sn-v2 accepted %s", replacement)
			}
		}
	}
}

// releaseGateAggregateSkipCensus maps each package under root, as ./dir, to
// the sources of its test files that the default build compiles and that
// name a test either mode of the aggregate job skips. Like go, it skips
// testdata, hidden and underscore directories and nested modules.
func releaseGateAggregateSkipCensus(root string) (map[string][]string, error) {
	census := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path == root {
				return nil
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, "_test.go") {
			return nil
		}
		if matched, err := build.Default.MatchFile(filepath.Dir(path), name); err != nil || !matched {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(source), "TestSettlementV2E2E") && !strings.Contains(string(source), "TestMonitor") && !slices.ContainsFunc(releaseGateLocalModeTestSkips, func(skip releaseGateLocalModeTestSkip) bool { return strings.Contains(string(source), skip.name) }) {
			return err
		}
		relative, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		packagePath := "./" + filepath.ToSlash(relative)
		census[packagePath] = append(census[packagePath], string(source))
		return nil
	})
	return census, err
}

// The aggregate job skips exactly the E suite and the stmonitor fixture's
// roots: its whole body is the full normal run with that skip, and every test
// of sn that the skip names is in ./mainnet and selected by sn-e2e. Local
// mode's skip adds exactly its named tests of ./sim-testnet.
func verifyReleaseGateAggregateE2ESkip(script string, census map[string][]string) error {
	commands, _, err := releaseGatePhaseCommands(script, "sn_all_normal")
	if err != nil {
		return err
	}
	if !slices.Equal(commands, releaseGateAggregateNormalBody) {
		return fmt.Errorf("release_phase_sn_all_normal runs %q, want the full run without the sharded packages and the E suite", commands)
	}
	if strings.Count(script, releaseGateAggregateSkipAssignment) != 1 || strings.Count(script, "release_gate_aggregate_skip=") != 2 {
		return errors.New("the aggregate job's skip is not each mode's exact reviewed assignment")
	}
	selector, err := releaseConnectPolicySelectorAssignment(script, "settlement_v2_e2e_tests")
	if err != nil {
		return err
	}
	selected, err := regexp.Compile(selector)
	if err != nil {
		return err
	}
	fixtureSelector, err := releaseConnectPolicySelectorAssignment(script, "stmonitor_fixture_tests")
	if err != nil {
		return err
	}
	fixtureSelected, err := regexp.Compile(fixtureSelector)
	if err != nil {
		return err
	}
	suite, skip, localSkip := regexp.MustCompile(releaseGateAggregateE2ESkip), regexp.MustCompile(releaseGateAggregatePinnedSkip), regexp.MustCompile(releaseGateAggregateLocalSkip)
	skipped, fixtureSkipped, localOnly := 0, 0, 0
	for _, packagePath := range slices.Sorted(maps.Keys(census)) {
		for _, source := range census[packagePath] {
			names, err := releaseSourceTestDeclarations(source)
			if err != nil {
				return err
			}
			for _, name := range names {
				if skip.MatchString(name) != localSkip.MatchString(name) {
					if skip.MatchString(name) || packagePath != "./sim-testnet" || !slices.ContainsFunc(releaseGateLocalModeTestSkips, func(skip releaseGateLocalModeTestSkip) bool { return skip.name == name }) {
						return fmt.Errorf("the aggregate job's local mode also skips %s of %s", name, packagePath)
					}
					localOnly++
				}
				if !skip.MatchString(name) {
					continue
				}
				if packagePath != "./mainnet" || !selected.MatchString(name) && !fixtureSelected.MatchString(name) {
					return fmt.Errorf("the aggregate job skips %s of %s, which sn-e2e does not run", name, packagePath)
				}
				if suite.MatchString(name) {
					skipped++
				} else {
					fixtureSkipped++
				}
			}
		}
	}
	if skipped == 0 {
		return errors.New("the aggregate job's skip names no test of the E suite")
	}
	if fixtureSkipped != len(releaseGateStmonitorFixtureRoots) {
		return errors.New("the aggregate job does not skip exactly the stmonitor fixture's roots")
	}
	if localOnly != len(releaseGateLocalModeTestSkips) {
		return errors.New("the aggregate job's local mode does not skip exactly its named sim-testnet tests")
	}
	return nil
}

// E-1 to E-7: the aggregate job skips exactly the E suite, which sn-e2e runs.
func TestReleaseGateSettlementV2AggregateSkipsOnlyTheEndToEndSuite(t *testing.T) {
	t.Parallel()
	census, err := releaseGateAggregateSkipCensus("..")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyReleaseGateAggregateE2ESkip(readReleaseGateLocalScript(t), census); err != nil {
		t.Fatal(err)
	}
}

// E-1 to E-7: an aggregate job without the skip, or with a wider one, fails,
// as do a skipped test outside ./mainnet, one that sn-e2e's selector misses,
// and a skip that names no test.
func TestReleaseGateSettlementV2AggregateSkipRejectsDrift(t *testing.T) {
	t.Parallel()
	script := readReleaseGateLocalScript(t)
	census, err := releaseGateAggregateSkipCensus("..")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyReleaseGateAggregateE2ESkip(script, census); err != nil {
		t.Fatal(err)
	}
	const assignment = "settlement_v2_e2e_tests='^Test(SettlementV2E2E)'"
	const pinnedSkip, localSkip = "release_gate_aggregate_skip='" + releaseGateAggregatePinnedSkip + "'", "release_gate_aggregate_skip='" + releaseGateAggregateLocalSkip + "'"
	for _, change := range []struct{ old, new string }{
		{releaseGateAggregateNormalCommand, "go test -parallel=4 -timeout 90m ./... -count=1"},
		{pinnedSkip, "release_gate_aggregate_skip='^TestSettlementV2'"},
		{pinnedSkip, "release_gate_aggregate_skip='" + releaseGateAggregateE2ESkip + "'"},
		{releaseGateAggregateNormalCommand, releaseGateAggregateNormalCommand + " -skip '^TestValidator'"},
		{localSkip, pinnedSkip},
		{localSkip, "release_gate_aggregate_skip='^(TestSettlementV2E2E|TestReleaseLockMatchesCheckout$|TestValidator)'"},
		{`[[ "$release_gate_dependency_mode" == local ]]; then` + "\n  release_gate_aggregate_skip=", `[[ "$release_gate_dependency_mode" == pinned ]]; then` + "\n  release_gate_aggregate_skip="},
		{assignment, "settlement_v2_e2e_tests='^Test(SettlementV2E2EDaemon)'"},
	} {
		if strings.Count(script, change.old) != 1 {
			t.Fatalf("the mutation names no unique text: %s", change.old)
		}
		if err := verifyReleaseGateAggregateE2ESkip(strings.Replace(script, change.old, change.new, 1), census); err == nil {
			t.Fatalf("aggregate skip accepted %q", change.new)
		}
	}
	stray := maps.Clone(census)
	stray["./validator"] = append(slices.Clone(census["./validator"]), "package fixture\nfunc TestSettlementV2E2EStray(t *testing.T) {}\n")
	empty := maps.Clone(census)
	delete(empty, "./mainnet")
	for name, changed := range map[string]map[string][]string{"a skipped test outside ./mainnet": stray, "a skip that names no test": empty} {
		if err := verifyReleaseGateAggregateE2ESkip(script, changed); err == nil {
			t.Fatalf("aggregate skip accepted %s", name)
		}
	}
}

// E-1 to E-7: the end-to-end job runs as pinned.
func TestReleaseGateSettlementV2RunsTheEndToEndSuite(t *testing.T) {
	t.Parallel()
	if err := verifyReleaseGateSnE2E(readReleaseGateLocalScript(t), releaseGateSnE2EGroup(t)); err != nil {
		t.Fatal(err)
	}
}

// E-1 to E-7: a narrowed or emptied selector; a lost, masked, conditional or
// cached mode or one without its deadline; a lost services preamble, resource check or switch to sn; an
// admission lost, duplicated, conditional, unguarded by the services, outside
// the database suites, before its definition or without its unavailable
// owner; or a root of an end-to-end test file that escapes the selector fails.
func TestReleaseGateSettlementV2EndToEndSuiteRejectsDrift(t *testing.T) {
	t.Parallel()
	script := readReleaseGateLocalScript(t)
	group := releaseGateSnE2EGroup(t)
	if err := verifyReleaseGateSnE2E(script, group); err != nil {
		t.Fatal(err)
	}
	const assignment = "settlement_v2_e2e_tests='^Test(SettlementV2E2E)'"
	const start = "release_gate_start sn-e2e release_phase_sn_e2e"
	const unavailable = "release_gate_start sn-e2e release_gate_unavailable_services"
	const ready = `if [[ "$release_gate_services_ready" != 1 ]]; then`
	phase := regexp.MustCompile(`(?ms)^[\t ]*release_phase_sn_e2e\(\) \{\n.*?^[\t ]*\}[\t ]*\n`).FindString(script)
	block := "  " + strings.ReplaceAll(releaseGateServiceShardsAdmission, "\n", "\n  ") + "\n"
	block = strings.ReplaceAll(block, "\n  release_gate_start", "\n    release_gate_start")
	const infrastructure = `echo "[release-1.0] Subtensor infrastructure regressions"`
	type change struct{ old, new string }
	changes := []change{
		{assignment, "settlement_v2_e2e_tests='^Test()'"},
		{assignment, "settlement_v2_e2e_tests='^Test(SettlementV2E2EDaemon)'"},
		{assignment, "settlement_v2_e2e_tests='^TestSettlementV2E2E$'"},
		{start, "# " + start},
		{start, start + "\n    " + start},
		{start, "if false; then\n" + start + "\nfi"},
		{unavailable, ":"},
		{ready, `if [[ "$release_gate_services_ready" == 1 ]]; then`},
		{infrastructure, start + "\n" + infrastructure},
		{phase, ""},
		{block, block + phase},
		{"    " + start + "\n", ""},
	}
	for _, line := range []string{`cd "$workspace/server"`, `source "$RELEASE_GATE_SERVICE_ENV"`, `source "$workspace/server/test-env.sh"`, `test_env_validate_suite_resource_manifest "$TEST_ENV_SUITE_RESOURCE_MANIFEST" "$WARP_VAULT_HOME" "$WARP_CONFIG_HOME"`, `cd "$sn_repo"`} {
		changes = append(changes, change{phase, strings.Replace(phase, "    "+line+"\n", "", 1)})
	}
	for _, command := range group.commands {
		for _, replacement := range []string{"# " + command, command + " || true", "if false; then\n    " + command + "\n    fi", strings.Replace(command, " -count=1", "", 1), strings.Replace(command, " -timeout 30m", "", 1)} {
			changes = append(changes, change{command, replacement})
		}
	}
	for _, change := range changes {
		if strings.Count(script, change.old) != 1 || change.new == change.old {
			t.Fatalf("the mutation names no unique text or changes nothing: %s", change.old)
		}
		if err := verifyReleaseGateSnE2E(strings.Replace(script, change.old, change.new, 1), group); err == nil {
			t.Fatalf("end-to-end job accepted %q", change.new)
		}
	}
	if moved := strings.Replace(strings.Replace(script, phase, "", 1), block, block+phase, 1); verifyReleaseGateSnE2E(moved, group) == nil {
		t.Fatal("end-to-end job accepted an admission before its definition")
	}
	escaped := group
	escaped.sources = map[string][]string{"./mainnet": append(slices.Clone(group.sources["./mainnet"]), "package fixture\nfunc TestOmittedSettlementV2Root(t *testing.T) {}\n")}
	if err := verifyReleaseGateSnE2E(script, escaped); err == nil {
		t.Fatal("end-to-end job accepted a root outside its selector")
	}
}

// Gate 1 (§15 coverage): the coverage phase runs as pinned.
func TestReleaseGateSettlementV2ChecksTheSolidityCoverageBar(t *testing.T) {
	t.Parallel()
	if err := verifyReleaseGateSolidityCoverage(readReleaseGateLocalScript(t)); err != nil {
		t.Fatal(err)
	}
}

// Gate 1 (§15 coverage): each change to the pinned command, its private build,
// the bar's input, the returned status or the admission fails.
func TestReleaseGateSettlementV2CoverageRejectsDrift(t *testing.T) {
	t.Parallel()
	script := readReleaseGateLocalScript(t)
	const check = `go run ./scripts/solidity-coverage evm "$TMPDIR/lcov.info"`
	const start = "release_gate_start solidity-coverage release_phase_solidity_coverage"
	command := releaseGateSolidityCoverageCommand
	for _, change := range []struct{ name, old, new string }{
		{"default mode", command, strings.Replace(command, " --ir-minimum", "", 1)},
		{"summary report", command, strings.Replace(command, "--report lcov", "--report summary", 1)},
		{"unskipped evidence library", command, strings.Replace(command, " --skip STValidatorEvidence.sol", "", 1)},
		{"unskipped storage test", command, strings.Replace(command, " --skip ValidatorEvidenceStorage.t.sol", "", 1)},
		{"unskipped activation test", command, strings.Replace(command, " --skip ValidatorEvidenceActivation.t.sol", "", 1)},
		{"release build directory", command, strings.Replace(command, `FOUNDRY_OUT="$TMPDIR/coverage-out" `, "", 1)},
		{"report in the checkout", command, strings.Replace(command, ` --report-file "$TMPDIR/lcov.info"`, "", 1)},
		{"masked test failure", command, strings.Replace(command, "|| coverage_status=$?", "|| true", 1)},
		{"unchecked bar", check, "# " + check},
		{"another report", check, strings.Replace(check, "$TMPDIR/lcov.info", "lcov.info", 1)},
		{"dropped status", `return "$coverage_status"`, "return 0"},
		{"commented admission", start, "# " + start},
		{"duplicate admission", start, start + "\n" + start},
		{"conditional admission", start, "if false; then\n" + start + "\nfi"},
	} {
		if strings.Count(script, change.old) != 1 {
			t.Fatalf("%s: the mutation names no unique text", change.name)
		}
		if err := verifyReleaseGateSolidityCoverage(strings.Replace(script, change.old, change.new, 1)); err == nil {
			t.Fatalf("coverage phase accepted %s", change.name)
		}
	}
}

func readReleaseGateInvariantSources(t *testing.T) (string, string) {
	t.Helper()
	foundry, err := os.ReadFile("../evm/foundry.toml")
	if err != nil {
		t.Fatal(err)
	}
	invariant, err := os.ReadFile("../evm/test/V2Invariant.t.sol")
	if err != nil {
		t.Fatal(err)
	}
	return string(foundry), string(invariant)
}

// I-1 to I-6: the Solidity phase runs the deep campaign.
func TestReleaseGateSettlementV2RunsTheDeepInvariantCampaign(t *testing.T) {
	t.Parallel()
	foundry, invariant := readReleaseGateInvariantSources(t)
	if err := verifyReleaseGateV2InvariantCampaign(readReleaseGateLocalScript(t), foundry, invariant); err != nil {
		t.Fatal(err)
	}
}

// I-1 to I-6: a missing profile or campaign size, a profile name Foundry's
// inline config ignores, a wider or masked run, a second or misplaced run, or a
// commented one fails.
func TestReleaseGateSettlementV2InvariantCampaignRejectsDrift(t *testing.T) {
	t.Parallel()
	script := readReleaseGateLocalScript(t)
	foundry, invariant := readReleaseGateInvariantSources(t)
	command := releaseGateV2InvariantCommand
	for _, change := range []struct{ name, old, new string }{
		{"hyphenated profile", command, strings.Replace(command, "v2_invariant", "v2-invariant", 1)},
		{"default profile", command, strings.Replace(command, "FOUNDRY_PROFILE=v2_invariant ", "", 1)},
		{"whole suite", command, strings.Replace(command, " --match-path test/V2Invariant.t.sol", "", 1)},
		{"masked failure", command, command + " || true"},
		{"commented campaign", command, "# " + command},
		{"second campaign", command, command + "\n  " + command},
	} {
		if strings.Count(script, change.old) != 1 {
			t.Fatalf("%s: the mutation names no unique text", change.name)
		}
		if err := verifyReleaseGateV2InvariantCampaign(strings.Replace(script, change.old, change.new, 1), foundry, invariant); err == nil {
			t.Fatalf("invariant campaign accepted %s", change.name)
		}
	}
	const campaign = "  " + releaseGateV2InvariantCommand + "\n"
	moved := strings.Replace(script, campaign, "", 1)
	moved = strings.Replace(moved, "  forge build --deny warnings --sizes\n", campaign+"  forge build --deny warnings --sizes\n", 1)
	if strings.Count(script, campaign) != 1 || strings.Count(moved, campaign) != 1 || moved == script {
		t.Fatal("the moved campaign names no unique text")
	}
	if err := verifyReleaseGateV2InvariantCampaign(moved, foundry, invariant); err == nil {
		t.Fatal("invariant campaign accepted a run before the release build and the plain suite")
	}
	for _, change := range []struct{ name, foundry, invariant string }{
		{"no profile", strings.Replace(foundry, "[profile.v2_invariant.invariant]", "[profile.default.invariant.v2]", 1), invariant},
		{"no deep runs", foundry, regexp.MustCompile(`(?m)^/// forge-config: v2_invariant\.invariant\.runs = .*\n`).ReplaceAllString(invariant, "")},
		{"no deep depth", foundry, regexp.MustCompile(`(?m)^/// forge-config: v2_invariant\.invariant\.depth = .*\n`).ReplaceAllString(invariant, "")},
	} {
		if change.foundry == foundry && change.invariant == invariant {
			t.Fatalf("%s: the mutation changes nothing", change.name)
		}
		if err := verifyReleaseGateV2InvariantCampaign(script, change.foundry, change.invariant); err == nil {
			t.Fatalf("invariant campaign accepted %s", change.name)
		}
	}
}

// SV-1 to SV-8 and migrations 821 and 822: the database phase runs every group.
func TestReleaseGateSettlementV2RunsTheOperatorSuites(t *testing.T) {
	t.Parallel()
	script := readReleaseGateLocalScript(t)
	for _, group := range releaseGateOperatorV2Groups(t) {
		if err := verifyReleaseEvidenceV2GateGroup(script, group); err != nil {
			t.Fatalf("%s: %v", group.variable, err)
		}
	}
}

// SV-1 to SV-8 and migrations 821 and 822: removing a family, a mode or the
// admitted phase fails, as does a root of a v2, suffix contract or scale
// database test file that escapes its selector.
func TestReleaseGateSettlementV2OperatorSuitesRejectOmissions(t *testing.T) {
	t.Parallel()
	script := readReleaseGateLocalScript(t)
	for _, group := range releaseGateOperatorV2Groups(t) {
		verifyReleaseGateGroupRejectsOmissions(t, script, group)
	}
}

// SV-1 to SV-8: under load the race run's ./controller reached Go's implicit
// ten-minute deadline, at 600.6 and 601.6 seconds, with normal mode at 509 to
// 521, so both modes keep their explicit 30-minute deadline; an implicit,
// shorter or disabled one fails.
func TestReleaseGateSettlementV2OperatorLedgerSuitesKeepTheirDeadline(t *testing.T) {
	t.Parallel()
	script := readReleaseGateLocalScript(t)
	group := releaseGateLedgerV2ServerGroup(t)
	if err := verifyReleaseEvidenceV2GateGroup(script, group); err != nil {
		t.Fatal(err)
	}
	for _, command := range group.commands {
		if !strings.HasSuffix(command, " -timeout 30m") || strings.Count(script, command) != 1 {
			t.Fatalf("the gate has no one command %s with its deadline", command)
		}
		for _, replacement := range []string{
			strings.TrimSuffix(command, " -timeout 30m"),
			strings.Replace(command, " -timeout 30m", " -timeout 10m", 1),
			command + " -timeout 0",
		} {
			if err := verifyReleaseEvidenceV2GateGroup(strings.Replace(script, command, replacement, 1), group); err == nil {
				t.Fatalf("server-db accepted %s", replacement)
			}
		}
	}
}

// CL-2 and CL-4: the SDK phase runs the v2 ledger display and claims.
func TestReleaseGateSettlementV2RunsTheSdkLedgerSuites(t *testing.T) {
	t.Parallel()
	if err := verifyReleaseEvidenceV2GateGroup(readReleaseGateLocalScript(t), releaseGateSdkLedgerV2Group(t)); err != nil {
		t.Fatal(err)
	}
}

// CL-2 and CL-4: removing a family, a mode or the SDK phase fails, as does a
// root of a v2 test file that escapes the selector.
func TestReleaseGateSettlementV2SdkLedgerSuitesRejectOmissions(t *testing.T) {
	t.Parallel()
	verifyReleaseGateGroupRejectsOmissions(t, readReleaseGateLocalScript(t), releaseGateSdkLedgerV2Group(t))
}
