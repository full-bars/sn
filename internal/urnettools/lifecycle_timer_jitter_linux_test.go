//go:build linux

package urnettools

import (
	"strings"
	"testing"
)

func TestTimerUnitContentsAlwaysRandomize(t *testing.T) {
	const weekly = "Sun *-*-* 00:00:00 UTC"
	for _, tc := range []struct {
		name     string
		existing []byte
	}{
		{name: "new timer"},
		{name: "legacy timer", existing: []byte("[Timer]\nOnCalendar=daily\nPersistent=true\n")},
		{name: "replace old delay", existing: []byte("[Timer]\nOnCalendar=daily\nRandomizedDelaySec=30s\nPersistent=true\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := timerUnitContents("urnetwork-update.timer", weekly, tc.existing)
			if err != nil {
				t.Fatal(err)
			}
			text := string(got)
			if !strings.Contains(text, "OnCalendar="+weekly) {
				t.Fatalf("timer lost its calendar: %s", text)
			}
			if strings.Count(text, "RandomizedDelaySec=6h") != 1 || strings.Contains(text, "RandomizedDelaySec=30s") {
				t.Fatalf("timer delay = %s, want exactly one 6h delay", text)
			}
		})
	}
}
