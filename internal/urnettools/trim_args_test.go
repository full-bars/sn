package urnettools

import (
	"strings"
	"testing"
)

// A trim the operator only wants to see must reach the provider as --preview;
// the apply must not. Nothing else in the CLI builds this line, so pinning it
// here is what stops the preview path from drifting back to a silent apply.
func TestTrimProviderArgsCarriesPreviewOnlyWhenAsked(t *testing.T) {
	preview := trimProviderArgs([]string{"off"}, true)
	want := []string{"proxy", "trim", "off", "--preview"}
	if len(preview) != len(want) {
		t.Fatalf("preview args = %v, want %v", preview, want)
	}
	for i := range want {
		if preview[i] != want[i] {
			t.Fatalf("preview args = %v, want %v", preview, want)
		}
	}
	apply := trimProviderArgs([]string{"500"}, false)
	if strings.Contains(strings.Join(apply, " "), "--preview") {
		t.Fatalf("an apply carried --preview: %v", apply)
	}
	if len(apply) != 3 || apply[2] != "500" {
		t.Fatalf("apply args = %v, want the positionals passed through", apply)
	}
}

// Clearing the cap is not "trim to off running proxies". The prompt is the only
// thing the operator reads before agreeing, so it has to name the action.
func TestTrimConfirmPromptNamesClearingTheCap(t *testing.T) {
	p := Provider{Unit: "urnetwork.service"}
	if got := trimConfirmPrompt(p, "off"); strings.Contains(got, " to off ") || !strings.Contains(got, "clear the trim cap") {
		t.Fatalf("prompt for off = %q, want it to name clearing the cap", got)
	}
	if got := trimConfirmPrompt(p, "off"); !strings.Contains(got, "urnetwork.service") {
		t.Fatalf("prompt for off = %q, want it to name the provider", got)
	}
	if got := trimConfirmPrompt(p, "500"); !strings.Contains(got, "500") || strings.Contains(got, "clear the trim cap") {
		t.Fatalf("prompt for a cap = %q, want a trim prompt", got)
	}
}

// remove-dead --preview is consumed by the dispatcher as the global dry-run
// flag, so it only reaches the provider (which lists candidates and writes
// nothing) if the builder re-adds it. An apply must never carry it.
func TestRemoveDeadProviderArgsCarriesPreviewOnlyWhenAsked(t *testing.T) {
	got := strings.Join(removeDeadProviderArgs([]string{"--degraded=24h"}, true), " ")
	if got != "proxy remove-dead --degraded=24h --preview" {
		t.Fatalf("preview args = %q", got)
	}
	got = strings.Join(removeDeadProviderArgs([]string{"--degraded=24h"}, false), " ")
	if got != "proxy remove-dead --degraded=24h" {
		t.Fatalf("apply args = %q, want no --preview", got)
	}
}
