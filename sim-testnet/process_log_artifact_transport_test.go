package main

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func artifactTransportTestLine() string {
	return "W0926 15:58:12.869839 123 sn_attempt_artifact_handlers.go:49] [sn-attempt] artifact stream failed: kind=records hash=0x" + strings.Repeat("ab", 32) + " bytes=3064430 elapsed=2m35.81601504s context=<nil> error=" + strconv.Quote("read tcp 192.0.2.1:60874->192.0.2.2:23900: read: connection reset by peer")
}

func TestProcessLogArtifactTransportResetIsTypedAndBlocking(t *testing.T) {
	line := artifactTransportTestLine()
	classification, matched := classifyProcessLogLine([]byte(line))
	if !matched || classification.class != "artifact-stream-transport-reset" || classification.nonblockingDisposition != "" || classification.faultAttributable {
		t.Fatalf("reset lost its strict typed finding: %+v", classification)
	}
	for _, mutation := range []struct{ from, to string }{
		{from: "sn_attempt_artifact_handlers.go", to: "other_handler.go"},
		{from: "context=<nil>", to: "context=context canceled"},
		{from: "kind=records", to: "kind=unknown"},
		{from: "connection reset by peer", to: "context deadline exceeded"},
		{from: `by peer"`, to: `by peer\nartifact hash mismatch"`},
	} {
		candidate := strings.Replace(line, mutation.from, mutation.to, 1)
		classification, matched := classifyProcessLogLine([]byte(candidate))
		if candidate == line || !matched || classification.class != "warning" || classification.nonblockingDisposition != "" {
			t.Fatalf("near-miss was forgiven: %+v %q", classification, candidate)
		}
	}
}

func TestProcessLogArtifactTransportMigrationPreservesV12(t *testing.T) {
	fixture := newProcessLogGateFixture(t, "", "")
	appendProcessLog(t, fixture.stderrPath, "W0926 15:58:12.000000 123 other.go:1] retained warning\n")
	if _, err := fixture.gate.Scan(false); err != nil {
		t.Fatal(err)
	}
	fixture.gate.state.Classifier = processLogClassifierV12
	retained := fixture.gate.state
	if err := fixture.gate.persistWithLock(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadProcessLogGate(fixture.dir, fixture.manifest, fixture.supervisor)
	if err != nil || reloaded.state.Classifier != processLogClassifierVersion || !reflect.DeepEqual(retained.Findings, reloaded.state.Findings) || !reflect.DeepEqual(retained.Cursors, reloaded.state.Cursors) {
		t.Fatalf("migration rewrote historical evidence: %v", err)
	}
	appendProcessLog(t, fixture.stderrPath, artifactTransportTestLine()+"\n")
	if _, err := reloaded.Scan(true); err != nil {
		t.Fatal(err)
	}
	classes := map[string]bool{}
	for _, finding := range reloaded.state.Findings {
		classes[finding.Class] = true
	}
	if len(reloaded.state.Findings) != 2 || !classes["warning"] || !classes["artifact-stream-transport-reset"] {
		t.Fatalf("new reset merged into or replaced historical evidence: %+v", reloaded.state.Findings)
	}
}
