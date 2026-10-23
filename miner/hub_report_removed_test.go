package miner

import (
	"os"
	"strings"
	"testing"
)

// hubReportIdentifiers are the names the removed fleet-hub report path used.
// The miner must not carry any of them again.
var hubReportIdentifiers = []string{
	"report_url",
	"report_interval",
	"bandwidthreporter",
	"hubreport",
	"reporturl",
}

// B13 (ef2106790): the fleet hub is gone, so the miner must not accept the
// report settings. A stored report_url from an older install is ignored, not
// resurrected as a live key.
func TestNoHubReportControlKeys(t *testing.T) {
	for _, key := range []string{"report_url", "report_interval"} {
		if controlKeys[key] {
			t.Errorf("%s is still an accepted control key", key)
		}
	}

	state := newControlState()
	path, cleanup := testControlSocket(t, state)
	defer cleanup()
	for _, key := range []string{"report_url", "report_interval"} {
		resp := controlRequestJSON(t, path, controlRequest{
			Cmd:   "set",
			Key:   key,
			Value: "http://198.51.100.10:8080",
		})
		if resp.OK {
			t.Errorf("set %s was accepted; the hub report settings are removed", key)
		}
		if !strings.Contains(resp.Error, "unknown control key") {
			t.Errorf("set %s error = %q, want an unknown control key refusal", key, resp.Error)
		}
		if _, found := state.get(key); found {
			t.Errorf("set %s persisted a value despite the refusal", key)
		}
	}
}

// B13: no reporter code survives in the miner's non-test sources. The scan is
// textual on purpose: a resurrected reporter would bring one of these names
// back even if no control key reached it.
func TestNoHubReportCodeInMiner(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned++
		lower := strings.ToLower(string(b))
		for _, identifier := range hubReportIdentifiers {
			if strings.Contains(lower, identifier) {
				t.Errorf("%s still references %q", name, identifier)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no miner sources; the guard is not looking at the package")
	}
}
