package main

// Local mode's skips: each printed first with its reason, each named test
// skipped in every job that would run it, and each still a test of its
// package, so a renamed test fails here instead of escaping the skip.

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type releaseGateLocalModeTestSkip struct{ name, reason string }

// The sim-testnet tests local mode skips by name, with the reasons it prints.
var releaseGateLocalModeTestSkips = []releaseGateLocalModeTestSkip{
	{"TestReleaseLockMatchesCheckout", "only a sealed run's checkouts match the release lock"},
	{"TestRepositoryDiscoveryUsesModuleIdentity", "it stats the workspace vault's main/st.yml"},
}

// Local mode's selection prints everything it skips, before anything runs.
var releaseGateLocalModeSelectionLines = []string{
	"[release-1.0] dependency mode local: the working checkouts as they are, not sealed",
	"[release-1.0] source freeze provenance skipped (local mode): it checks the working checkouts' module inventory and tidiness, not that they are clean, fetched and at origin",
	"[release-1.0] runtime source and metadata artifact attestations skipped (local mode): they read the published runtime source and the testnet archive node, not the working checkouts",
	"[release-1.0] final release-lock and source-freeze checkouts skipped (local mode): only a sealed run's checkouts pass them",
	"[release-1.0] vault checkout skipped (local mode): local mode never reads it",
	"[release-1.0] TestReleaseLockMatchesCheckout skipped (local mode): only a sealed run's checkouts match the release lock",
	"[release-1.0] TestRepositoryDiscoveryUsesModuleIdentity skipped (local mode): it stats the workspace vault's main/st.yml",
}

// One go test command that can run ./sim-testnet's tests, with its -run and
// -skip as one mode evaluates them; nil selects every test or skips none.
type releaseGateSimulatorCommand struct {
	line      string
	run, skip *regexp.Regexp
}

func (self releaseGateSimulatorCommand) runs(name string) bool {
	return (self.run == nil || self.run.MatchString(name)) && (self.skip == nil || !self.skip.MatchString(name))
}

// A selector variable's value in one mode: its one assignment, or the
// reviewed pinned/local pair that the mode chooses between.
func releaseGateModeSelector(script, variable, mode string) (string, error) {
	assignment := regexp.MustCompile(`(?m)^[\t ]*` + regexp.QuoteMeta(variable) + `='([^'\n]*)'[\t ]*$`)
	values := assignment.FindAllStringSubmatch(script, -1)
	switch len(values) {
	case 1:
		return values[0][1], nil
	case 2:
		pair := variable + "='" + values[0][1] + "'\nif [[ \"$release_gate_dependency_mode\" == local ]]; then\n  " + variable + "='" + values[1][1] + "'\nfi\n"
		if strings.Count(script, pair) != 1 {
			return "", fmt.Errorf("%s has two assignments outside one mode choice", variable)
		}
		if mode == "local" {
			return values[1][1], nil
		}
		return values[0][1], nil
	}
	return "", fmt.Errorf("%s has %d assignments", variable, len(values))
}

// Every go test command of the gate that runs tests of ./sim-testnet, in sn,
// as one mode runs them. A command in the branch of a mode check that the
// mode does not take is not run; every other command counts as run. The
// simulator's shards of each mode together count as one command, which runs
// every test but their named skips, with the aggregate skip of the shard
// owner's one reviewed test run.
func releaseGateSimulatorCommands(script, mode string) ([]releaseGateSimulatorCommand, error) {
	const check = `if [[ "$release_gate_dependency_mode" == local ]]; then`
	if err := verifyReleaseGateShardOwner(script); err != nil {
		return nil, err
	}
	type branch struct{ mode, taken bool }
	var branches []branch
	var commands []releaseGateSimulatorCommand
	flag := regexp.MustCompile(` -(run|skip) ('([^']*)'|"\$([A-Za-z_][A-Za-z0-9_]*)")`)
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "if ") && strings.HasSuffix(line, "; then"):
			branches = append(branches, branch{mode: line == check, taken: mode == "local"})
			continue
		case line == "else":
			if len(branches) == 0 {
				return nil, errors.New("the gate has an unowned else")
			}
			branches[len(branches)-1].taken = !branches[len(branches)-1].taken
			continue
		case strings.HasPrefix(line, "elif ") && strings.HasSuffix(line, "; then"):
			if len(branches) == 0 || branches[len(branches)-1].mode {
				return nil, errors.New("the gate's mode census reads no elif of a mode check")
			}
			continue
		case line == "fi":
			if len(branches) == 0 {
				return nil, errors.New("the gate has an unowned fi")
			}
			branches = branches[:len(branches)-1]
			continue
		}
		skipped := slices.ContainsFunc(branches, func(entry branch) bool { return entry.mode && !entry.taken })
		if line == releaseGateSimulatorNormalShards.ownerCommand() || line == releaseGateSimulatorRaceShards.ownerCommand() {
			if skipped {
				continue
			}
			skip, err := releaseGateModeSelector(script, "release_gate_aggregate_skip", mode)
			if err != nil {
				return nil, err
			}
			names, err := releaseGateModeSelector(script, releaseGateSimulatorNormalShards.skipVariable, mode)
			if err != nil {
				return nil, err
			}
			if names != "" {
				skip = "(" + skip + ")|^(" + strings.Join(strings.Fields(names), "|") + ")$"
			}
			compiled, err := regexp.Compile(skip)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", line, err)
			}
			commands = append(commands, releaseGateSimulatorCommand{line: line, skip: compiled})
			continue
		}
		if !strings.HasPrefix(line, "go test ") || strings.Contains(line, " -list ") {
			continue
		}
		fields := strings.Fields(line)
		if !slices.Contains(fields, "./sim-testnet") && !slices.Contains(fields, "./...") {
			continue
		}
		if skipped {
			continue
		}
		command := releaseGateSimulatorCommand{line: line}
		for _, match := range flag.FindAllStringSubmatch(line, -1) {
			value := match[3]
			if match[4] != "" {
				var err error
				if value, err = releaseGateModeSelector(script, match[4], mode); err != nil {
					return nil, err
				}
			}
			compiled, err := regexp.Compile(value)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", line, err)
			}
			if match[1] == "run" {
				command.run = compiled
			} else {
				command.skip = compiled
			}
		}
		if strings.Count(line, " -run ") > 1 || strings.Count(line, " -skip ") > 1 {
			return nil, fmt.Errorf("%s repeats a selector flag", line)
		}
		commands = append(commands, command)
	}
	if len(branches) != 0 {
		return nil, errors.New("the gate has an unclosed if")
	}
	return commands, nil
}

// The top-level tests that ./sim-testnet's default build compiles.
func releaseGateSimulatorTestRoots(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("*_test.go")
	if err != nil || len(paths) == 0 {
		t.Fatalf("sim-testnet test census: %d files, %v", len(paths), err)
	}
	var roots []string
	for _, path := range paths {
		if matched, err := build.Default.MatchFile(".", path); err != nil || !matched {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || !strings.HasPrefix(function.Name.Name, "Test") || function.Name.Name == "TestMain" || function.Type.Params == nil || len(function.Type.Params.List) != 1 {
				continue
			}
			if pointer, ok := function.Type.Params.List[0].Type.(*ast.StarExpr); ok {
				if parameter, ok := pointer.X.(*ast.SelectorExpr); ok && parameter.Sel.Name == "T" {
					roots = append(roots, function.Name.Name)
				}
			}
		}
	}
	slices.Sort(roots)
	return roots
}

// Local mode drops exactly its named tests from the jobs, every one of them a
// test of ./sim-testnet that pinned mode still runs, and prints each of them.
func verifyReleaseGateLocalModeSkips(script string, roots []string) error {
	pinned, err := releaseGateSimulatorCommands(script, "pinned")
	if err != nil {
		return err
	}
	local, err := releaseGateSimulatorCommands(script, "local")
	if err != nil {
		return err
	}
	var dropped []string
	for _, root := range roots {
		inPinned := slices.ContainsFunc(pinned, func(command releaseGateSimulatorCommand) bool { return command.runs(root) })
		inLocal := slices.ContainsFunc(local, func(command releaseGateSimulatorCommand) bool { return command.runs(root) })
		if inLocal && !inPinned {
			return fmt.Errorf("local mode runs %s, which pinned mode does not", root)
		}
		if inPinned && !inLocal {
			dropped = append(dropped, root)
		}
	}
	var want []string
	for _, skip := range releaseGateLocalModeTestSkips {
		if !slices.Contains(roots, skip.name) {
			return fmt.Errorf("local mode skips %s, which is no test of ./sim-testnet", skip.name)
		}
		for _, command := range local {
			if command.runs(skip.name) {
				return fmt.Errorf("local mode still runs %s in %s", skip.name, command.line)
			}
		}
		line := "  echo \"[release-1.0] " + skip.name + " skipped (local mode): " + skip.reason + "\"\n"
		if strings.Count(script, line) != 1 {
			return fmt.Errorf("local mode does not print %s's skip with its reason", skip.name)
		}
		want = append(want, skip.name)
	}
	slices.Sort(want)
	if !slices.Equal(dropped, want) {
		return fmt.Errorf("local mode drops %q, want exactly its named tests %q", dropped, want)
	}
	return nil
}

// Local mode's selection prints its complete skip list, each with its reason,
// before test storage, the job owner or any preflight exists.
func TestReleaseGateLocalModePrintsEverySkipFirst(t *testing.T) {
	t.Parallel()
	output, stderr, err := runReleaseGateModeSelection(t, "RELEASE_GATE_DEPENDENCY_MODE=local")
	if want := strings.Join(releaseGateLocalModeSelectionLines, "\n") + "\nselected local\n"; err != nil || output != want || stderr != "" {
		t.Fatalf("local mode selection = %q %q %v, want %q", output, stderr, err, want)
	}
	script := readReleaseGateLocalScript(t)
	if print, storage := strings.Index(script, releaseGateLocalModeSelectionLines[1]), strings.Index(script, "sn_test_storage_init\n"); print < 0 || storage < print {
		t.Fatal("local mode prints its skips after the gate starts its work")
	}
	for _, skip := range releaseGateLocalModeTestSkips {
		if !slices.Contains(releaseGateLocalModeSelectionLines, "[release-1.0] "+skip.name+" skipped (local mode): "+skip.reason) {
			t.Fatalf("local mode's printed list omits %s", skip.name)
		}
	}
	pinned, _, err := runReleaseGateModeSelection(t, "RELEASE_GATE_DEPENDENCY_MODE=pinned")
	if err != nil || strings.Contains(pinned, "skipped") {
		t.Fatalf("pinned mode prints a skip: %q %v", pinned, err)
	}
}

// No job runs a test that local mode skips, in the simulator's shards of either
// mode or anywhere else, while pinned mode still runs each of them.
func TestReleaseGateLocalModeRunsNoSkippedTestInAnyJob(t *testing.T) {
	t.Parallel()
	script := readReleaseGateLocalScript(t)
	roots := releaseGateSimulatorTestRoots(t)
	if err := verifyReleaseGateLocalModeSkips(script, roots); err != nil {
		t.Fatal(err)
	}
	local, err := releaseGateSimulatorCommands(script, "local")
	if err != nil {
		t.Fatal(err)
	}
	for _, shards := range []releaseGateShards{releaseGateSimulatorNormalShards, releaseGateSimulatorRaceShards} {
		if !slices.ContainsFunc(local, func(command releaseGateSimulatorCommand) bool { return command.line == shards.ownerCommand() }) {
			t.Fatalf("the census of sim-testnet commands omits the simulator's %s shards", shards.mode)
		}
	}
	for _, change := range []struct{ old, new string }{
		{"|TestRepositoryDiscoveryUsesModuleIdentity$)'", ")'"},
		{"|TestReleaseLockMatchesCheckout$|TestRepositoryDiscoveryUsesModuleIdentity$)'", ")'"},
		{releaseGateShardCommand, strings.Replace(releaseGateShardCommand, ` -skip "$release_gate_aggregate_skip"`, "", 1)},
		{releaseGateValidatorRaceShards.ownerCommand(), releaseGateValidatorRaceShards.ownerCommand() + "\n  go test ./sim-testnet -run '^TestRepositoryDiscovery' -count=1"},
		{"  echo \"[release-1.0] TestRepositoryDiscoveryUsesModuleIdentity skipped (local mode): it stats the workspace vault's main/st.yml\"\n", ""},
	} {
		if strings.Count(script, change.old) != 1 {
			t.Fatalf("the mutation names no unique text: %q", change.old)
		}
		if err := verifyReleaseGateLocalModeSkips(strings.Replace(script, change.old, change.new, 1), roots); err == nil {
			t.Fatalf("local mode accepted %q", change.new)
		}
	}
	// A fence that runs in local mode, outside its mode check, runs the test.
	fence := "    go test ./sim-testnet -run '^TestReleaseLockMatchesCheckout$' -count=1\n"
	if strings.Count(script, fence) != 1 || verifyReleaseGateLocalModeSkips(strings.Replace(script, "echo 'release gate: source fences cannot precede an unproven child join' >&2\n", "echo 'release gate: source fences cannot precede an unproven child join' >&2\n"+fence, 1), roots) == nil {
		t.Fatal("local mode accepted the release-lock fence outside its mode check")
	}
}

// Every name local mode skips is still a test of ./sim-testnet: a renamed or
// removed test fails here rather than leaving its skip naming nothing.
func TestReleaseGateLocalModeSkipsOnlyDeclaredTests(t *testing.T) {
	t.Parallel()
	script := readReleaseGateLocalScript(t)
	roots := releaseGateSimulatorTestRoots(t)
	for _, skip := range releaseGateLocalModeTestSkips {
		if !slices.Contains(roots, skip.name) {
			t.Fatalf("local mode skips %s, which is no test of ./sim-testnet", skip.name)
		}
		renamed := slices.DeleteFunc(slices.Clone(roots), func(root string) bool { return root == skip.name })
		renamed = append(renamed, skip.name+"Renamed")
		slices.Sort(renamed)
		if err := verifyReleaseGateLocalModeSkips(script, renamed); err == nil {
			t.Fatalf("local mode accepted a skip of the renamed %s", skip.name)
		}
	}
	source, err := os.ReadFile("config_test.go")
	if err != nil || !strings.Contains(string(source), "\nfunc TestRepositoryDiscoveryUsesModuleIdentity(t *testing.T) {\n") || !strings.Contains(string(source), "\nfunc TestRepositoryDiscoveryDoesNotRequireOperatorProxy(t *testing.T) {\n") {
		t.Fatalf("the vault-discovery test or its synthetic counterpart moved: %v", err)
	}
	if !slices.Contains(roots, "TestRepositoryDiscoveryDoesNotRequireOperatorProxy") {
		t.Fatal("discovery lost its synthetic-workspace test")
	}
	local, err := releaseGateSimulatorCommands(script, "local")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(local, func(command releaseGateSimulatorCommand) bool {
		return command.runs("TestRepositoryDiscoveryDoesNotRequireOperatorProxy")
	}) {
		t.Fatal("local mode skips discovery's synthetic-workspace test too")
	}
}
