package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A gate exports its ./mainnet exports path to every job, so a nested fixture
// inherits it. On 2026-10-15 a fixture whose initializer refused after the exit
// trap was installed removed the running gate's exports mid-build: the two
// longest Rust exporter tests then found their output directories gone.

// releaseGateExportsCanary is a parent gate's exports directory as a job sees
// it: its root and exports path in the environment, and a file inside.
func releaseGateExportsCanary(t *testing.T) (root string, exports string, env []string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "urnetwork-release-gate.parent")
	exports = filepath.Join(root, "mainnet-exports")
	if err := os.MkdirAll(exports, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exports, "environment.sh"), []byte("# parent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env = append(os.Environ(), "release_gate_root="+root, "release_gate_mainnet_exports="+exports, "TMPDIR="+t.TempDir())
	return root, exports, env
}

func releaseGateJobsScript(t *testing.T, env []string, body string) (string, error) {
	t.Helper()
	helper, err := filepath.Abs("../scripts/release-gate-jobs.sh")
	if err != nil {
		t.Fatal(err)
	}
	sn, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	// The initializer sources the server's gate services; a stub cleans nothing.
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "server", "local"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "server", "local", "release-gate-services.sh"), []byte("release_gate_services_cleanup() { :; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", "-c", `set -euo pipefail
source "$1"
sn_repo="$2"; workspace="$3"
release_gate_effective_resources() { RELEASE_GATE_CPUS=24; RELEASE_GATE_MEMORY=34359738368; }
`+body, "release-exports-owner-test", helper, sn, workspace)
	command.Env = env
	output, err := command.CombinedOutput()
	return string(output), err
}

func requireReleaseGateExportsCanary(t *testing.T, exports string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(exports, "environment.sh")); err != nil {
		t.Fatalf("the parent gate's exports were removed: %v", err)
	}
}

func TestReleaseGateJobsFailedInitKeepsAnInheritedExportsDirectory(t *testing.T) {
	_, exports, env := releaseGateExportsCanary(t)
	// RELEASE_GATE_JOBS is checked after the exit trap is installed.
	output, err := releaseGateJobsScript(t, append(env, "RELEASE_GATE_JOBS=9999"), "release_gate_jobs_init\n")
	if err == nil || !strings.Contains(output, "RELEASE_GATE_JOBS must be between 1 and the effective limit") {
		t.Fatalf("initializer did not refuse RELEASE_GATE_JOBS=9999: %v\n%s", err, output)
	}
	requireReleaseGateExportsCanary(t, exports)
}

// release_gate_complete and the exit trap remove the exports only through
// release_gate_remove_own_exports.
func TestReleaseGateJobsCleanupWithoutInitKeepsAnInheritedExportsDirectory(t *testing.T) {
	_, exports, env := releaseGateExportsCanary(t)
	if output, err := releaseGateJobsScript(t, env, "release_gate_remove_own_exports\n"); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	requireReleaseGateExportsCanary(t, exports)
}

func TestReleaseGateJobsRemovesItsOwnExports(t *testing.T) {
	_, exports, env := releaseGateExportsCanary(t)
	output, err := releaseGateJobsScript(t, env, `release_gate_jobs_init
[[ "$release_gate_mainnet_exports" == "$release_gate_root/mainnet-exports" ]]
mkdir -m 700 -- "$release_gate_mainnet_exports"
printf 'own\n' > "$release_gate_mainnet_exports/environment.sh"
release_gate_remove_own_exports
[[ ! -e "$release_gate_mainnet_exports" ]]
printf 'own exports removed\n'
`)
	if err != nil || !strings.Contains(output, "own exports removed") {
		t.Fatalf("gate did not remove its own exports: %v\n%s", err, output)
	}
	requireReleaseGateExportsCanary(t, exports)
}
