package miner

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

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

// The SDK's activation error is arbitrary outside text. It must not carry a
// credential or a terminal escape to `extender status` / `top`, and bounding
// it must not split a UTF-8 sequence or leave half a secret visible.
func TestExtenderActivationErrorIsRedactedAndInert(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4eHh4In0.c2lnbmF0dXJlc2lnbg"
	cases := map[string]string{
		"jwt":      "activation failed: token " + jwt + " rejected",
		"bearer":   "401 with Authorization: Bearer abcdef0123456789abcdef",
		"userinfo": "dial https://operator:hunter2secret@relay.example/api failed",
		"assign":   "request failed password=hunter2secret&x=1",
		"proxy":    "via 203.0.113.9:1080:alice:hunter2secret refused",
	}
	for name, in := range cases {
		out := extenderDetailError(in)
		for _, secret := range []string{jwt, "abcdef0123456789abcdef", "hunter2secret"} {
			if strings.Contains(out, secret) {
				t.Fatalf("%s: %q leaks %q", name, out, secret)
			}
		}
		if !strings.Contains(out, "redacted") {
			t.Fatalf("%s: %q was not redacted", name, out)
		}
	}
	out := extenderDetailError("bad\x1b[2J\x1b]0;pwn\x07 line\nnext\x00")
	if strings.ContainsAny(out, "\x1b\x07\x00\n") {
		t.Fatalf("control characters survive: %q", out)
	}
	// A secret that would straddle the length bound is redacted before the cut.
	long := strings.Repeat("a ", 250) + "Bearer " + strings.Repeat("Z", 40)
	if got := extenderDetailError(long); strings.Contains(got, "ZZZZ") {
		t.Fatalf("secret at the bound survives: %q", got[len(got)-40:])
	}
	// Multi-byte text is cut on a rune boundary.
	multi := extenderDetailText(strings.Repeat("é", extenderDetailTextMax))
	if !utf8.ValidString(multi) || len(multi) > extenderDetailTextMax {
		t.Fatalf("len=%d valid=%v", len(multi), utf8.ValidString(multi))
	}
	// And it holds on the real wire path.
	status := &sdk.ExtenderProvideStatus{LastActivationError: cases["jwt"]}
	if d := extenderDetailFromStatus(status); strings.Contains(d.LastActivationError, jwt) {
		t.Fatalf("wire detail leaks: %q", d.LastActivationError)
	}
}
