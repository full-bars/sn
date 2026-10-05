// Mainnet consumes a public production-only projection of the same reviewed
// bytecode catalog, never simulator helper payloads or a second compiler output.
package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// Synthetic compiler items exercise the export independently of live artifacts.
func releaseJsonTestItems(t *testing.T) []item {
	t.Helper()
	var result []item
	for _, definition := range artifactDefinitions {
		value := testContractItem(t, definition.name, "6001", strings.Repeat("11", 32))
		value.Release = definition.release
		value.Variable = definition.variable
		result = append(result, value)
	}
	return result
}

// Exactly the five production contracts survive, including validator evidence;
// ordering is deterministic and semantic immutable offsets remain exact.
func TestReleaseJsonRetainsProductionArtifactsAndExactBytes(t *testing.T) {
	items := releaseJsonTestItems(t)
	raw, err := renderReleaseJson(items)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(items)
	again, err := renderReleaseJson(items)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("release JSON depends on input ordering")
	}
	var envelope struct {
		Schema    string `json:"schema"`
		Artifacts []struct {
			Name                string           `json:"name"`
			Creation            string           `json:"creation"`
			Runtime             string           `json:"runtime"`
			ImmutableReferences map[string][]int `json:"immutable_references"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Schema != "urnetwork-contract-release-artifacts-v1" || len(envelope.Artifacts) != 5 {
		t.Fatalf("production census changed: %+v", envelope)
	}
	seen := map[string]bool{}
	for _, entry := range envelope.Artifacts {
		seen[entry.Name] = true
		var source item
		for _, candidate := range items {
			if candidate.Name == entry.Name {
				source = candidate
			}
		}
		if !source.Release || entry.Creation != source.Creation || entry.Runtime != source.Runtime {
			t.Fatalf("export changed reviewed bytes for %s", entry.Name)
		}
	}
	if !seen["ValidatorEvidence"] || seen["CoordinatorAdversary"] || seen["SubnetProbe"] || seen["FleetBatcher"] {
		t.Fatal("release export omitted evidence or admitted a testnet helper")
	}
}

// Missing, repeated or falsely marked helper artifacts cannot create a
// superficially complete production catalog.
func TestReleaseJsonRejectsIncompleteOrAmbiguousCensus(t *testing.T) {
	for _, name := range []string{"missing", "duplicate", "helper", "empty"} {
		items := releaseJsonTestItems(t)
		switch name {
		case "missing":
			items = items[1:]
		case "duplicate":
			items = append(items, items[0])
		case "helper":
			items[0].Release = false
			for i := range items {
				if !items[i].Release && i != 0 {
					items[i].Release = true
					break
				}
			}
		case "empty":
			items[0].Creation = ""
		}
		if _, err := renderReleaseJson(items); err == nil {
			t.Fatalf("accepted %s release census", name)
		}
	}
}

// Metadata-equivalent regeneration must export the retained creation/runtime,
// just as the established committed-Go generator does.
func TestReleaseJsonUsesRetainedReviewedBytecode(t *testing.T) {
	original := releaseJsonTestItems(t)
	generated, err := renderContractArtifacts(original)
	if err != nil {
		t.Fatal(err)
	}
	changed := releaseJsonTestItems(t)
	for i := range changed {
		updated := testContractItem(t, changed[i].Name, "6001", strings.Repeat("22", 32))
		updated.Release = changed[i].Release
		updated.Variable = changed[i].Variable
		changed[i] = updated
	}
	preserved, err := preserveReviewedBytecode("contracts_gen.go", generated, changed)
	if err != nil {
		t.Fatal(err)
	}
	want, err := renderReleaseJson(original)
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderReleaseJson(preserved)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("public release export lost reviewed bytecode: %v", err)
	}
}
