package urnettools

import "testing"

// Release tags of this line are dates plus a build number, not the older
// dotted fix numbers, so the tool-first check must order them too.
func TestCompareVersionTagsDateStyleReleases(t *testing.T) {
	for _, tc := range []struct {
		name   string
		v1, v2 string
		want   int
	}{
		{"later day, same month", "v2026.11.8-1093262540-sn", "v2026.11.6-1091584480-sn", 1},
		{"earlier day", "v2026.11.6-1091584480-sn", "v2026.11.8-1093262540-sn", -1},
		{"later month wins over a larger day", "v2026.11.1-1087168030-sn", "v2026.10.30-1085641150-sn", 1},
		{"later build on the same day", "v2026.10.29-1084846960-sn", "v2026.10.29-1084766765-sn", 1},
		{"same tag", "v2026.11.8-1093262540-sn", "v2026.11.8-1093262540-sn", 0},
		{"old suffix", "v2026.10.29-1084766765-sn", "v2026.10.6-1064742750-meso", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := compareVersionTags(tc.v1, tc.v2); got != tc.want {
				t.Fatalf("compareVersionTags(%q, %q) = %d, want %d", tc.v1, tc.v2, got, tc.want)
			}
		})
	}
	if !isToolNewer("v2026.11.8-1093262540-sn", "v2026.11.6-1091584480-sn") || isToolNewer("v2026.11.6-1091584480-sn", "v2026.11.8-1093262540-sn") {
		t.Fatal("isToolNewer must order date-style release tags")
	}
}
