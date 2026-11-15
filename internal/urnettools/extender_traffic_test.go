package urnettools

import (
	"strings"
	"testing"

	"github.com/urfoundation/sn/internal/tui"
)

const (
	testKiB = 1024
	testMiB = 1024 * testKiB
	testGiB = 1024 * testMiB
	testTiB = 1024 * testGiB
)

// The traffic table prints every size in a human unit and sums the carriers.
func TestFormatExtenderTrafficHumanUnits(t *testing.T) {
	var b strings.Builder
	formatExtenderTraffic(&b, []ExtenderCarrierBytes{
		{Carrier: "tcp-443", IngressBytes: 1536, EgressBytes: 5 * testMiB, RateInBps: 2 * testKiB, RateOutBps: 3 * testMiB, LiveConns: 4, ConnsTotal: 90},
		{Carrier: "udp-443", IngressBytes: 3 * testGiB, EgressBytes: 2 * testTiB},
		{Carrier: "udp-4053"},
		{Carrier: "udp-53", IngressBytes: 7},
	})
	out := b.String()
	for _, want := range []string{
		"Traffic relayed", "carrier", "rate in", "connections",
		"tcp-443:", "1.5 KiB", "5.0 MiB", "2.0 KiB/s", "3.0 MiB/s", "4 now, 90 total",
		"udp-443:", "3.0 GiB", "2.0 TiB",
		"udp-53:", "7 B",
		"all:", // the totals row
	} {
		if !strings.Contains(out, want) {
			t.Errorf("traffic table is missing %q:\n%s", want, out)
		}
	}
	var none strings.Builder
	formatExtenderTraffic(&none, nil)
	if none.Len() != 0 {
		t.Errorf("no carriers must print nothing, got %q", none.String())
	}
}

// The Extender box of top shows one row per carrier that has carried traffic.
func TestTopExtenderBoxShowsCarrierTraffic(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := internalsModel(t, clock)
	m.applyExtender(m.gen, ExtenderReading{Source: SourceControlSocket, RoleState: "active", Bound: true,
		CarrierBytes: []ExtenderCarrierBytes{
			{Carrier: "tcp-443", IngressBytes: 1200 * testMiB, EgressBytes: 4 * testGiB, LiveConns: 3},
			{Carrier: "udp-443", IngressBytes: 12 * testMiB, EgressBytes: 3 * testMiB},
			{Carrier: "udp-4053"},
		}})
	got, _ := showOnSim(t, m, 150, 50)
	for _, want := range []string{"tcp443", "1.2 GiB", "4.0 GiB", "udp443", "12.0 MiB"} {
		if !strings.Contains(got, want) {
			t.Errorf("top is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "udp4053") {
		t.Errorf("a carrier with no traffic must not take a row:\n%s", got)
	}
	for i, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if w := tui.StringWidth(line); w > 150 {
			t.Fatalf("row %d is %d cells wide", i, w)
		}
	}
}
