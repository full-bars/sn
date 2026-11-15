package urnettools

import (
	"strings"
	"testing"

	"github.com/urfoundation/sn/internal/tui"
)

// The Extender box never gives way: Internals shrinks and Resources gives up rows
// before it is dropped, at every size from the smallest full layout up.
func TestTopExtenderBoxKeepsItsPlace(t *testing.T) {
	for _, size := range []struct{ w, h int }{{100, 26}, {100, 30}, {120, 34}, {120, 40}, {150, 46}, {180, 60}} {
		clock := &fakeClock{t: topBase}
		m := internalsModel(t, clock)
		m.applyExtender(m.gen, ExtenderReading{Source: SourceControlSocket, RoleState: "active", Bound: true,
			ActivatedV4: true, Ipv4: "198.51.100.7", ActivatedV6: true, Ipv6: "2001:db8:0:2b00::a", ConnectionCount: 12})
		got, _ := showOnSim(t, m, size.w, size.h)
		if !strings.Contains(got, "Extender") {
			t.Errorf("%dx%d: the Extender box is missing:\n%s", size.w, size.h, got)
		}
		if !strings.Contains(got, "role") {
			t.Errorf("%dx%d: the Extender box has no role row:\n%s", size.w, size.h, got)
		}
		for i, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
			if w := tui.StringWidth(line); w > size.w {
				t.Fatalf("%dx%d: row %d is %d cells wide", size.w, size.h, i, w)
			}
		}
	}
}

// Internals still appears when there is room for it beside the Extender box.
func TestTopInternalsStillShownBesideTheExtenderBox(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := internalsModel(t, clock)
	m.applyExtender(m.gen, ExtenderReading{Source: SourceControlSocket, RoleState: "active", Bound: true})
	got, _ := showOnSim(t, m, 150, 50)
	for _, want := range []string{"Extender", "Internals"} {
		if !strings.Contains(got, want) {
			t.Errorf("150x50: %q is missing:\n%s", want, got)
		}
	}
}
