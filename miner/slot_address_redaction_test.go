package miner

import (
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// The slot label, the contract description and the slot display each print the
// stored address, which the legacy parser may keep malformed with userinfo.
func TestSlotAddressSinksDoNotPrintMalformedAddress(t *testing.T) {
	leaks := []string{"\x1f", "alice", "secret", "token", sinkTestUser, sinkTestPassword}
	for _, address := range []string{
		"socks5://alice:secret@host:1080/token",
		"alice:secret@host:1080",
		"host:1080;password=secret",
	} {
		check := func(sink, text string) {
			t.Helper()
			for _, leak := range leaks {
				if strings.Contains(text, leak) {
					t.Errorf("%s leaks %q for %q: %q", sink, leak, address, text)
				}
			}
		}
		check("slotSelectLabel", slotSelectLabel(3, address))
		check("contractProxyDescription", contractProxyDescription(3, address))
		check("providerSlotDisplay", providerSlotDisplay(&connect.ProxySettings{Network: "socks5", Address: address}))

		// the real select-line path, through the logger that prints it
		out := captureTlog(t, func() {
			log := withSlotSelectLabel(nil, slotSelectLabel(3, address), nil)
			log.Infof(slotSelectSuccessFormat, "normal success=1 error=0", int64(5))
		})
		if out == "" {
			t.Fatal("the select line was not printed")
		}
		check("slot select line", out)

		// the udp verdict line
		settings := &connect.ProxySettings{Network: "socks5", Address: address}
		out = captureTlog(t, func() {
			logProxyUDPResult(settings, nil, proxyUDPResult{Status: proxyUDPNoRelay, Err: errors.New("no relay")})
		})
		if out == "" {
			t.Fatal("the udp line was not printed")
		}
		check("udp check line", out)
	}
}
