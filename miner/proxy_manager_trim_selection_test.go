package miner

import "testing"

// The control socket's snapshot reports the trim decision this process made, so
// `urnet-tools proxy trim` (no argument) and `urnet-tools status` can show a
// reduced-capacity box from the provider's own numbers instead of reading the
// operator file. These tests pin the record; the wire fill is pinned in
// control_report_trim_test.go and the completeness gate in
// proxy_manager_trim_complete_test.go.

func TestNoteTrimSelectionRecordsDesiredAndCap(t *testing.T) {
	withTempHome(t)
	if err := writeTrimTarget(500); err != nil {
		t.Fatal(err)
	}
	noteTrimSelection(1200)

	desired, cap, source, ok := currentTrimSelection()
	if !ok {
		t.Fatal("selection not recorded")
	}
	if desired != 1200 || cap != 500 || source != trimCapOperator {
		t.Fatalf("got desired=%d cap=%d source=%q, want 1200/500/%q", desired, cap, source, trimCapOperator)
	}
}

func TestNoteTrimSelectionClampsCapToDesired(t *testing.T) {
	withTempHome(t)
	if err := writeTrimTarget(5000); err != nil {
		t.Fatal(err)
	}
	noteTrimSelection(300)

	if _, cap, _, _ := currentTrimSelection(); cap != 300 {
		t.Fatalf("cap = %d, want it clamped to the 300 configured", cap)
	}
}

func TestNoteTrimSelectionNoCap(t *testing.T) {
	withTempHome(t)
	noteTrimSelection(700)

	desired, cap, source, ok := currentTrimSelection()
	if !ok {
		t.Fatal("selection not recorded")
	}
	if desired != 700 || cap != 0 || source != "" {
		t.Fatalf("got desired=%d cap=%d source=%q, want 700/0/empty", desired, cap, source)
	}
}

// Before the first decision nothing is reported: the reader must see "not
// reported" rather than a zero cap that reads as "this provider runs its full
// pool".
func TestCurrentTrimSelectionUnset(t *testing.T) {
	withTempHome(t)

	if _, _, _, ok := currentTrimSelection(); ok {
		t.Fatal("a selection was reported before the first trim decision")
	}
}
