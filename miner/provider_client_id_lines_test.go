package miner

import (
	"fmt"
	"strings"
	"testing"
)

// The per-slot identity lines are what an operator greps on every start to see
// whether the proxies came up with the identity they should have, and
// docs/Hot-Restart.md documents them by shape. These assertions exist so that a
// quiet edit to either line fails here instead of in someone's startup log.
func TestProviderClientIdLinesKeepTheirShape(t *testing.T) {
	cases := []struct {
		name   string
		format string
		want   string
	}{
		{
			name:   "reused",
			format: providerClientIdReusedFormat,
			want:   "♻️ client_id: %s (reused) provider=%d slot=%s\n",
		},
		{
			name:   "new",
			format: providerClientIdNewFormat,
			want:   "✨ client_id: %s (new) provider=%d slot=%s\n",
		},
	}
	for _, c := range cases {
		if c.format != c.want {
			t.Fatalf("%s line changed shape: got %q want %q", c.name, c.format, c.want)
		}
	}
}

// A grep for (reused) has to find every slot that kept its identity, which is
// how Hot-Restart.md tells an operator the restart changed nothing.
func TestProviderClientIdLinesAreGreppable(t *testing.T) {
	for _, c := range []struct {
		format string
		want   string
	}{
		{providerClientIdReusedFormat, "(reused)"},
		{providerClientIdNewFormat, "(new)"},
	} {
		got := fmt.Sprintf(c.format, "abc", 7, "proxy:203.0.113.9:1081")
		if !strings.Contains(got, c.want) {
			t.Fatalf("line is no longer greppable for %s: %q", c.want, got)
		}
		if !strings.Contains(got, "slot=proxy:203.0.113.9:1081") {
			t.Fatalf("line no longer names the slot: %q", got)
		}
	}
}
