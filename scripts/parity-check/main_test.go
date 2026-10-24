package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture writes a minimal repository: a provider package, a miner package and
// the two configuration files. Tests override only the files they care about,
// so a comparison failure points at one rule rather than at unrelated noise.
func fixture(t *testing.T, files map[string]string, allowlist, mappings string) options {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		"scripts/parity-check/allowlist.txt": allowlist,
		"scripts/parity-check/mappings.txt":  mappings,
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return options{
		root:      root,
		provider:  "provider",
		miner:     "miner",
		allowlist: "scripts/parity-check/allowlist.txt",
		mappings:  "scripts/parity-check/mappings.txt",
		minerOnly: true,
	}
}

func TestSameStemIsACounterpart(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/alpha.go": "package provider\nfunc run() {}\ntype Config struct{}\n",
		"miner/alpha.go":    "package miner\nfunc run() {}\ntype Config struct{}\n",
	}, "", "")

	out, code, err := run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitOK {
		t.Fatalf("code = %d, want %d\n%s", code, exitOK, out)
	}
	if strings.Contains(out, "(a) provider files with no miner counterpart (0)") == false {
		t.Fatalf("expected an empty file-gap section:\n%s", out)
	}
}

func TestRenamedFileMatchesByDeclarationOverlap(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/proxy_url.go": "package provider\nfunc parseProxyURL() {}\nfunc trimProxyURL() {}\n",
		"miner/proxy_manager_url.go": "package miner\nfunc parseProxyURL() {}\n" +
			"func trimProxyURL() {}\nfunc extra() {}\n",
	}, "", "")

	out, code, err := run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitOK {
		t.Fatalf("code = %d, want %d\n%s", code, exitOK, out)
	}
	if !strings.Contains(out, "matched by declaration overlap: 1 file(s)") {
		t.Fatalf("expected the overlap match to be reported:\n%s", out)
	}
}

// A port can split one provider file across several miner files. Neither half
// holds enough declarations on its own, so the one-to-one matcher reported the
// provider file as missing and never compared its declarations.
func TestProviderFilePairsWithSeveralMinerFiles(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/snapshot.go": "package provider\n" +
			"type Alpha struct{}\ntype Beta struct{}\ntype Gamma struct{}\n" +
			"type Delta struct{}\ntype Epsilon struct{}\n",
		"miner/control_report.go":  "package miner\ntype Alpha struct{}\ntype Beta struct{}\n",
		"miner/control_runtime.go": "package miner\ntype Gamma struct{}\ntype Delta struct{}\n",
	}, "", "")

	rep, err := buildReport(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.fileGaps) != 0 {
		t.Fatalf("fileGaps = %d, want 0: %v", len(rep.fileGaps), rep.fileGaps)
	}
	if got := strings.Join(rep.pairings["snapshot.go"], " "); got != "control_report.go control_runtime.go" {
		t.Fatalf("pairing = %q, want both miner files", got)
	}
	if rep.multiFilePairs != 1 {
		t.Fatalf("multiFilePairs = %d, want 1", rep.multiFilePairs)
	}
	// Epsilon is absent from every paired file and must still be named.
	if len(rep.declGaps) != 1 || rep.declGaps[0].name != "Epsilon" {
		t.Fatalf("declGaps = %v, want only Epsilon", rep.declGaps)
	}
}

// The declaration comparison must look in every paired file, not just the one
// the matcher happened to credit first.
func TestDeclarationInSecondPairedFileIsCovered(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/snapshot.go": "package provider\n" +
			"type Alpha struct{}\ntype Beta struct{}\ntype Gamma struct{}\n" +
			"type Delta struct{}\ntype Epsilon struct{}\n",
		"miner/control_report.go":  "package miner\ntype Alpha struct{}\ntype Beta struct{}\n",
		"miner/control_runtime.go": "package miner\ntype Gamma struct{}\n",
	}, "", "")

	rep, err := buildReport(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, gap := range rep.declGaps {
		if gap.name == "Gamma" {
			t.Fatalf("Gamma lives in the second paired file but was reported missing: %v", rep.declGaps)
		}
	}
	var names []string
	for _, gap := range rep.declGaps {
		names = append(names, gap.name)
	}
	if strings.Join(names, " ") != "Delta Epsilon" {
		t.Fatalf("declGaps = %v, want Delta and Epsilon", names)
	}
}

// A file whose whole exported surface crossed over is not a missing file, even
// when most of its unexported helpers were renamed: those helpers are not
// compared by default, so counting them against the file would be noise.
func TestCompleteExportedSurfacePairsALargeFile(t *testing.T) {
	files := map[string]string{
		"provider/snapshot.go": "package provider\n" +
			"type Alpha struct{}\ntype Beta struct{}\n" +
			"func helperOne() {}\nfunc helperTwo() {}\nfunc helperThree() {}\n" +
			"func helperFour() {}\nfunc helperFive() {}\nfunc helperSix() {}\n",
		"miner/control_report.go": "package miner\ntype Alpha struct{}\ntype Beta struct{}\n",
	}

	opts := fixture(t, files, "", "")
	rep, err := buildReport(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.fileGaps) != 0 || len(rep.declGaps) != 0 {
		t.Fatalf("default run: files=%v decls=%v, want no gaps", rep.fileGaps, rep.declGaps)
	}

	// -all compares the helpers, so half the declarations must be present for
	// the file to count as ported; the helpers are then reported by name.
	opts.includeUnexported = true
	rep, err = buildReport(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.fileGaps) != 1 || rep.fileGaps[0].name != "snapshot.go" {
		t.Fatalf("-all: fileGaps = %v, want snapshot.go", rep.fileGaps)
	}
}

// A mapping may list several miner files, so a split port can be recorded by
// hand when the declarations were renamed beyond what the overlap matcher and
// the name variants can see.
func TestFileMappingWithSeveralTargets(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/alpha.go": "package provider\nfunc One() {}\nfunc Two() {}\nfunc Three() {}\n",
		"miner/left.go":     "package miner\nfunc One() {}\n",
		"miner/right.go":    "package miner\nfunc Two() {}\n",
	}, "", "file alpha.go left.go\nfile alpha.go right.go\n")

	rep, err := buildReport(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.fileGaps) != 0 {
		t.Fatalf("fileGaps = %d, want 0: %v", len(rep.fileGaps), rep.fileGaps)
	}
	if got := strings.Join(rep.pairings["alpha.go"], " "); got != "left.go right.go" {
		t.Fatalf("pairing = %q, want both mapped targets", got)
	}
	// Three is absent from both targets and is compared as a declaration.
	if len(rep.declGaps) != 1 || rep.declGaps[0].name != "Three" {
		t.Fatalf("declGaps = %v, want only Three", rep.declGaps)
	}
}

func TestMissingFileFailsAndNamesItsDeclarations(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/liveness_watchdog.go": "package provider\nfunc startWatchdog() {}\n",
		"miner/other.go":                "package miner\n",
	}, "", "")

	out, code, err := run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitGap {
		t.Fatalf("code = %d, want %d\n%s", code, exitGap, out)
	}
	if !strings.Contains(out, "liveness_watchdog.go") || !strings.Contains(out, "FAIL: 1 unlisted gap(s)") {
		t.Fatalf("expected the file gap and a failing summary:\n%s", out)
	}
}

func TestAllowlistSuppressesAFileAndItsDeclarations(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/liveness_watchdog.go": "package provider\nfunc StartWatchdog() {}\n",
		"miner/other.go":                "package miner\n",
	}, "file liveness_watchdog.go liveness is owned by the supervisor on the miner\n", "")

	out, code, err := run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitOK {
		t.Fatalf("code = %d, want %d\n%s", code, exitOK, out)
	}
	if !strings.Contains(out, "allowlisted: 1 file(s)") {
		t.Fatalf("expected the allowlist count:\n%s", out)
	}
	if strings.Contains(out, "unlisted gap") && strings.Contains(out, "FAIL") {
		t.Fatalf("allowlisted file must not fail:\n%s", out)
	}
}

func TestAllowlistEntryWithoutReasonIsRejected(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/alpha.go": "package provider\n",
		"miner/alpha.go":    "package miner\n",
	}, "file alpha.go\n", "")

	_, _, err := run(opts)
	if err == nil || !strings.Contains(err.Error(), "needs a reason") {
		t.Fatalf("err = %v, want a reason requirement", err)
	}
}

func TestUnknownAllowlistKindIsRejected(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/alpha.go": "package provider\n",
		"miner/alpha.go":    "package miner\n",
	}, "method Foo why\n", "")

	_, _, err := run(opts)
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("err = %v, want an unknown-kind error", err)
	}
}

func TestProviderMinerNameSubstitution(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/alpha.go": "package provider\nfunc runProvider() {}\ntype providerConfig struct{}\n",
		"miner/alpha.go":    "package miner\nfunc runMiner() {}\ntype minerConfig struct{}\n",
	}, "", "")

	out, code, err := run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitOK {
		t.Fatalf("code = %d, want %d\n%s", code, exitOK, out)
	}
}

func TestExportChangeIsNotAGap(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/alpha.go": "package provider\nfunc BindHead() {}\n",
		"miner/alpha.go":    "package miner\nfunc bindHead() {}\n",
	}, "", "")

	out, code, err := run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitOK {
		t.Fatalf("code = %d, want %d\n%s", code, exitOK, out)
	}
}

func TestExplicitFunctionMapping(t *testing.T) {
	files := map[string]string{
		"provider/alpha.go": "package provider\nfunc BindHead() {}\n",
		"miner/alpha.go":    "package miner\nfunc bindHeadTier() {}\n",
	}

	unmapped := fixture(t, files, "", "")
	out, code, err := run(unmapped)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitGap || !strings.Contains(out, "BindHead") {
		t.Fatalf("expected BindHead to be a gap, code=%d:\n%s", code, out)
	}

	mapped := fixture(t, files, "", "func BindHead bindHeadTier\n")
	out, code, err = run(mapped)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitOK {
		t.Fatalf("mapped name must pass, code = %d:\n%s", code, out)
	}
}

func TestUnexportedDeclarationsNeedTheAllFlag(t *testing.T) {
	files := map[string]string{
		"provider/alpha.go": "package provider\nfunc helper() {}\n",
		"miner/alpha.go":    "package miner\n",
	}

	opts := fixture(t, files, "", "")
	out, code, err := run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitOK {
		t.Fatalf("unexported helper must be ignored by default, code = %d:\n%s", code, out)
	}

	opts.includeUnexported = true
	out, code, err = run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitGap || !strings.Contains(out, "helper") {
		t.Fatalf("expected helper with -all, code = %d:\n%s", code, out)
	}
}

func TestMinerOnlyFilesAreInformational(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/alpha.go": "package provider\nfunc run() {}\n",
		"miner/alpha.go":    "package miner\nfunc run() {}\n",
		"miner/extra.go":    "package miner\nfunc minerOnly() {}\n",
	}, "", "")

	out, code, err := run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitOK {
		t.Fatalf("miner-only files must not fail, code = %d:\n%s", code, out)
	}
	if !strings.Contains(out, "miner-only files") || !strings.Contains(out, "extra.go") {
		t.Fatalf("expected the informational section:\n%s", out)
	}
}

// A counterpart found by declaration overlap has a different file name, so a
// name-only miner-only check listed it as a port addition. It is ported
// provider behaviour, not an addition.
func TestOverlapPairedMinerFileIsNotAMinerOnlyAddition(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/proxy_url.go":      "package provider\nfunc parseProxyURL() {}\nfunc trimProxyURL() {}\n",
		"miner/proxy_manager_url.go": "package miner\nfunc parseProxyURL() {}\nfunc trimProxyURL() {}\nfunc extra() {}\n",
	}, "", "")

	rep, err := buildReport(opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rep.pairings["proxy_url.go"], " "); got != "proxy_manager_url.go" {
		t.Fatalf("pairing = %q, want proxy_manager_url.go", got)
	}
	if len(rep.minerOnly) != 0 {
		var names []string
		for _, file := range rep.minerOnly {
			names = append(names, file.name)
		}
		t.Fatalf("minerOnly = %v, want no addition: the file is a counterpart", names)
	}
}

// The same holds for a counterpart named only by an explicit mapping.
func TestMappedMinerFileIsNotAMinerOnlyAddition(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/alpha.go": "package provider\nfunc run() {}\n",
		"miner/left.go":     "package miner\nfunc run() {}\n",
		"miner/extra.go":    "package miner\nfunc minerOnly() {}\n",
	}, "", "file alpha.go left.go\n")

	rep, err := buildReport(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.minerOnly) != 1 || rep.minerOnly[0].name != "extra.go" {
		var names []string
		for _, file := range rep.minerOnly {
			names = append(names, file.name)
		}
		t.Fatalf("minerOnly = %v, want only extra.go", names)
	}
}

func TestOutputIsStable(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/alpha.go": "package provider\nfunc run() {}\nfunc missing() {}\n",
		"miner/alpha.go":    "package miner\nfunc run() {}\n",
		"miner/extra.go":    "package miner\n",
	}, "", "")

	first, firstCode, err := run(opts)
	if err != nil {
		t.Fatal(err)
	}
	second, secondCode, err := run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || firstCode != secondCode {
		t.Fatalf("two runs differ:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestMappingToMissingMinerFileIsAnError(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/alpha.go": "package provider\nfunc run() {}\n",
		"miner/alpha.go":    "package miner\nfunc run() {}\n",
	}, "", "file alpha.go nope.go\n")

	_, _, err := run(opts)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want a missing-target error", err)
	}
}

// A file mapping that names a _test.go file can only resolve when the scan
// included test files. Saying "does not exist" there would be a false claim
// about a file that is present on disk.
func TestTestFileMappingNeedsTheTestsFlag(t *testing.T) {
	files := map[string]string{
		"provider/alpha.go":  "package provider\nfunc run() {}\n",
		"miner/alpha.go":     "package miner\nfunc run() {}\n",
		"miner/beta_test.go": "package miner\nfunc TestBeta() {}\n",
	}

	opts := fixture(t, files, "", "file alpha.go beta_test.go\n")
	_, _, err := run(opts)
	if err == nil || !strings.Contains(err.Error(), "-tests") {
		t.Fatalf("err = %v, want a hint that -tests is required", err)
	}

	opts.includeTests = true
	rep, err := buildReport(opts)
	if err != nil {
		t.Fatalf("-tests: %v", err)
	}
	if got := strings.Join(rep.pairings["alpha.go"], " "); got != "beta_test.go" {
		t.Fatalf("pairing = %q, want beta_test.go", got)
	}
}

func TestStampAndTestMethodAreNotTopLevelDeclarations(t *testing.T) {
	opts := fixture(t, map[string]string{
		"provider/alpha.go": "package provider\nfunc (c *Config) Run() {}\nfunc run() { f := func() {}; _ = f }\n",
		"miner/alpha.go":    "package miner\nfunc run() {}\n",
	}, "", "")

	out, code, err := run(opts)
	if err != nil {
		t.Fatal(err)
	}
	if code != exitOK {
		t.Fatalf("methods and func literals must not count, code = %d:\n%s", code, out)
	}
	if !strings.Contains(out, "provider: 1 files, 1 funcs") {
		t.Fatalf("expected exactly one provider function:\n%s", out)
	}
}
