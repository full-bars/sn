package main

// server-db's native-fee owner runs every server native-fee settlement test,
// in the root package, stnativefees, ./controller and ./model. The public ones
// settle the fixtures that sn-mainnet-exports exports, with the mainnet.test
// image that exported them, which they pin by path and SHA-256. Like a
// ./mainnet shard's owner, it needs that job's exit zero, reads that job's
// environment and runs its tests in both modes, and server-db, which the gate
// admits after it joins the exports job, runs it last.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The server's native-fee settlement test families.
const releaseGateServerNativeFeeSelector = "^Test(StNativeFee|NativeFeeCli|NativeFeeController)"

// The owner's whole body, trimmed.
var releaseGateServerNativeFeeBody = []string{
	`if [[ "${release_gate_mainnet_exports_exit:-}" != 0 ]]; then`,
	`printf 'release gate: server-db has no native-fee exports: sn-mainnet-exports exit %s\n' "${release_gate_mainnet_exports_exit:-unknown}" >&2`,
	`return 1`,
	`fi`,
	`source "$release_gate_mainnet_exports/environment.sh"`,
	`cd "$workspace/server"`,
	"native_fee_settlement_tests='" + releaseGateServerNativeFeeSelector + "'",
	`go test -timeout 40m . ./cli/stnativefees ./controller ./model -run "$native_fee_settlement_tests" -count=1`,
	`go test -race -timeout 40m . ./cli/stnativefees ./controller ./model -run "$native_fee_settlement_tests" -count=1`,
}

// The server's native-fee test sources, whose every test the owner runs.
var releaseGateServerNativeFeeSources = []string{"../../server/st_native_fee_*_test.go", "../../server/cli/stnativefees/*_test.go", "../../server/controller/st_native_fee_*_test.go", "../../server/model/st_native_fee_*_test.go"}

// The owner's body in the script, exported, and called once, as server-db's
// last command; the owner runs every test of its sources.
func verifyReleaseGateServerNativeFeeOwner(script string, sources []string) error {
	definitions := regexp.MustCompile(`(?ms)^  release_gate_server_native_fee\(\) \{\n(.*?)^  \}$`).FindAllStringSubmatch(script, -1)
	if len(definitions) != 1 {
		return fmt.Errorf("the gate has %d native-fee owners", len(definitions))
	}
	var body []string
	for _, line := range strings.Split(definitions[0][1], "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			body = append(body, line)
		}
	}
	if !slices.Equal(body, releaseGateServerNativeFeeBody) {
		return fmt.Errorf("the native-fee owner runs %q, want %q", body, releaseGateServerNativeFeeBody)
	}
	commands, _, err := releaseGatePhaseCommands(script, "server_db")
	if err != nil {
		return err
	}
	calls := regexp.MustCompile(`(?m)^[\t ]*release_gate_server_native_fee[\t ]*$`).FindAllString(script, -1)
	if len(commands) == 0 || commands[len(commands)-1] != "release_gate_server_native_fee" || len(calls) != 1 {
		return errors.New("server-db does not run the native-fee owner once, last")
	}
	exports := regexp.MustCompile(`(?m)^[\t ]*export -f release_gate_server_native_fee[\t ]*$`).FindAllStringIndex(script, -1)
	if start := strings.Index(script, "\n    release_gate_start server-db release_phase_server_db\n"); len(exports) != 1 || start < 0 || exports[0][0] > start {
		return errors.New("the gate does not export the native-fee owner before admitting server-db")
	}
	return verifyReleaseSourceTestCoverage(releaseGateServerNativeFeeSelector, "^Test", sources)
}

// server-db is admitted after the gate joins the exports job, as every
// ./mainnet shard is, both with its private services and without them.
func verifyReleaseGateServerDbAwaitsTheExports(script string) error {
	registry := releaseGateRegistryLines(script)
	await := strings.Index(registry, "\n"+strings.Join(releaseGateMainnetExportsAwait, "\n")+"\n")
	if await < 0 {
		return errors.New("the gate does not join the exports job")
	}
	for _, start := range []string{"\nrelease_gate_start server-db release_phase_server_db\n", "\nrelease_gate_start server-db release_gate_unavailable_services\n"} {
		if index := strings.Index(registry, start); index < await {
			return fmt.Errorf("server-db is admitted before the exports job joins: %q", strings.TrimSpace(start))
		}
	}
	return nil
}

// Run the gate's own native-fee owner after the prelude, in a fixture whose
// exports' environment names its three variables and whose go records each
// call's arguments, directory and native-fee settlement variables.
func runReleaseGateServerNativeFeeOwner(t *testing.T, exit string) (string, string, []string, error) {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{"workspace/server", "exports"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	environment := "export URNETWORK_NATIVE_FEE_SETTLEMENT_FIXTURE_DIRECTORY=" + filepath.Join(root, "exports", "native-fee") + "\n" +
		"export URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER=" + filepath.Join(root, "exports", "native-fee-verifier", "mainnet.test") + "\n" +
		"export URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER_SHA256=sha256:" + strings.Repeat("ab", 32) + "\n"
	if err := os.WriteFile(filepath.Join(root, "exports", "environment.sh"), []byte(environment), 0o600); err != nil {
		t.Fatal(err)
	}
	fragment := releaseGateLocalFragment(t, "  release_gate_server_native_fee() {", "  export -f release_gate_server_native_fee")
	prelude := "fixture=" + root + "\nworkspace=\"$fixture/workspace\"\nrelease_gate_mainnet_exports=\"$fixture/exports\"\n" + exit + "\n" +
		"go() { printf '%s|%s|%s\\n' \"$*\" \"$PWD\" \"$(env | grep '^URNETWORK_NATIVE_FEE_SETTLEMENT_' | LC_ALL=C sort | tr '\\n' ' ')\" >> \"$fixture/calls\"; }\n"
	command := exec.Command("bash", "-c", "set -euo pipefail\n"+prelude+fragment+"\nrelease_gate_server_native_fee")
	output, err := command.CombinedOutput()
	raw, readErr := os.ReadFile(filepath.Join(root, "calls"))
	var calls []string
	if readErr == nil {
		calls = strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	} else if !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return root, string(output), calls, err
}

// The gate's native-fee owner is its one reviewed body, which server-db
// exports and runs last, and it runs every test of the server's native-fee
// settlement sources, the public ones among them; a changed body or a lost
// call is refused.
func TestReleaseGateServerNativeFeeOwnerRunsEveryNativeFeeTest(t *testing.T) {
	t.Parallel()
	script := readReleaseGateLocalScript(t)
	var sources []string
	for _, pattern := range releaseGateServerNativeFeeSources {
		paths, err := filepath.Glob(pattern)
		if err != nil || len(paths) == 0 {
			t.Fatalf("native-fee sources %s: %d files, %v", pattern, len(paths), err)
		}
		for _, path := range paths {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			sources = append(sources, string(raw))
		}
	}
	for _, public := range []string{"TestNativeFeeControllerPublicProofSettlesWithoutSignerConfiguration", "TestNativeFeeControllerPublicUnknownRefundKeepsOriginalCeiling", "TestStNativeFeeSettlementPublicOwnedProofRetainsAllOriginals", "TestStNativeFeeSettlementPublicMissingRefundKeepsFullCeiling", "TestStNativeFeeSettlementPublicProofRecoversLostSignerResult", "TestStNativeFeeSettlementPublicFractionalConflictRevokesCredit"} {
		if declared, err := releaseSelectedTestDeclarations("^"+public+"$", sources); err != nil || len(declared) != 1 {
			t.Fatalf("the native-fee sources do not declare %s: %v", public, err)
		}
	}
	if err := verifyReleaseGateServerNativeFeeOwner(script, sources); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct{ old, new string }{
		{"\n    release_gate_server_native_fee\n  }\n", "\n  }\n"},
		{"\n    release_gate_server_native_fee\n  }\n", "\n    release_gate_server_native_fee || true\n  }\n"},
		{"\n  export -f release_gate_server_native_fee\n", "\n"},
		{"\n    source \"$release_gate_mainnet_exports/environment.sh\"\n    cd \"$workspace/server\"\n", "\n    cd \"$workspace/server\"\n"},
		{"\n    go test -race -timeout 40m . ./cli/stnativefees ./controller ./model -run \"$native_fee_settlement_tests\" -count=1\n", "\n"},
		{"native_fee_settlement_tests='^Test(StNativeFee|NativeFeeCli|NativeFeeController)'", "native_fee_settlement_tests='^Test(StNativeFee|NativeFeeController)'"},
	} {
		if strings.Count(script, change.old) != 1 {
			t.Fatalf("the mutation names no unique text: %q", change.old)
		}
		if verifyReleaseGateServerNativeFeeOwner(strings.Replace(script, change.old, change.new, 1), sources) == nil {
			t.Fatalf("the native-fee owner accepted %q", change.new)
		}
	}
	if verifyReleaseGateServerNativeFeeOwner(script, append(slices.Clone(sources), "package model\nfunc TestNativeFeeSettlementUnselected(t *testing.T) {}\n")) == nil {
		t.Fatal("the native-fee owner accepted a native-fee test it does not run")
	}
}

// With the exports job's exit zero, the owner runs the native-fee settlement
// tests normal and race in the server's checkout, with exactly the exports'
// native-fee settlement variables.
func TestReleaseGateServerNativeFeeOwnerRunsWithTheExports(t *testing.T) {
	t.Parallel()
	root, output, calls, err := runReleaseGateServerNativeFeeOwner(t, "release_gate_mainnet_exports_exit=0")
	if err != nil {
		t.Fatalf("the owner failed: %v\n%s", err, output)
	}
	environment := "URNETWORK_NATIVE_FEE_SETTLEMENT_FIXTURE_DIRECTORY=" + filepath.Join(root, "exports", "native-fee") +
		" URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER=" + filepath.Join(root, "exports", "native-fee-verifier", "mainnet.test") +
		" URNETWORK_NATIVE_FEE_SETTLEMENT_VERIFIER_SHA256=sha256:" + strings.Repeat("ab", 32) + " "
	server := filepath.Join(root, "workspace", "server")
	want := []string{
		"test -timeout 40m . ./cli/stnativefees ./controller ./model -run " + releaseGateServerNativeFeeSelector + " -count=1|" + server + "|" + environment,
		"test -race -timeout 40m . ./cli/stnativefees ./controller ./model -run " + releaseGateServerNativeFeeSelector + " -count=1|" + server + "|" + environment,
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("the owner ran\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// Without the exports job's exit zero, the owner fails, naming that exit,
// before it reads the exports or runs a test.
func TestReleaseGateServerNativeFeeOwnerFailsWithoutTheExports(t *testing.T) {
	t.Parallel()
	for exit, named := range map[string]string{"release_gate_mainnet_exports_exit=9": "9", "release_gate_mainnet_exports_exit=": "unknown", "unset release_gate_mainnet_exports_exit": "unknown"} {
		_, output, calls, err := runReleaseGateServerNativeFeeOwner(t, exit)
		if err == nil || output != "release gate: server-db has no native-fee exports: sn-mainnet-exports exit "+named+"\n" || len(calls) != 0 {
			t.Fatalf("%s: the owner ran %q: %v\n%s", exit, calls, err, output)
		}
	}
}

// server-db, with or without its private services, is admitted only after the
// gate joins the exports job, so the owner's exit check reads that job's exit.
func TestReleaseGateServerDbIsAdmittedAfterTheExportsJoin(t *testing.T) {
	t.Parallel()
	script := readReleaseGateLocalScript(t)
	if err := verifyReleaseGateServerDbAwaitsTheExports(script); err != nil {
		t.Fatal(err)
	}
	await := strings.Join(releaseGateMainnetExportsAwait, "\n") + "\n"
	if strings.Count(script, await) != 1 {
		t.Fatal("the gate does not join the exports job once")
	}
	moved := strings.Replace(script, await, "", 1) + await
	if verifyReleaseGateServerDbAwaitsTheExports(moved) == nil {
		t.Fatal("server-db was accepted before the exports job joins")
	}
}
