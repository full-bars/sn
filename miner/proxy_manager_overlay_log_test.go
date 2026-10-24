package miner

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// TestDesiredOverlayLogStatesTheCountOnce pins the operator-visible line: the
// count is rendered by plural(), which already carries the number, so the
// format must not print the number a second time. The old "%d prox%s" produced
// "2 prox2 ies".
func TestDesiredOverlayLogStatesTheCountOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		want  string
		stale string
	}{
		{name: "one", count: 1, want: "overlaid live health onto 1 proxy before the desired-set build", stale: "prox1"},
		{name: "many", count: 2, want: "overlaid live health onto 2 proxies before the desired-set build", stale: "prox2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTempHome(t)
			resetProxyHealthReportStateForTest()

			proxies := map[string]ProxyEntry{}
			for i := 1; i <= tc.count; i++ {
				settings := &connect.ProxySettings{Address: fmt.Sprintf("203.0.113.%d:1080", i)}
				key := settings.Key()
				proxies[key] = ProxyEntry{ID: i, Health: "dead"}
				noteLiveProxyHealthUp(key)
			}
			if err := writeProxyState(&ProxyState{Proxies: proxies}); err != nil {
				t.Fatal(err)
			}

			var log strings.Builder
			previous := proxyManagerLogWriter
			proxyManagerLogWriter = &log
			t.Cleanup(func() { proxyManagerLogWriter = previous })

			if _, _, _, err := newProxyManager(defaultProxyManagerConfig()).desired(false); err != nil {
				t.Fatal(err)
			}
			out := log.String()
			if !strings.Contains(out, tc.want) {
				t.Fatalf("overlay line missing %q:\n%s", tc.want, out)
			}
			if strings.Contains(out, tc.stale) {
				t.Fatalf("the count is printed twice (%q):\n%s", tc.stale, out)
			}
		})
	}
}
