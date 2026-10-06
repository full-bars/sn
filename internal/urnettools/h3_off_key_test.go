package urnettools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// `urnet-tools set h3 off` must reach the provider as the VALUE "off", not as a
// generic clear. The clear path drops the override and applies the provider's
// live default for h3, which is "all", so the documented way to disable H3
// would turn it on for every identity instead. Removing "h3" from
// treatsOffAsClear makes the queued-op assertion fail (a clear queue), and the
// canonical-path assertion at the top fails it even earlier.
func TestSetH3OffIsSentAsAValueNotAClear(t *testing.T) {
	c, ok := canonicalControlKey("h3")
	if !ok || c != "h3" {
		t.Fatalf("canonicalControlKey(h3) = %q, %v; want h3", c, ok)
	}
	if treatsOffAsClear(c) {
		t.Fatalf("h3 must NOT take the off-as-clear path: clearing restores the default all")
	}

	dir := t.TempDir()
	if err := applySetOverride(Provider{StateDir: dir}, "h3", "off", false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "pending_overrides.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ops []pendingOp
	if err := json.Unmarshal(data, &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Op != "set" || ops[0].Key != "h3" || ops[0].Value != "off" {
		t.Fatalf("queued ops = %+v, want one set h3=off", ops)
	}
}

// The CLI pre-check must accept exactly what the provider's parseH3Mode accepts:
// a positive count or one of the documented aliases. "0" is the off alias; any
// other zero spelling is rejected rather than read as a count of zero, which
// would mean `direct`.
func TestH3ValueValidationMatchesPositiveCounts(t *testing.T) {
	for _, ok := range []string{"off", "direct", "on", "all", "auto", "0", "1", "7", "128", "01"} {
		if err := validateControlValue("h3", ok); err != nil {
			t.Errorf("validateControlValue(h3, %q) = %v; want accepted", ok, err)
		}
	}
	for _, bad := range []string{"00", "+0", "-0", "-1", "1.5", "nonsense"} {
		if err := validateControlValue("h3", bad); err == nil {
			t.Errorf("validateControlValue(h3, %q) accepted a value it must reject", bad)
		}
	}
}
