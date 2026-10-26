package urnettools

import "testing"

// A lineage mismatch is the state where the tool's own updater would replace
// the provider with a binary from a different engine. It must fire only when
// both sides are known: a stripped or unreadable binary must stay quiet.
func TestLineageMismatch(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		provider string
		want     bool
	}{
		{"same lineage", "github.com/full-bars/sn", "github.com/full-bars/sn", false},
		{"different lineage", "github.com/full-bars/urnetwork-3.23-fix", "github.com/full-bars/sn", true},
		{"different lineage reversed", "github.com/full-bars/sn", "github.com/full-bars/meso-miner", true},
		{"unknown tool", "", "github.com/full-bars/sn", false},
		{"unknown provider", "github.com/full-bars/sn", "", false},
		{"both unknown", "", "", false},
	}
	for _, c := range cases {
		if got := lineageMismatch(c.tool, c.provider); got != c.want {
			t.Errorf("%s: lineageMismatch(%q, %q) = %v, want %v", c.name, c.tool, c.provider, got, c.want)
		}
	}
}

// The tool must be able to state its own lineage, or the warning can never
// fire on any host.
func TestToolLineageIsRecorded(t *testing.T) {
	path := toolLineage()
	if path == "" {
		t.Fatal("toolLineage is empty: the build carries no module path")
	}
	if path != "github.com/urfoundation/sn" {
		t.Fatalf("toolLineage = %q, want github.com/urfoundation/sn", path)
	}
}
