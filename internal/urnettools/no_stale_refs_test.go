package urnettools

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoHardcodedRepoRefs verifies that the source tree has no stale
// "urnetwork-3.23-fix" references in production (non-test) .go files.
// This prevents accidental re-introduction after bulk find-and-replace.
func TestNoHardcodedRepoRefs(t *testing.T) {
	const stale = "urnetwork-3.23-fix"

	// fix_hint.go names the old repo on purpose: when a provider runs the
	// 3.23-fix lineage, the tool it needs is published there. That is a
	// deliberate cross-lineage map, not a stale reference left by a rename,
	// and TestReleaseRepoForProvider pins it. Matched on the exact relative
	// path, so a nested file of the same name is still checked.
	allowed := map[string]bool{
		"fix_hint.go": true,
	}

	// Walk all .go files under internal/urnettools/ (excluding *_test.go).
	root := "."
	var violations []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable paths
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if allowed[filepath.Base(path)] {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		lineno := 0
		for scanner.Scan() {
			lineno++
			if strings.Contains(scanner.Text(), stale) {
				violations = append(violations, fmt.Sprintf("%s:%d", path, lineno))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("found %d stale %q references in non-test code:\n  %s",
			len(violations), stale, strings.Join(violations, "\n  "))
	}
}
