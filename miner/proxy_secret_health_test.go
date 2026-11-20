package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/miner/bandwidth"
)

func TestProxySecretTrafficFile(t *testing.T) {
	resetProxyEarnBandwidthForTest()
	t.Cleanup(resetProxyEarnBandwidthForTest)
	const address = "socks5://canary-user-7731:canary-pass-9042@example.com:1080?token=canary-token-5518"
	publishProxyEarnBandwidth(address, &bandwidth.ProxyBandwidth{})
	now := time.Now()
	report := proxyHealthReport{Bandwidth: proxyTrafficBandwidth(map[string]ProxyEntry{address: {ID: 1}}, now)}
	if len(report.Bandwidth) != 1 {
		t.Fatal("missing traffic row")
	}
	dir := t.TempDir()
	writeProxyTrafficState(dir, report, now)
	data, err := os.ReadFile(filepath.Join(dir, "proxy_traffic.state"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"canary-user-7731", "canary-pass-9042", "canary-token-5518"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("traffic file contains a canary")
		}
	}
}
