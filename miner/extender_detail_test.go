package miner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/sdk"
)

// The control reply flattens the closed observation and the detail into one
// object, bounds free text, and the compared observation stays detail-free so
// a ping counter moving is not a change.
func TestExtenderControlReadingFlatAndBounded(t *testing.T) {
	resetProviderExtenderObservation()
	t.Cleanup(resetProviderExtenderObservation)
	testExtenderState(t)
	listener := newProviderExtenderStatusListener(nil, 0, "")
	status := &sdk.ExtenderProvideStatus{Enabled: true, Listening: true, ActivatedV4: true, Ipv4: "198.51.100.11", ConnectionCount: 3, LastActivationError: strings.Repeat("x", 64*1024)}
	listener.ExtenderProvideStatusChanged(status)
	first := listener.last
	status.ConnectionCount = 9
	listener.ExtenderProvideStatusChanged(status)
	if listener.last != first {
		t.Fatal("a detail counter changed the compared observation")
	}
	reading := providerExtenderControlLatest()
	if reading == nil {
		t.Fatal("no reading published")
	}
	raw, err := json.Marshal(reading)
	if err != nil {
		t.Fatal(err)
	}
	var flat map[string]any
	if err := json.Unmarshal(raw, &flat); err != nil {
		t.Fatal(err)
	}
	if flat["enabled"] != true || flat["ipv4"] != "198.51.100.11" || flat["connection_count"] != float64(9) {
		t.Fatalf("flat reply = %s", raw)
	}
	if got := len(flat["last_activation_error"].(string)); got != extenderDetailTextMax {
		t.Fatalf("error text length = %d, expected %d", got, extenderDetailTextMax)
	}
}
