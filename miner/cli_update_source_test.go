package miner

import (
	"strings"
	"testing"
)

// The fork's `provider update`, and the automatic update timer that setup and
// service install, must stay on the fork's own releases. Upstream's default
// is urnetwork/build; a fork binary that updated from there would be replaced
// by the upstream build and lose every fork feature. A later sync that brings
// upstream's constant back fails here.
func TestMinerUpdateDefaultSourceIsTheForksReleases(t *testing.T) {
	t.Setenv("URNETWORK_UPDATE_URL", "")
	source, overridden := minerUpdateSource()
	if overridden {
		t.Fatalf("with URNETWORK_UPDATE_URL empty the source must be the default, got an override %q", source)
	}
	if !strings.HasPrefix(source, "https://api.github.com/repos/full-bars/sn/releases/") {
		t.Fatalf("the default update source must be the fork's releases, got %q", source)
	}
	if strings.Contains(source, "urnetwork/build") {
		t.Fatalf("the default update source must not be upstream's build repo, got %q", source)
	}
}

// The override still wins, which is how an update is tested before a release.
func TestMinerUpdateSourceOverrideStillWins(t *testing.T) {
	t.Setenv("URNETWORK_UPDATE_URL", "file:///tmp/releases.json")
	source, overridden := minerUpdateSource()
	if !overridden || source != "file:///tmp/releases.json" {
		t.Fatalf("source = %q overridden = %v", source, overridden)
	}
}
