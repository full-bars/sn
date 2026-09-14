// Capture guards inspect the compiled test population before semantic publication.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// List the exact running binary without rebuilding it or starting test bodies.
// The context owns the direct child through CombinedOutput's completed wait.
func releaseGateCompiledCaptureTests(ctx context.Context, selection string) ([]string, error) {
	if selection == "" {
		return nil, errors.New("compiled capture selection is empty")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate compiled capture tests: %w", err)
	}
	command := exec.CommandContext(ctx, executable, "-test.list="+selection)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("enumerate producer gate capture tests: %w\n%s", err, output)
	}
	selected := []string{}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Test") {
			selected = append(selected, line)
		}
	}
	slices.Sort(selected)
	return selected, nil
}

// Match the complete compiled census to source and retain prelaunch boundaries.
func TestProducerGateCaptureSelectionExcludesOfflineSemanticAnalysis(t *testing.T) {
	scriptPath := filepath.Join("..", "scripts", "test-release-1.0-producer-gate.sh")
	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := releaseConnectPolicySelectorAssignment(string(raw), "capture_tests")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := regexp.Compile(selection); err != nil {
		t.Fatalf("producer gate capture selection is invalid: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	compiledTests, err := releaseGateCompiledCaptureTests(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	sourceTests, err := releaseSelectedTestDeclarations(selection, releaseEvidenceV2GateSources(t, []string{"*_test.go"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyReleaseSemanticCensus(compiledTests, sourceTests); err != nil {
		t.Fatalf("compiled producer capture census differs from source: %v", err)
	}
	selected := map[string]bool{}
	for _, name := range compiledTests {
		selected[name] = true
	}
	for _, required := range []string{
		"TestFinalArchiveRetentionPreflightWritesExactDepthReceipt",
		"TestFinalCollectedFileEntryRejectsSymlinkComponentsAndSameSizeMutation",
		"TestScenarioCompletionCommitsUseDirectStoresWithoutSupervisedAPIHTTP",
		"TestFleetLifecycleSelectsTheOldestSurvivingProviderChurnRoles",
		"TestProductionHandoffAuthenticationPrecedesPrepareMutation",
		"TestScenarioCampaignAttemptRetryReusesRunIDAndSkipsCompletedPreparation",
		"TestInitialScenarioFailurePreservesExactProductionPredecessor",
		"TestExactReleaseCampaignGateRejectsSignedGraphSplice",
		"TestProducerGateCaptureSelectionExcludesOfflineSemanticAnalysis",
	} {
		if !selected[required] {
			t.Errorf("producer gate omitted required prelaunch test %s", required)
		}
	}
	for name := range selected {
		for _, forbidden := range []string{
			"FinalSemanticEvidence", "FinalSemanticBuilder", "FinalSemanticSupplement",
			"ReleaseAnalyzer", "CompletedCampaign", "CampaignParentCancellation",
			"ProduceFinalSemanticOutputs", "PublicFinalSemantic",
			"CampaignFinalSemanticEvidenceRequiresExactlyOneClosedObject",
			"PublicScenarioBundleRequiresReplicatedOwnerCompletionCommit",
		} {
			if strings.Contains(name, forbidden) {
				t.Errorf("producer gate selected post-capture analysis test %s", name)
			}
		}
	}
	if len(selected) < 40 {
		t.Errorf("producer gate capture selection unexpectedly shrank to %d tests", len(selected))
	}
}

// An empty tool path rejects the original recursive Go invocation immediately;
// the child marker also makes accidental test execution fail without recursion.
func TestProducerGateCaptureSelectionListsCurrentBinaryWithoutGoTool(t *testing.T) {
	const ownerKey = "SN_TEST_CAPTURE_LIST_OWNER"
	if os.Getenv(ownerKey) == t.Name() {
		t.Fatal("compiled capture enumeration ran a test body")
	}
	t.Setenv(ownerKey, t.Name())
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	selected, err := releaseGateCompiledCaptureTests(ctx, "^"+regexp.QuoteMeta(t.Name())+"$")
	if err != nil {
		t.Fatalf("compiled capture enumeration required a Go tool or ran a test body: %v", err)
	}
	if !slices.Equal(selected, []string{t.Name()}) {
		t.Fatalf("compiled capture enumeration changed exact selection: %v", selected)
	}
}

// A canceled owner must return its cancellation without starting a list child.
func TestProducerGateCaptureSelectionEnumerationHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	selected, err := releaseGateCompiledCaptureTests(ctx, "^"+regexp.QuoteMeta(t.Name())+"$")
	if !errors.Is(err, context.Canceled) || len(selected) != 0 {
		t.Fatalf("canceled compiled capture enumeration returned roots=%v error=%v", selected, err)
	}
}

// An empty list flag would run tests; reject it before reaching the child.
// A canceled owner makes removal of this guard fail without executing bodies.
func TestProducerGateCaptureSelectionEnumerationRejectsEmptySelector(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	selected, err := releaseGateCompiledCaptureTests(ctx, "")
	if err == nil || err.Error() != "compiled capture selection is empty" || len(selected) != 0 {
		t.Fatalf("empty compiled capture selection reached execution: roots=%v error=%v", selected, err)
	}
}

func TestProduceFinalSemanticOutputsWritesOneSealedPair(t *testing.T) {
	t.Parallel()
	source, artifacts := finalSemanticFixture(t)
	load := func(_ context.Context, locator FinalArtifactLocator) ([]byte, error) {
		data, ok := artifacts[locator.URI]
		if !ok {
			return nil, errors.New("missing fixture artifact")
		}
		return append([]byte(nil), data...), nil
	}
	factory := func(_ context.Context, draft *FinalSemanticEvidence) (FinalSemanticChainReader, error) {
		return &finalTestChainReader{evidence: draft}, nil
	}
	runDir := t.TempDir()
	sealed, err := ProduceFinalSemanticOutputs(context.Background(), runDir, source, load, factory, func(string, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(filepath.Join(runDir, finalSemanticEvidenceFilename))
	if err != nil {
		t.Fatal(err)
	}
	var observed FinalSemanticEvidence
	if err := json.Unmarshal(encoded, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.EvidenceHash != sealed.EvidenceHash || observed.PublicVerification == nil {
		t.Fatal("producer did not persist the sealed public transcript")
	}
	markdown, err := os.ReadFile(filepath.Join(runDir, finalSemanticMarkdownFilename))
	if err != nil || !strings.Contains(string(markdown), sealed.PublicVerification.TranscriptHash) {
		t.Fatalf("FINAL.md does not bind transcript: %v", err)
	}
	if _, err := ProduceFinalSemanticOutputs(context.Background(), runDir, source, load, factory, func(string, []byte) error { return nil }); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate semantic publication was not rejected: %v", err)
	}
}

func TestProduceFinalSemanticOutputsScansBeforeCreatingFiles(t *testing.T) {
	t.Parallel()
	source, artifacts := finalSemanticFixture(t)
	load := func(_ context.Context, locator FinalArtifactLocator) ([]byte, error) {
		return artifacts[locator.URI], nil
	}
	factory := func(_ context.Context, draft *FinalSemanticEvidence) (FinalSemanticChainReader, error) {
		return &finalTestChainReader{evidence: draft}, nil
	}
	runDir := t.TempDir()
	_, err := ProduceFinalSemanticOutputs(context.Background(), runDir, source, load, factory, func(name string, _ []byte) error {
		if name == finalSemanticMarkdownFilename {
			return errors.New("secret detected")
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "secret detected") {
		t.Fatalf("output scanner failure was not propagated: %v", err)
	}
	for _, name := range []string{finalSemanticEvidenceFilename, finalSemanticMarkdownFilename} {
		if _, statErr := os.Lstat(filepath.Join(runDir, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("%s was created before all checks passed: %v", name, statErr)
		}
	}
}

func TestProduceFinalSemanticOutputsScannerCannotMutateVerifiedBytes(t *testing.T) {
	t.Parallel()
	source, artifacts := finalSemanticFixture(t)
	load := func(_ context.Context, locator FinalArtifactLocator) ([]byte, error) {
		return append([]byte(nil), artifacts[locator.URI]...), nil
	}
	factory := func(_ context.Context, draft *FinalSemanticEvidence) (FinalSemanticChainReader, error) {
		return &finalTestChainReader{evidence: draft}, nil
	}
	runDir := t.TempDir()
	sealed, err := ProduceFinalSemanticOutputs(context.Background(), runDir, source, load, factory, func(_ string, content []byte) error {
		for index := range content {
			content[index] = 'x'
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	evidenceBytes, err := os.ReadFile(filepath.Join(runDir, finalSemanticEvidenceFilename))
	if err != nil {
		t.Fatal(err)
	}
	var observed FinalSemanticEvidence
	if err := json.Unmarshal(evidenceBytes, &observed); err != nil || observed.EvidenceHash != sealed.EvidenceHash {
		t.Fatalf("scanner mutated verified semantic evidence: hash=%s error=%v", observed.EvidenceHash, err)
	}
	markdown, err := os.ReadFile(filepath.Join(runDir, finalSemanticMarkdownFilename))
	if err != nil || !strings.Contains(string(markdown), sealed.EvidenceHash) {
		t.Fatalf("scanner mutated verified markdown: %v", err)
	}
}

func TestProduceFinalSemanticOutputsRejectsSymlinkRunDirectory(t *testing.T) {
	parent := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(parent, "run")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := ProduceFinalSemanticOutputs(context.Background(), link, FinalSemanticEvidence{}, func(context.Context, FinalArtifactLocator) ([]byte, error) {
		return nil, errors.New("unexpected artifact read")
	}, func(context.Context, *FinalSemanticEvidence) (FinalSemanticChainReader, error) {
		return nil, errors.New("unexpected reader construction")
	}, func(string, []byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("symlink run directory was accepted: %v", err)
	}
	entries, readErr := os.ReadDir(target)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("symlink target was modified: entries=%d error=%v", len(entries), readErr)
	}
}

func TestProduceFinalSemanticOutputsCanceledContextCreatesNoFiles(t *testing.T) {
	runDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ProduceFinalSemanticOutputs(ctx, runDir, FinalSemanticEvidence{}, func(context.Context, FinalArtifactLocator) ([]byte, error) {
		return nil, errors.New("unexpected artifact read")
	}, func(context.Context, *FinalSemanticEvidence) (FinalSemanticChainReader, error) {
		return nil, errors.New("unexpected reader construction")
	}, func(string, []byte) error { return errors.New("unexpected scan") })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled semantic production returned %v", err)
	}
	for _, name := range []string{finalSemanticEvidenceFilename, finalSemanticMarkdownFilename} {
		if _, statErr := os.Lstat(filepath.Join(runDir, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("canceled semantic production created %s: %v", name, statErr)
		}
	}
}

func TestStageFinalSemanticOutputsExposesOnlyDurablePrivateBytes(t *testing.T) {
	directory := t.TempDir()
	evidence := []byte("complete evidence bytes\n")
	markdown := []byte("complete markdown bytes\n")
	stageRoot, stagedEvidence, stagedMarkdown, err := stageFinalSemanticOutputs(directory, evidence, markdown)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stageRoot)
	if filepath.Dir(stageRoot) != directory || !strings.HasPrefix(filepath.Base(stageRoot), finalSemanticStagePrefix) {
		t.Fatalf("private stage escaped its output directory: %s", stageRoot)
	}
	for _, name := range []string{finalSemanticEvidenceFilename, finalSemanticMarkdownFilename} {
		if _, err := os.Lstat(filepath.Join(directory, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staging exposed authoritative %s: %v", name, err)
		}
	}
	for path, want := range map[string][]byte{stagedEvidence: evidence, stagedMarkdown: markdown} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("staged byte stream %s is incomplete: %q %v", path, got, err)
		}
	}
	transient := filepath.ToSlash(filepath.Join(filepath.Base(stageRoot), finalSemanticEvidenceFilename))
	if !isFinalSemanticPostCapturePath(transient) {
		t.Fatalf("crash-left private stage would invalidate the closed live capture: %s", transient)
	}
	if err := validateFinalSemanticPostCapturePath(transient); err == nil {
		t.Fatalf("private stage was accepted as a publishable semantic output: %s", transient)
	}
}

func TestWriteExclusiveFinalSemanticOutputsHasSingleConcurrentWinner(t *testing.T) {
	dir := t.TempDir()
	evidencePath := filepath.Join(dir, finalSemanticEvidenceFilename)
	markdownPath := filepath.Join(dir, finalSemanticMarkdownFilename)
	start := make(chan struct{})
	type outcome struct {
		index int
		err   error
	}
	outcomes := make(chan outcome, 8)
	var wait sync.WaitGroup
	for index := 0; index < cap(outcomes); index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			outcomes <- outcome{index: index, err: writeExclusiveFinalSemanticOutputs(evidencePath, []byte(fmt.Sprintf("evidence-%d", index)), markdownPath, []byte(fmt.Sprintf("markdown-%d", index)))}
		}(index)
	}
	close(start)
	wait.Wait()
	close(outcomes)
	winner := -1
	for result := range outcomes {
		if result.err == nil {
			if winner != -1 {
				t.Fatalf("multiple concurrent publishers succeeded: %d and %d", winner, result.index)
			}
			winner = result.index
		}
	}
	if winner == -1 {
		t.Fatal("no concurrent publisher succeeded")
	}
	evidence, evidenceErr := os.ReadFile(evidencePath)
	markdown, markdownErr := os.ReadFile(markdownPath)
	if evidenceErr != nil || markdownErr != nil || string(evidence) != fmt.Sprintf("evidence-%d", winner) || string(markdown) != fmt.Sprintf("markdown-%d", winner) {
		t.Fatalf("published pair came from different producers: evidence=%q markdown=%q errors=%v/%v winner=%d", evidence, markdown, evidenceErr, markdownErr, winner)
	}
}

func TestWriteExclusiveFinalSemanticOutputsRollsBackFirstFile(t *testing.T) {
	dir := t.TempDir()
	evidencePath := filepath.Join(dir, finalSemanticEvidenceFilename)
	markdownPath := filepath.Join(dir, finalSemanticMarkdownFilename)
	if err := os.WriteFile(markdownPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusiveFinalSemanticOutputs(evidencePath, []byte("candidate"), markdownPath, []byte("candidate")); err == nil {
		t.Fatal("second-file collision was accepted")
	}
	if _, err := os.Lstat(evidencePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first output survived failed pair publication: %v", err)
	}
	markdown, err := os.ReadFile(markdownPath)
	if err != nil || string(markdown) != "existing" {
		t.Fatalf("existing second output changed: %q %v", markdown, err)
	}
}
