package miner

import (
	"strings"
	"testing"
)

func TestGoRuntimeTuningLineReportsEnvironmentMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		percent   int
		ok        bool
		want      string
	}{
		{"mismatch", "50", 100, true, "GOGC 100 (source: unknown; differs from env GOGC=50)"},
		{"matches", "50", 50, true, "GOGC 50 (source: env GOGC)"},
		{"off matches", "off", -1, true, "GOGC off, collection disabled (source: env GOGC)"},
		{"off differs", "off", 50, true, "GOGC 50 (source: unknown; differs from env GOGC=off)"},
		{"unknown", "50", 0, false, "GOGC unknown (source: env GOGC)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := goGCObservedSource(tc.env, tc.percent, tc.ok, "env GOGC")
			line := goRuntimeTuningLogLine(1<<30, "env GOMEMLIMIT", tc.percent, tc.ok, source)
			if !strings.Contains(line, tc.want) {
				t.Fatalf("line = %q, want %q", line, tc.want)
			}
		})
	}
}
