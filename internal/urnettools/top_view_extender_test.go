package urnettools

import (
	"bytes"
	"strings"
	"testing"

	"github.com/urfoundation/sn/internal/tui"
)

func TestTopExtenderPort53Rows(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ports      []ExtenderPort
		dns53State string
		wantValue  string
	}{
		{"bound", []ExtenderPort{{Name: "dns 53", Up: true}}, "", "bound"},
		{"down without reason", []ExtenderPort{{Name: "dns 53", Up: false}}, "", "DOWN"},
		{"in use", []ExtenderPort{{Name: "dns 53", Reason: "address_in_use"}}, "", "DOWN: address already in use"},
		{"permission", []ExtenderPort{{Name: "dns 53", Reason: "permission_denied"}}, "", "DOWN: permission denied"},
		{"unknown cause", []ExtenderPort{{Name: "dns 53", Reason: "unknown"}}, "", "DOWN: bind failed, cause unknown"},
		{"not requested", []ExtenderPort{{Name: "dns 53", Reason: "not_requested"}}, "", "not requested"},
		{"disabled", []ExtenderPort{{Name: "dns 53", Disabled: true}}, "", "off by setting"},
		{"from dns53_state bound", nil, "bound", "bound"},
		{"from dns53_state in use", nil, "address_in_use", "DOWN: address already in use"},
		{"omitted with other ports", []ExtenderPort{{Name: "tcp 443", Up: true}}, "", "not reported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(tui.DefaultTheme(), []Provider{{StateDir: t.TempDir(), Running: true}}, &fakeClock{t: topBase})
			m.applyExtender(m.gen, ExtenderReading{
				Source:     SourceControlSocket,
				RoleState:  "active",
				Ports:      tc.ports,
				Dns53State: tc.dns53State,
			})
			lines := m.extenderLines()
			found := false
			for _, l := range lines {
				if l.key == "dns 53" {
					found = true
					if l.value != tc.wantValue {
						t.Fatalf("dns 53 = %q, want %q", l.value, tc.wantValue)
					}
					break
				}
			}
			if !found {
				t.Fatalf("dns 53 row not found in extenderLines: %+v", lines)
			}
		})
	}
}

func TestExtenderStatusFormatPortsBlock(t *testing.T) {
	r := ExtenderReading{
		Source:    SourceControlSocket,
		RoleState: "active",
		Bound:     true,
		Ports: []ExtenderPort{
			{Name: "tcp 443", Up: true},
			{Name: "udp 443", Disabled: true},
			{Name: "dns 4053", Up: true},
			{Name: "dns 53", Reason: "permission_denied"},
		},
	}
	var buf bytes.Buffer
	formatExtenderBlock(&buf, r)
	out := buf.String()
	for _, want := range []string{
		"tcp 443 bound",
		"udp 443 disabled by setting",
		"dns 4053 bound",
		"dns 53 not bound (permission denied)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("formatExtenderBlock output missing %q:\n%s", want, out)
		}
	}
}
