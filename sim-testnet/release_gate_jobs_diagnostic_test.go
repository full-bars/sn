package main

// Exercises the real gate owner and source fences. Only disposable fixture
// files, machine capacity and the external service boundary are supplied here.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A small source fixture admits the real diagnostic initializer without a
// second checkout, a replacement gate command, or any live service authority.
type releaseGateDiagnosticFixture struct {
	root         string
	workspace    string
	helper       string
	sn           string
	sourcePath   string
	hashManifest string
	modeManifest string
}

// Both manifests describe the same actual private file; mutations are made
// only after retaining these original bytes and modes.
func newReleaseGateDiagnosticFixture(t *testing.T) *releaseGateDiagnosticFixture {
	t.Helper()
	root := t.TempDir()
	self := &releaseGateDiagnosticFixture{
		root:         root,
		workspace:    filepath.Join(root, "workspace"),
		hashManifest: filepath.Join(root, "source.sha256"),
		modeManifest: filepath.Join(root, "source.modes"),
	}
	var err error
	self.helper, err = filepath.Abs("../scripts/release-gate-jobs.sh")
	if err != nil {
		t.Fatal(err)
	}
	self.sn, err = filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(self.workspace, "server", "local"), 0o700); err != nil {
		t.Fatal(err)
	}
	cleanup := "release_gate_services_cleanup() { printf 'cleaned\\n' > \"$RELEASE_GATE_DIAGNOSTIC_FIXTURE_ROOT/cleaned\"; }\n"
	if err := os.WriteFile(filepath.Join(self.workspace, "server", "local", "release-gate-services.sh"), []byte(cleanup), 0o600); err != nil {
		t.Fatal(err)
	}
	self.sourcePath = filepath.Join(self.workspace, "source.txt")
	source := []byte("synthetic immutable source\n")
	if err := os.WriteFile(self.sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self.hashManifest, []byte(fmt.Sprintf("%x  source.txt\n", sha256.Sum256(source))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self.modeManifest, []byte(fmt.Sprintf("600 %d source.txt\n", len(source))), 0o600); err != nil {
		t.Fatal(err)
	}
	return self
}

// The deadline is a deadlock backstop; every asserted transition is a real
// process exit, retained source witness or explicit queue barrier.
func (self *releaseGateDiagnosticFixture) run(t *testing.T, body string, overrides ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", "-c", `set -euo pipefail
source "$1"
sn_repo="$2"; workspace="$3"
release_gate_effective_resources() { RELEASE_GATE_CPUS=24; RELEASE_GATE_MEMORY=34359738368; }
release_gate_jobs_init
`+body, "diagnostic-owner-test", self.helper, self.sn, self.workspace)
	command.Env = append(os.Environ(),
		"TMPDIR="+self.root,
		"RELEASE_GATE_DIAGNOSTIC_FIXTURE_ROOT="+self.root,
		"RELEASE_GATE_DIAGNOSTIC_FIXTURE_HELPER="+self.helper,
		"RELEASE_GATE_DIAGNOSTIC=1",
		"RELEASE_GATE_DIAGNOSTIC_SOURCE_MANIFEST="+self.hashManifest,
		"RELEASE_GATE_DIAGNOSTIC_MODE_MANIFEST="+self.modeManifest,
		"RUN_SERVER_DB_TESTS=1", "SIM_TESTNET_LIVE_DEPENDENCIES=0",
		"RELEASE_GATE_JOBS=2", "RELEASE_GATE_CONCURRENT_GATES=2")
	command.Env = append(command.Env, overrides...)
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = releaseGateQueueFixtureWaitDelay
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("diagnostic fixture did not join: %v\n%s", ctx.Err(), output)
	}
	if command.ProcessState == nil {
		t.Fatalf("diagnostic fixture could not start: %v\n%s", err, output)
	}
	return command.ProcessState.ExitCode(), string(output)
}

// The owner creates exactly one root; a nested-owner test inspects its own
// explicit marker instead of treating a second root as the parent.
func releaseGateDiagnosticRoot(t *testing.T, root string) string {
	t.Helper()
	roots, err := filepath.Glob(filepath.Join(root, "urnetwork-release-gate.*"))
	if err != nil || len(roots) != 1 {
		t.Fatalf("diagnostic owner roots=%v: %v", roots, err)
	}
	return roots[0]
}

// Retained witnesses are exact bytes, not a success inferred from console text.
func releaseGateDiagnosticRead(t *testing.T, path string) string {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// Reproduces the real missing-checkout refusal through each entry script's
// actual initial preflight block. The default must never admit its next body.
func TestReleaseGateJobsDiagnosticStrictDefaultRetainsOriginalSourceRefusal(t *testing.T) {
	for _, name := range []string{"test-release-1.0-local.sh", "test-release-1.0-producer-gate.sh"} {
		encoded, err := os.ReadFile(filepath.Join("..", "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		source := string(encoded)
		start := strings.Index(source, "# The ordinary entry stays strict")
		end := strings.Index(source, "\nif ! command -v forge")
		if start < 0 || end <= start {
			t.Fatalf("%s has no complete initial preflight block", name)
		}
		fixture := newReleaseGateJobsFixture(t, source[start:end]+"\nprintf 'body-admitted\\n' > \"$RELEASE_GATE_QUEUE_FIXTURE_ROOT/body-admitted\"\n")
		fixture.join(t, 1)
		root := releaseGateDiagnosticRoot(t, fixture.root)
		if got := releaseGateDiagnosticRead(t, filepath.Join(root, "preflights", "source-freeze", "exit")); got != "1\n" {
			t.Fatalf("%s original refusal=%q", name, got)
		}
		if got := releaseGateDiagnosticRead(t, filepath.Join(root, "preflights", "source-freeze", "stderr")); !strings.Contains(got, "release repository is missing or not a Git checkout") {
			t.Fatalf("%s lost original source refusal: %s", name, got)
		}
		if _, err := os.Stat(filepath.Join(fixture.root, "body-admitted")); !os.IsNotExist(err) {
			t.Fatalf("%s admitted a body after its strict refusal: %v", name, err)
		}
		if strings.Contains(fixture.output.String(), "release gate passed") {
			t.Fatalf("%s qualified a refused source", name)
		}
	}
}

// A failed preflight and failed phase cannot hide each other or prevent an
// already admitted independent phase from completing and joining its owner.
func TestReleaseGateJobsDiagnosticRetainsRefusalsAndJoinsIndependentBodies(t *testing.T) {
	fixture := newReleaseGateJobsFixture(t, `
release_gate_diagnostic=1; export release_gate_diagnostic
printf '%s\n' "$release_gate_root" > "$RELEASE_GATE_QUEUE_FIXTURE_ROOT/events"
read -r release < "$RELEASE_GATE_QUEUE_FIXTURE_ROOT/control-c"
release_gate_diagnostic_fence initial
diagnostic_preflights() {
  release_gate_record_preflight refused bash -c 'printf "original refusal\n" >&2; exit 17' || release_gate_preflight_refusal "$?"
  release_gate_record_preflight independent bash -c 'printf "independent preflight\n"' || release_gate_preflight_refusal "$?"
  release_gate_preflight_status
}
phase_a() { printf 'a\n' > "$RELEASE_GATE_QUEUE_FIXTURE_ROOT/events"; read -r release < "$RELEASE_GATE_QUEUE_FIXTURE_ROOT/control-a"; return 23; }
phase_b() { printf 'b\n' > "$RELEASE_GATE_QUEUE_FIXTURE_ROOT/events"; read -r release < "$RELEASE_GATE_QUEUE_FIXTURE_ROOT/control-b"; }
release_gate_start diagnostic-preflights diagnostic_preflights
release_gate_start a phase_a
release_gate_start b phase_b
release_gate_wait_one
[[ "$release_gate_body_result" == 23 && "$release_gate_active" == 1 ]]
printf 'failed-body-joined\n' > "$RELEASE_GATE_QUEUE_FIXTURE_ROOT/events"
status=0; release_gate_complete || status=$?
[[ "$release_gate_active" == 0 && "$release_gate_services_cleaned" == 1 && "$status" == 17 ]]
diagnostic_status=0; release_gate_diagnostic_finish "$status" 1 || diagnostic_status=$?
exit "$diagnostic_status"
`)
	root := fixture.line(t, "events")
	source := []byte("synthetic bounded source\n")
	if err := os.WriteFile(filepath.Join(fixture.root, "workspace", "source.txt"), source, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.sha256"), []byte(fmt.Sprintf("%x  source.txt\n", sha256.Sum256(source))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.modes"), []byte(fmt.Sprintf("600 %d source.txt\n", len(source))), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.write(t, "control-c", "release")
	first, second := fixture.line(t, "events"), fixture.line(t, "events")
	if !((first == "a" && second == "b") || (first == "b" && second == "a")) {
		t.Fatalf("independent phases did not both enter: %q %q", first, second)
	}
	fixture.write(t, "control-a", "release")
	if got := fixture.line(t, "events"); got != "failed-body-joined" {
		t.Fatalf("failed body was not joined before its sibling: %q", got)
	}
	fixture.write(t, "control-b", "release")
	fixture.join(t, 2)
	if got := releaseGateDiagnosticRead(t, filepath.Join(root, "preflights", "refused", "exit")); got != "17\n" {
		t.Fatalf("lost preflight failure: %q", got)
	}
	if got := releaseGateDiagnosticRead(t, filepath.Join(root, "preflights", "refused", "stderr")); got != "original refusal\n" {
		t.Fatalf("lost original refusal bytes: %q", got)
	}
	if got := releaseGateDiagnosticRead(t, filepath.Join(root, "preflights", "independent", "exit")); got != "0\n" {
		t.Fatalf("independent preflight did not run: %q", got)
	}
	if got := releaseGateDiagnosticRead(t, filepath.Join(root, "final-source.exit")); got != "0\n" {
		t.Fatalf("unchanged source failed: %q", got)
	}
	if !strings.Contains(fixture.output.String(), "NOT RELEASE-QUALIFIED: preflights=17 bodies=23 phases-cleanup=17 release-fences=1 candidate-fence=0") {
		t.Fatalf("diagnostic failure classes were lost:\n%s", fixture.output.String())
	}
}

// Consuming the entry flags is part of admission: existing queue regression
// children may inherit the environment, but their own initializer stays strict.
func TestReleaseGateJobsDiagnosticPassingBodiesCannotQualifyOrLeakEntryMode(t *testing.T) {
	fixture := newReleaseGateDiagnosticFixture(t)
	status, output := fixture.run(t, `
[[ "$release_gate_diagnostic" == 1 && ! -v RELEASE_GATE_DIAGNOSTIC && ! -v RELEASE_GATE_DIAGNOSTIC_SOURCE_MANIFEST && ! -v RELEASE_GATE_DIAGNOSTIC_MODE_MANIFEST ]]
nested_owner() {
  bash -c 'set -euo pipefail
    source "$RELEASE_GATE_DIAGNOSTIC_FIXTURE_HELPER"
    release_gate_effective_resources() { RELEASE_GATE_CPUS=24; RELEASE_GATE_MEMORY=34359738368; }
    release_gate_jobs_init
    [[ "$release_gate_diagnostic" == 0 && ! -v RELEASE_GATE_DIAGNOSTIC ]]
    printf "nested-strict\n" > "$RELEASE_GATE_DIAGNOSTIC_FIXTURE_ROOT/nested-strict"
    release_gate_complete'
}
release_gate_start nested-owner nested_owner
release_gate_complete
[[ "$release_gate_diagnostic" == 1 && "$release_gate_body_result" == 0 ]]
diagnostic_status=0; release_gate_diagnostic_finish 0 0 || diagnostic_status=$?
exit "$diagnostic_status"
`)
	if status != 2 || !strings.Contains(output, "NOT RELEASE-QUALIFIED: preflights=0 bodies=0 phases-cleanup=0 release-fences=0 candidate-fence=0") {
		t.Fatalf("passing diagnostic granted qualification or lost a join: exit=%d\n%s", status, output)
	}
	if got := releaseGateDiagnosticRead(t, filepath.Join(fixture.root, "nested-strict")); got != "nested-strict\n" {
		t.Fatalf("nested owner inherited diagnostic admission: %q", got)
	}
	if strings.Contains(output, "release gate passed") {
		t.Fatalf("diagnostic emitted a release success marker: %s", output)
	}
}

// An initial byte, mode or census mismatch refuses before any test work; a
// missing manifest is never substituted with an inferred source snapshot.
func TestReleaseGateJobsDiagnosticRejectsMissingOrChangedInitialSource(t *testing.T) {
	for _, mutation := range []string{"missing", "bytes", "mode", "census", "link", "ancestor"} {
		fixture := newReleaseGateDiagnosticFixture(t)
		var err error
		switch mutation {
		case "missing":
			err = os.Remove(fixture.hashManifest)
		case "bytes":
			err = os.WriteFile(fixture.sourcePath, []byte("changed source\n"), 0o600)
		case "mode":
			err = os.Chmod(fixture.sourcePath, 0o640)
		case "link":
			retained := filepath.Join(fixture.root, "retained-source.txt")
			err = os.Rename(fixture.sourcePath, retained)
			if err == nil {
				err = os.Symlink(retained, fixture.sourcePath)
			}
		case "ancestor":
			directory := filepath.Join(fixture.workspace, "directory")
			retained := filepath.Join(fixture.root, "retained-directory")
			err = os.Mkdir(directory, 0o700)
			if err == nil {
				err = os.Rename(fixture.sourcePath, filepath.Join(directory, "source.txt"))
			}
			for _, manifest := range []string{fixture.hashManifest, fixture.modeManifest} {
				if err == nil {
					err = os.WriteFile(manifest, []byte(strings.ReplaceAll(releaseGateDiagnosticRead(t, manifest), "source.txt", "directory/source.txt")), 0o600)
				}
			}
			if err == nil {
				err = os.Rename(directory, retained)
			}
			if err == nil {
				err = os.Symlink(retained, directory)
			}
		case "census":
			original := releaseGateDiagnosticRead(t, fixture.modeManifest)
			err = os.WriteFile(fixture.modeManifest, []byte(original+original), 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
		status, output := fixture.run(t, `printf 'body-admitted\n' > "$RELEASE_GATE_DIAGNOSTIC_FIXTURE_ROOT/body-admitted"`)
		if status == 0 {
			t.Fatalf("%s source mutation admitted: %s", mutation, output)
		}
		if _, err := os.Stat(filepath.Join(fixture.root, "body-admitted")); !os.IsNotExist(err) {
			t.Fatalf("%s source mutation reached a body: %v", mutation, err)
		}
		if mutation != "missing" {
			root := releaseGateDiagnosticRoot(t, fixture.root)
			if got := releaseGateDiagnosticRead(t, filepath.Join(root, "initial-source.exit")); got != "1\n" {
				t.Fatalf("%s source failure was not retained: %q", mutation, got)
			}
		}
	}
}

// Even an early successful return that omits the diagnostic summary cannot
// turn an explicitly diagnostic owner into a release-success exit status.
func TestReleaseGateJobsDiagnosticExitCannotGrantQualifiedSuccess(t *testing.T) {
	fixture := newReleaseGateDiagnosticFixture(t)
	status, output := fixture.run(t, "release_gate_complete\nexit 0\n")
	if status != 2 || !strings.Contains(output, "diagnostic cannot grant a release-qualified zero exit") {
		t.Fatalf("diagnostic early return acquired release status: exit=%d\n%s", status, output)
	}
}

// The final candidate fence is independent of the release preflight and of
// body status, so a successful job cannot conceal a source write it performed.
func TestReleaseGateJobsDiagnosticFinalSourceMutationRemainsFailure(t *testing.T) {
	fixture := newReleaseGateDiagnosticFixture(t)
	status, output := fixture.run(t, `
mutating_phase() { printf 'changed source\n' > "$workspace/source.txt"; }
release_gate_start mutating mutating_phase
release_gate_complete
diagnostic_status=0; release_gate_diagnostic_finish 0 0 || diagnostic_status=$?
exit "$diagnostic_status"
`)
	if status != 2 || !strings.Contains(output, "bodies=0 phases-cleanup=0 release-fences=0 candidate-fence=1") {
		t.Fatalf("final source mutation was hidden: exit=%d\n%s", status, output)
	}
	root := releaseGateDiagnosticRoot(t, fixture.root)
	if got := releaseGateDiagnosticRead(t, filepath.Join(root, "initial-source.exit")); got != "0\n" {
		t.Fatalf("source was not initially admitted: %q", got)
	}
	if got := releaseGateDiagnosticRead(t, filepath.Join(root, "final-source.exit")); got != "1\n" {
		t.Fatalf("final mutation was not retained: %q", got)
	}
}

// Diagnostic execution cannot opt into external campaign writes or omit the
// requested private database workload; unsupported modes fail before bodies.
func TestReleaseGateJobsDiagnosticRefusesLiveOptInAndIncompleteAdmission(t *testing.T) {
	for _, value := range []string{"SIM_TESTNET_LIVE_DEPENDENCIES=1", "RUN_SERVER_DB_TESTS=0", "RELEASE_GATE_DIAGNOSTIC=2"} {
		fixture := newReleaseGateDiagnosticFixture(t)
		status, output := fixture.run(t, `printf 'body-admitted\n' > "$RELEASE_GATE_DIAGNOSTIC_FIXTURE_ROOT/body-admitted"`, value)
		if status == 0 {
			t.Fatalf("unsafe diagnostic admission %s succeeded: %s", value, output)
		}
		if _, err := os.Stat(filepath.Join(fixture.root, "body-admitted")); !os.IsNotExist(err) {
			t.Fatalf("unsafe admission %s reached a body: %v", value, err)
		}
	}
}

// Both real entry scripts refuse stateful admission after a private-service
// failure while the actual queue still executes and joins independent work.
func TestReleaseGateJobsDiagnosticServiceRefusalCannotSelectHostResources(t *testing.T) {
	for _, name := range []string{"test-release-1.0-local.sh", "test-release-1.0-producer-gate.sh"} {
		encoded, err := os.ReadFile(filepath.Join("..", "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		source := string(encoded)
		start := strings.Index(source, "release_gate_open_services() {")
		end := strings.Index(source, "release_phase_server_db() {")
		admissionStart := strings.Index(source, "if [[ \"$release_gate_services_ready\" == 1 ]]; then")
		if start < 0 || end <= start || admissionStart <= end {
			t.Fatalf("%s has no complete private-service admission", name)
		}
		admissionTail := source[admissionStart:]
		admissionEnd := strings.Index(admissionTail, "\nfi")
		if indentedEnd := strings.Index(admissionTail, "\n  fi"); indentedEnd >= 0 && (admissionEnd < 0 || indentedEnd < admissionEnd) {
			admissionEnd = indentedEnd
		}
		if admissionEnd < 0 {
			t.Fatalf("%s private-service admission is unterminated", name)
		}
		admission := admissionTail[:admissionEnd]
		fixture := newReleaseGateJobsFixture(t, `
release_gate_diagnostic=1; export release_gate_diagnostic
release_gate_services_start() { printf 'private owner refused\n' >&2; return 31; }
release_phase_server_db() { printf 'host-fallback\n' > "$RELEASE_GATE_QUEUE_FIXTURE_ROOT/host-fallback"; }
`+source[start:end]+"\n"+admission+"\nfi\n"+`
independent_body() { printf 'independent\n' > "$RELEASE_GATE_QUEUE_FIXTURE_ROOT/independent"; }
release_gate_start independent independent_body
status=0; release_gate_complete || status=$?
[[ "$status" == 31 && "$release_gate_active" == 0 && "$release_gate_services_cleaned" == 1 ]]
exit "$status"
`)
		fixture.join(t, 31)
		root := releaseGateDiagnosticRoot(t, fixture.root)
		if got := releaseGateDiagnosticRead(t, filepath.Join(root, "preflights", "private-services", "exit")); got != "31\n" {
			t.Fatalf("%s lost private-service refusal: %q", name, got)
		}
		if got := releaseGateDiagnosticRead(t, filepath.Join(fixture.root, "independent")); got != "independent\n" {
			t.Fatalf("%s did not join independent work: %q", name, got)
		}
		if _, err := os.Stat(filepath.Join(fixture.root, "host-fallback")); !os.IsNotExist(err) {
			t.Fatalf("%s admitted stateful work without its private owner: %v", name, err)
		}
	}
}
