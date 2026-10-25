package urnettools

import "testing"

// TestMatchKeyCollisionSameNetworkDifferentStateDir: two providers with the
// SAME user@network but different state dirs must have DIFFERENT matchKeys,
// otherwise selectByLabels dedup silently drops one.
func TestMatchKeyCollisionSameNetworkDifferentStateDir(t *testing.T) {
	a := Provider{User: "urnet", Unit: "", Network: "mynetwork", StateDir: "/home/urnet/.urnetwork"}
	b := Provider{User: "urnet", Unit: "", Network: "mynetwork", StateDir: "/opt/beta/urnet/.urnetwork"}
	if matchKey(a) == matchKey(b) {
		t.Fatalf("matchKey collision: both are %q — selectByLabels would silently drop one", matchKey(a))
	}
}
