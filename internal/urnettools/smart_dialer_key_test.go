package urnettools

import (
	"strings"
	"testing"
)

// `urnet-tools set smart-dialer on|off` is the measured-cost transport
// preference's switch. The provider side accepts smart_dialer on its control
// socket, but the CLI did not know the key at all: `set smart-dialer on` was
// rejected as unknown even though the same setting worked through
// `urnet-tools smart-dialer on`. The key must resolve to the socket's
// canonical name, be validated before anything is sent or queued, and appear in
// the help. The accepted values must be exactly the provider's (on and off).
func TestSmartDialerKeyIsWiredIntoTheCLI(t *testing.T) {
	for _, in := range []string{"smart-dialer", "smart_dialer", "Smart-Dialer"} {
		c, ok := canonicalControlKey(in)
		if !ok || c != "smart_dialer" {
			t.Errorf("canonicalControlKey(%q) = %q, %v; want smart_dialer", in, c, ok)
		}
	}
	for _, v := range []string{"on", "off", "ON", "Off"} {
		if err := validateControlValue("smart_dialer", v); err != nil {
			t.Errorf("value %q rejected: %v", v, err)
		}
	}
	// The provider's validator accepts only on and off for smart_dialer; so
	// must this one, or the CLI would queue a value the provider rejects.
	for _, v := range []string{"", "maybe", "1", "true", "shadow"} {
		if err := validateControlValue("smart_dialer", v); err == nil {
			t.Errorf("value %q must be rejected", v)
		}
	}
	found := false
	for _, h := range setKeyHelps {
		if strings.Contains(h, "smart-dialer") {
			found = true
		}
	}
	if !found {
		t.Error("setKeyHelps has no smart-dialer line")
	}
}

// `urnet-tools smart-dialer` must not tell an operator to restart. The key is
// live (miner/control_socket.go classifies it so, and the miner's scheduler
// makes the live case real), so help text that says "takes effect on the next
// start" is the CLI promising a worse contract than the process keeps.
func TestSmartDialerHelpDoesNotClaimARestartIsNeeded(t *testing.T) {
	long := strings.ToLower(newSmartDialerCmd().Long)
	for _, bad := range []string{"restart is required", "next start", "next restart"} {
		if strings.Contains(long, bad) {
			t.Fatalf("smart-dialer help still says %q: %s", bad, long)
		}
	}
	if !strings.Contains(long, "immediately") {
		t.Fatalf("smart-dialer help does not say the change applies immediately: %s", long)
	}
}
