package main

// Gate censuses read Go declarations independently of fixture text and selectors.

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Parse complete files or the declaration fragments used by selector controls.
// Top-level functions alone contribute names; comments, strings and methods do not.
func releaseSourceTestDeclarations(source string) ([]string, error) {
	files := token.NewFileSet()
	var lexer scanner.Scanner
	lexer.Init(files.AddFile("source_test.go", files.Base(), len(source)), []byte(source), nil, 0)
	_, first, _ := lexer.Scan()
	if first != token.PACKAGE {
		source = "package fixture\n" + source
	}
	file, err := parser.ParseFile(token.NewFileSet(), "source_test.go", source, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse release test declarations: %w", err)
	}
	var names []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Recv == nil && strings.HasPrefix(function.Name.Name, "Test") {
			names = append(names, function.Name.Name)
		}
	}
	return names, nil
}

// The original regex treats each column-zero fixture line as an executable root.
func TestProducerGateStateSelectionSourceCensusIgnoresFixtureText(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{
		"const synthetic = `package fixture\nfunc TestRuntimeEvidenceSyntheticDirect(t *testing.T) {}\n`\n",
		"/*\nfunc TestRuntimeEvidenceSyntheticDirect(t *testing.T) {}\n*/\n",
		"const synthetic = \"func TestRuntimeEvidenceSyntheticDirect(t *testing.T) {}\\n\"\n",
		"type syntheticReceiver struct{}\nfunc (syntheticReceiver) TestRuntimeEvidenceSyntheticDirect(t *testing.T) {}\n",
	} {
		source := "package fixture\n" + fixture + "func TestSyntheticDeclared(t *testing.T) {}\n"
		if err := verifyReleaseSourceTestCoverage("^TestSyntheticDeclared$", "^Test", []string{source}); err != nil {
			t.Fatalf("fixture text invented a release regression: %v", err)
		}
	}
}

// Whitespace and package clauses do not hide a real declaration or a new sibling.
func TestProducerGateStateSelectionSourceCensusKeepsRealDeclarations(t *testing.T) {
	t.Parallel()
	for _, packageClause := range []string{"", "package fixture\n", "package fixture_test\n"} {
		source := packageClause + "func\nTestSyntheticZulu(t *testing.T) {}\n\tfunc /* boundary */ TestSyntheticAlpha(t *testing.T) {}\n"
		names, err := releaseSelectedTestDeclarations("^TestSynthetic", []string{source})
		if err != nil || !slices.Equal(names, []string{"TestSyntheticAlpha", "TestSyntheticZulu"}) {
			t.Fatalf("real declaration census=%v: %v", names, err)
		}
		if err := verifyReleaseSourceTestCoverage("^TestSyntheticAlpha$", "^Test", []string{source}); err == nil || !strings.Contains(err.Error(), "omits source regression TestSyntheticZulu") {
			t.Fatalf("real sibling omission escaped source census: %v", err)
		}
	}
}

// AST parsing retains the existing fail-closed uniqueness, syntax and nonempty guards.
func TestProducerGateStateSelectionSourceCensusRejectsDeclarationDrift(t *testing.T) {
	t.Parallel()
	const declared = "func TestSyntheticDeclared(t *testing.T) {}\n"
	for _, testCase := range []struct {
		name string
		sources []string
		want string
	}{
		{name: "duplicate files", sources: []string{declared, declared}, want: "selected test declaration TestSyntheticDeclared is duplicated"},
		{name: "duplicate declarations", sources: []string{declared + declared}, want: "selected test declaration TestSyntheticDeclared is duplicated"},
		{name: "malformed function", sources: []string{"package fixture\n" + declared + "func broken("}, want: "parse release test declarations"},
		{name: "malformed package", sources: []string{"package\n" + declared}, want: "parse release test declarations"},
		{name: "fixture only", sources: []string{"package fixture\nconst synthetic = `\n" + declared + "`\n"}, want: "matched no test declarations"},
		{name: "no declarations", sources: []string{"package fixture\nconst synthetic = 1\n"}, want: "matched no test declarations"},
	} {
		_, err := releaseSelectedTestDeclarations("^Test", testCase.sources)
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s escaped exact source census: %v", testCase.name, err)
		}
	}
}

// Actual filename acquisition keeps current files and excludes another host's roots.
func TestProducerGateStateSelectionSourceCensusReadsCurrentBuildFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	foreign := "windows"
	if build.Default.GOOS == foreign {
		foreign = "linux"
	}
	for _, testCase := range []struct {
		name string
		source string
	}{
		{name: "active_test.go", source: "package fixture\nfunc TestSyntheticActive(t *testing.T) {}\n"},
		{name: "external_test.go", source: "package fixture_test\nfunc TestSyntheticExternal(t *testing.T) {}\n"},
		{name: "foreign_" + foreign + "_test.go", source: "package fixture\nfunc TestSyntheticForeign(t *testing.T) {}\n"},
		{name: "tagged_test.go", source: "//go:build " + foreign + "\n\npackage fixture\nfunc TestSyntheticTagged(t *testing.T) {}\n"},
	} {
		if err := os.WriteFile(filepath.Join(dir, testCase.name), []byte(testCase.source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pattern := filepath.Join(dir, "*_test.go")
	sources := releaseEvidenceV2GateSources(t, []string{pattern, pattern})
	names, err := releaseSelectedTestDeclarations("^Test", sources)
	if err != nil || !slices.Equal(names, []string{"TestSyntheticActive", "TestSyntheticExternal"}) {
		t.Fatalf("current build source census=%v: %v", names, err)
	}
}
