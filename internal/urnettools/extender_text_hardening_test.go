package urnettools

import (
	"strings"
	"testing"
)

func TestExtenderSetupHelpManualDNSAndFreeBSD(t *testing.T) {
	text := newExtenderSetupCmd().Long
	for _, want := range []string{"UDP 53", "user-managed", "FreeBSD", "untested", "manual"} {
		if !strings.Contains(text, want) {
			t.Errorf("setup help missing %q: %s", want, text)
		}
	}
}

func TestExtenderUDP4053WarningDoesNotClaimOtherCarriersAreDown(t *testing.T) {
	for _, process := range []string{"", "resolver"} {
		for _, port := range []string{"443", "4053"} {
			p := ExtenderPorts{}
			if port == "443" {
				p.UDP443 = PortHolder{Held: true, Process: process}
			} else {
				p.UDP4053 = PortHolder{Held: true, Process: process}
			}
			warnings := p.UDPWarnings()
			if len(warnings) != 1 {
				t.Fatalf("port %s process %q: warnings = %v", port, process, warnings)
			}
			warning := warnings[0]
			if strings.Contains(warning, "TCP alone") || strings.Contains(warning, "carrier disabled") || !strings.Contains(warning, port) {
				t.Errorf("misleading warning: %s", warning)
			}
		}
	}
}
