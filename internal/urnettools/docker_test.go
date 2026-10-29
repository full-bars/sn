package urnettools

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestIsDockerCandidate covers exact image repository matching, independent of name.
func TestIsDockerCandidate(t *testing.T) {
	cases := []struct {
		image string
		name  string
		want  bool
	}{
		{"ghcr.io/full-bars/sn:latest", "anything", true},
		{"GHCR.IO/FULL-BARS/SN:latest", "web", true},
		{"ghcr.io/full-bars/sn@sha256:abcdef", "web", true},
		{"ghcr.io/full-bars/sn:v1@sha256:abcdef", "web", true},
		{"ghcr.io/full-bars/meso-miner:latest", "web", true},
		{"ghcr.io/full-bars/urnetwork-3.23-fix:latest", "web", true},
		{"ghcr.io/urfoundation/meso-miner:latest", "web", true},
		{"3cape/sn:latest", "web", true},
		{"docker.io/3cape/sn:latest", "web", true},
		{"3cape/urnetwork-3.23-fix:latest", "web", true},
		{"docker.io/3cape/urnetwork-3.23-fix:latest", "web", true},
		{"nginx:latest", "my-urnetwork-test", false},
		{"evil.example/ghcr.io/full-bars/sn:latest", "urnet", false},
		{"ghcr.io/full-bars/sn-experimental:latest", "urnet", false},
		{"ghcr.io/full-bars/urnetwork-3.23-fix-hub:latest", "urnet", false},
		{"urnetwork:stable", "provider1", false},
		{"ubuntu:24.04", "urnet-test", false},
	}
	for _, c := range cases {
		if got := isDockerCandidate(c.image, c.name); got != c.want {
			t.Errorf("isDockerCandidate(%q, %q) = %v, want %v", c.image, c.name, got, c.want)
		}
	}
}

func TestParseDockerRepository(t *testing.T) {
	cases := []struct{ image, want string }{
		{"ghcr.io/full-bars/sn:latest", "ghcr.io/full-bars/sn"},
		{"ghcr.io/full-bars/sn@sha256:abcdef", "ghcr.io/full-bars/sn"},
		{"ghcr.io/full-bars/sn:v1@sha256:abcdef", "ghcr.io/full-bars/sn"},
		{"GHCR.IO/FULL-BARS/SN:V1", "ghcr.io/full-bars/sn"},
		{"ghcr.io/full-bars/sn", "ghcr.io/full-bars/sn"},
		{"3CAPE/SN:latest", "docker.io/3cape/sn"},
		{"docker.io/3cape/sn@sha256:abcdef", "docker.io/3cape/sn"},
		{"registry.example:5000/team/image:tag", "registry.example:5000/team/image"},
		{"registry.example:5000/team/image", "registry.example:5000/team/image"},
	}
	for _, c := range cases {
		if got := parseDockerRepository(c.image); got != c.want {
			t.Errorf("parseDockerRepository(%q) = %q, want %q", c.image, got, c.want)
		}
	}
}

func TestUnitDockerResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Docker test shim requires a POSIX shell")
	}
	shim := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
if [ "$1" = ps ]; then
  printf '%s\n' 'a123456789abcdef|local-miner|urnetwork-miner:local|running' 'a987654321abcdef|other|nginx:latest|exited' 'b123456789abcdef|official|ghcr.io/full-bars/sn:latest|exited'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(shim, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	old := testDockerBin
	testDockerBin = shim
	t.Cleanup(func() { testDockerBin = old })

	if got := discoverDockerContainers(); len(got) != 1 || got[0].Name != "official" {
		t.Fatalf("default discovery = %+v, want only official", got)
	}
	for _, unit := range []string{"local-miner", "a123456789abcdef", "a123"} {
		for _, selectFn := range []func([]Provider, Target) (Provider, error){selectDockerTarget, selectDockerTargetInteractive} {
			p, err := selectFn(nil, Target{Unit: unit})
			if err != nil || p.Unit != "local-miner" || !p.Running || p.Binary != "docker:urnetwork-miner:local" {
				t.Fatalf("unit %q: provider = %+v, error = %v", unit, p, err)
			}
		}
	}
	if err := cmdDockerStatus([]string{"--unit", "local-miner"}); err != nil {
		t.Fatalf("status --unit local-miner: %v", err)
	}
	if err := cmdDockerStatus([]string{"--unit", "missing"}); err == nil || !strings.Contains(err.Error(), `container "missing" not found`) {
		t.Fatalf("unknown unit error = %v", err)
	}
	if _, err := selectDockerTarget(nil, Target{Unit: "a"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous ID error = %v", err)
	}
	if _, err := selectDockerTarget(nil, Target{}); err == nil || !strings.Contains(err.Error(), "no providers found") {
		t.Fatalf("default selection included non-candidates: %v", err)
	}
}

// TestDockerImageVersion extracts the version from an image tag (only
// version-like tags; latest/stable/nightly/dev and bare tags return "").
func TestDockerImageVersion(t *testing.T) {
	cases := []struct {
		image string
		want  string
	}{
		{"ghcr.io/full-bars/sn:v3.23.0-fix.27.0", "v3.23.0-fix.27.0"},
		{"probe-test:mainnet", ""}, // plain tag, not version-like
		{"nginx:latest", ""},       // latest is not a version
		{"redis:7", ""},            // bare digit, not version-like
		{"urnetwork:stable", ""},   // stable is not a version
		{"urnetwork:v3.23.0-fix.26", "v3.23.0-fix.26"},
	}
	for _, c := range cases {
		if got := dockerImageVersion(c.image); got != c.want {
			t.Errorf("dockerImageVersion(%q) = %q, want %q", c.image, got, c.want)
		}
	}
}

// TestParseLogLineCount covers the optional trailing line-count argument
// (default 250, positive integers only).
func TestParseLogLineCount(t *testing.T) {
	if got, err := parseLogLineCount(nil); err != nil || got != defaultLogTailLines {
		t.Errorf("parseLogLineCount(nil) = %d, %v; want %d, nil", got, err, defaultLogTailLines)
	}
	if got, err := parseLogLineCount([]string{"50"}); err != nil || got != 50 {
		t.Errorf("parseLogLineCount([50]) = %d, %v; want 50, nil", got, err)
	}
	if _, err := parseLogLineCount([]string{"0"}); err == nil {
		t.Error("parseLogLineCount([0]) = nil error; want error")
	}
	if _, err := parseLogLineCount([]string{"-3"}); err == nil {
		t.Error("parseLogLineCount([-3]) = nil error; want error")
	}
	if _, err := parseLogLineCount([]string{"abc"}); err == nil {
		t.Error("parseLogLineCount([abc]) = nil error; want error")
	}
	if _, err := parseLogLineCount([]string{"3.5"}); err == nil {
		t.Error("parseLogLineCount([3.5]) = nil error; want error")
	}
	if _, err := parseLogLineCount([]string{"99999999999999999999"}); err == nil {
		t.Error("parseLogLineCount([overflow]) = nil error; want error")
	}
}

// TestSplitExecArgs covers the exec argument-splitting logic (the pure
// helper behind cmdDockerExec). The integration tests only
// exercised error paths (no docker daemon), so the actual -- separator
// forwarding of rest... was never verified. This pins the slice math
// directly without docker, and also pins the trailing-flag panic
// guard and the inner-help forwarding.
func TestSplitExecArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantPre  []string
		wantRest []string
		wantErr  string
	}{
		// -- separator forwarding (happy path)
		{"sep-first-no-target", []string{"--", "urnet-tools", "status"}, nil, []string{"urnet-tools", "status"}, ""},
		{"sep-flag-first-word", []string{"--", "-f", "urnet-tools"}, nil, []string{"-f", "urnet-tools"}, ""},
		{"sep-flags-with-values", []string{"--", "urnet-tools", "--proxy_file=/tmp/p.txt"}, nil, []string{"urnet-tools", "--proxy_file=/tmp/p.txt"}, ""},
		{"sep-empty-command", []string{"--unit", "x", "--"}, []string{"--unit", "x"}, nil, ""},
		{"sep-only", []string{"--"}, nil, nil, ""},
		{"sep-multiple-inner-flags", []string{"--", "urnet-tools", "--verbose", "cmd"}, nil, []string{"urnet-tools", "--verbose", "cmd"}, ""},
		// no-separator forms (backward compat)
		{"no-sep-command-first", []string{"urnet-tools", "status"}, nil, []string{"urnet-tools", "status"}, ""},
		{"no-sep-target-first", []string{"--unit", "x", "urnet-tools", "--proxy_file=/tmp/p.txt"}, []string{"--unit", "x"}, []string{"urnet-tools", "--proxy_file=/tmp/p.txt"}, ""},
		// unknown leading flags must error loudly (not be swallowed)
		{"unknown-dash-flag", []string{"--verbose", "cmd"}, nil, nil, "unknown flag"},
		{"unknown-short-flag", []string{"-f", "cmd"}, nil, nil, "unknown flag"},
		{"unknown-flag-with-target", []string{"--unit", "x", "--verbose", "urnet-tools"}, []string{"--unit", "x"}, nil, "unknown flag"},
		{"empty", nil, nil, nil, ""},
		// A trailing recognized target flag (no value)
		// must error, not panic on the slice below.
		{"trailing-unit-no-value", []string{"--unit"}, nil, nil, "requires a value"},
		{"trailing-unit-no-value-after-target", []string{"--unit", "x", "--network"}, []string{"--unit", "x"}, nil, "requires a value"},
		// -h/--help AFTER the -- separator belongs to the
		// container command and must be forwarded, not intercepted.
		{"help-after-sep-forwarded", []string{"--", "urnet-tools", "--help"}, nil, []string{"urnet-tools", "--help"}, ""},
		{"help-after-sep-with-target", []string{"--unit", "x", "--", "urnet-tools", "--help"}, []string{"--unit", "x"}, []string{"urnet-tools", "--help"}, ""},
		// -h/--help BEFORE the separator (no --) is docker help (errHelpShown).
		{"help-before-sep", []string{"--unit", "x", "--help"}, nil, nil, "help shown"},
		{"help-bare-long", []string{"--help"}, nil, nil, "help shown"},
		{"help-bare-short", []string{"-h"}, nil, nil, "help shown"},
		{"help-after-target-no-sep", []string{"--unit", "x", "-h"}, nil, nil, "help shown"},
		// command-first multi-arg form still works
		{"command-first-multi-arg", []string{"urnet-tools", "proxy", "add", "--proxy_file=/tmp/p.txt"}, nil, []string{"urnet-tools", "proxy", "add", "--proxy_file=/tmp/p.txt"}, ""},
		// equals-form IS recognized: --unit=x parses to the same target as the
		// space form, matching the rest of the suite's --flag=value convention.
		{"equals-form-accepted", []string{"--unit=x", "cmd"}, []string{"--unit=x"}, []string{"cmd"}, ""},
		// target flags after -- are forwarded verbatim (not parsed)
		{"inner-target-like-flag-forwarded", []string{"--", "--unit", "y", "cmd"}, nil, []string{"--unit", "y", "cmd"}, ""},
		{"target-then-sep-then-inner-target", []string{"--unit", "x", "--", "--unit", "y"}, []string{"--unit", "x"}, []string{"--unit", "y"}, ""},
	}

	// Pre-separator --help must PRINT usage
	// (side effect), not just return the sentinel silently. RunDocker
	// already covers bare exec --help via hasHelpFlag; this covers the
	// --unit x --help form that goes through splitExecArgs.
	t.Run("pre-sep-help-prints-usage", func(t *testing.T) {
		old := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stderr = w
		defer func() { os.Stderr = old }()

		var buf bytes.Buffer
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(&buf, r)
			close(done)
		}()

		err = cmdDockerExec([]string{"--unit", "x", "--help"})
		w.Close()
		<-done
		r.Close()
		if err != nil {
			t.Fatalf("cmdDockerExec([--unit x --help]) err = %v, want nil", err)
		}
		if !strings.Contains(buf.String(), "urnet-docker") {
			t.Fatalf("cmdDockerExec([--unit x --help]) did not print usage, got %q", buf.String())
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pre, rest, err := splitExecArgs(c.args)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("splitExecArgs(%v) err = %v, want contains %q", c.args, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitExecArgs(%v) unexpected err: %v", c.args, err)
			}
			if len(pre) != len(c.wantPre) {
				t.Fatalf("splitExecArgs(%v) pre = %v, want %v", c.args, pre, c.wantPre)
			}
			if len(rest) != len(c.wantRest) {
				t.Fatalf("splitExecArgs(%v) rest = %v, want %v", c.args, rest, c.wantRest)
			}
			for i := range pre {
				if pre[i] != c.wantPre[i] {
					t.Fatalf("splitExecArgs(%v) pre[%d] = %q, want %q", c.args, i, pre[i], c.wantPre[i])
				}
			}
			for i := range rest {
				if rest[i] != c.wantRest[i] {
					t.Fatalf("splitExecArgs(%v) rest[%d] = %q, want %q", c.args, i, rest[i], c.wantRest[i])
				}
			}
		})
	}
}
