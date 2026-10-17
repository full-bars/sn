// Mainnet consumes a public production-only projection of the same reviewed
// bytecode catalog, never simulator helper payloads or a second compiler output.
package main

import (
	"bytes"
	"encoding/json"
	"maps"
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
		value.ReleaseV2 = definition.releaseV2
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
		updated.ReleaseV2 = changed[i].ReleaseV2
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

// SV-8: the settlement v2 release holds exactly vault2, the implementation,
// the shared reviewed proxy and the claim batcher, with their exact bytes, in
// a fixed order.
func TestReleaseV2JsonRetainsSettlementArtifactsAndExactBytes(t *testing.T) {
	items := releaseJsonTestItems(t)
	raw, err := renderReleaseV2Json(items)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(items)
	again, err := renderReleaseV2Json(items)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("v2 release JSON depends on input ordering")
	}
	var envelope struct {
		Schema    string `json:"schema"`
		Artifacts []struct {
			Name     string `json:"name"`
			Creation string `json:"creation"`
			Runtime  string `json:"runtime"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range envelope.Artifacts {
		names = append(names, entry.Name)
		for _, source := range items {
			if source.Name == entry.Name && (!source.ReleaseV2 || entry.Creation != source.Creation || entry.Runtime != source.Runtime) {
				t.Fatalf("v2 export changed reviewed bytes for %s", entry.Name)
			}
		}
	}
	if envelope.Schema != "urnetwork-settlement-v2-contract-release-artifacts-v1" || !slices.Equal(names, []string{"ClaimBatcher", "ERC1967Proxy", "Settlement2", "SettlementVault2"}) {
		t.Fatalf("v2 release census changed: %+v", envelope)
	}
	original, err := renderReleaseJson(items)
	if err != nil || bytes.Contains(original, []byte("SettlementVault2")) || bytes.Contains(original, []byte("ClaimBatcher")) {
		t.Fatalf("v2 artifacts joined the original release: %v", err)
	}
}

// SV-8: a missing, repeated or incomplete v2 member cannot form the release.
func TestReleaseV2JsonRejectsIncompleteCensus(t *testing.T) {
	for _, name := range []string{"missing", "duplicate", "empty"} {
		items := releaseJsonTestItems(t)
		index := slices.IndexFunc(items, func(contract item) bool { return contract.Name == "SettlementVault2" })
		switch name {
		case "missing":
			items[index].ReleaseV2 = false
		case "duplicate":
			items = append(items, items[index])
		case "empty":
			items[index].Runtime = ""
		}
		if _, err := renderReleaseV2Json(items); err == nil || err.Error() == "" {
			t.Fatalf("accepted %s v2 release census", name)
		}
	}
}

// SV-8: both v2 contracts are cataloged apart from the original release, with
// their immutables in declaration order and the storage coordinates the
// deployment readback reads pinned to the compiler layout; so is the claim
// batcher, without immutables, whose layout must stay empty.
func TestSettlementV2DefinitionsPinImmutablesAndReadbackSlots(t *testing.T) {
	expected := map[string]artifactDefinition{
		"SettlementVault2": {path: "STSettlementVault2.sol/STSettlementVault2.json", immutableNames: []string{"netuid", "escrowHotkey", "selfColdkey", "bootstrap", "demandFeeWei"}, immutableSources: []string{"src/STSettlementVault2.sol"}, requiredStorageSlots: map[string]string{"coordinator": "0", "escrowRegistered": "0", "_entered": "0", "_totalCaptured": "1", "_totalPaid": "1"}},
		"Settlement2":      {path: "STSettlement2.sol/STSettlement2.json", immutableNames: []string{"__self"}, immutableSources: []string{"lib/openzeppelin-contracts/contracts/proxy/utils/UUPSUpgradeable.sol"}, requiredStorageSlots: map[string]string{"v1Coordinator": "0", "vault": "1", "_auditors": "2", "quorum": "3", "auditorSetVersion": "4"}},
		"ClaimBatcher":     {path: "STClaimBatcher.sol/STClaimBatcher.json", requireEmptyLinearStorage: true},
	}
	members := map[string]bool{}
	for _, definition := range artifactDefinitions {
		if definition.releaseV2 {
			members[definition.name] = true
		}
		want, ok := expected[definition.name]
		if !ok {
			continue
		}
		if definition.release || !definition.releaseV2 || definition.variable != "" || definition.path != want.path || !slices.Equal(definition.immutableNames, want.immutableNames) || !slices.Equal(definition.immutableSources, want.immutableSources) || !maps.Equal(definition.requiredStorageSlots, want.requiredStorageSlots) || definition.requireEmptyLinearStorage != want.requireEmptyLinearStorage {
			t.Fatalf("%s is not the exact settlement v2 artifact: %+v", definition.name, definition)
		}
		delete(expected, definition.name)
	}
	if len(expected) != 0 || !maps.Equal(members, map[string]bool{"SettlementVault2": true, "Settlement2": true, "ERC1967Proxy": true, "ClaimBatcher": true}) {
		t.Fatalf("settlement v2 release census: missing %v, members %v", expected, members)
	}
}
