package urnettools

import (
	"strings"
	"testing"
	"time"
)

func TestFormatHostCapability(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name string
		cap  *HostCapability
		want string
	}{
		{"dual stack", &HostCapability{IPv6Usable: true, CheckedAtUnix: now.Add(-2 * time.Minute).Unix()}, "ipv4 + ipv6"},
		{"v4 only, had it before", &HostCapability{
			IPv6Usable:           false,
			EverUsable:           true,
			CheckedAtUnix:        now.Add(-time.Hour).Unix(),
			LastIPv6UsableAtUnix: now.Add(-48 * time.Hour).Unix(),
		}, "ipv4 only"},
		{"v4 only, never had it", &HostCapability{CheckedAtUnix: now.Add(-time.Minute).Unix()}, "never had ipv6"},
		{"provider does not report", nil, "unknown"},
	}
	for _, c := range cases {
		got := formatHostCapability(c.cap, now)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: %q missing %q", c.name, got, c.want)
		}
	}
}
