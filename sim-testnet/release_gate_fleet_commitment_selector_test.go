package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The tagged fleet format replaced the runtime-specific encoding regression.
// Both producer commands must select its current encoding and zero-hash tests;
// restoring the obsolete alternative must still fail the strict census.
func TestReleaseGateSelectorCensusCoversCurrentFleetCommitmentInfo(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../scripts/test-release-1.0-producer-gate.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	commands, err := releaseGateRunCommands(script)
	if err != nil {
		t.Fatal(err)
	}
	const run = `"$runtime_client_tests"`
	var runtimeCommands []releaseGateRunCommand
	for _, command := range commands {
		if command.run == run {
			runtimeCommands = append(runtimeCommands, command)
		}
	}
	wantCommands := []string{
		`go test ./crv4 ./miner ./validator -run "$runtime_client_tests" -count=1`,
		`go test -race ./crv4 ./miner ./validator -run "$runtime_client_tests" -count=1`,
	}
	if len(runtimeCommands) != len(wantCommands) {
		t.Fatalf("runtime-client commands = %d, want normal and race", len(runtimeCommands))
	}
	values, err := releaseGateRunSelectorValues(script, run)
	if err != nil || len(values) != 1 {
		t.Fatalf("runtime-client selector values = %q: %v", values, err)
	}
	selected, err := regexp.Compile(values[0])
	if err != nil {
		t.Fatal(err)
	}
	for index, command := range runtimeCommands {
		if command.text != wantCommands[index] {
			t.Fatalf("runtime-client command %d = %q, want %q", index, command.text, wantCommands[index])
		}
		names, directories, err := releaseGateRunCommandTests(command, releaseGateCheckoutTestCensus)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(directories, []string{"../crv4", "../miner", "../validator"}) {
			t.Fatalf("runtime-client directories = %q", directories)
		}
		for _, name := range []string{"TestFleetCommitmentInfoIsTaggedRaw40", "TestFleetCommitmentInfoRejectsZero"} {
			if !slices.Contains(names, name) {
				t.Fatalf("runtime-client packages no longer declare %s", name)
			}
			if !selected.MatchString(name) {
				t.Errorf("line %d: runtime-client selector omits declared regression %s", command.line, name)
			}
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	if err := verifyReleaseGateRunSelector(script, run, runtimeCommands, releaseGateCheckoutTestCensus); err != nil {
		t.Fatalf("current runtime-client selector fails the census: %v", err)
	}
	const current, obsolete = "|FleetCommitmentInfo|", "|FleetCommitmentInfoRuntime|"
	if strings.Count(script, current) != 1 {
		t.Fatal("the producer gate no longer assigns the current fleet commitment family exactly once")
	}
	mutated := strings.Replace(script, current, obsolete, 1)
	err = verifyReleaseGateRunSelector(mutated, run, runtimeCommands, releaseGateCheckoutTestCensus)
	if err == nil || !strings.Contains(err.Error(), "its alternative ^TestFleetCommitmentInfoRuntime selects no test of ../crv4 ../miner ../validator") {
		t.Fatalf("the census accepted the obsolete runtime-specific fleet commitment family: %v", err)
	}
}
