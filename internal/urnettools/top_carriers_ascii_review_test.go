package urnettools

import (
	"strings"
	"testing"

	"github.com/urfoundation/sn/internal/tui"
)

func TestTopCarrierFamilyIndicatorsASCII(t *testing.T) {
	r := ExtenderReading{Source: SourceControlSocket, Carriers: []ExtenderCarrierStats{{Carrier: "t1"}}, ActivatedV4: true, ActivatedV6: false}
	for _, tc := range []struct {
		ascii bool
		want  string
	}{{true, "+-"}, {false, "●○"}} {
		m := newTopModel(nil, 0, 0, tui.Theme{ASCII: tc.ascii}, nil)
		m.extender = r
		b := tui.New(80, 3)
		m.drawCarriers(b)
		if !strings.Contains(b.String(), tc.want) {
			t.Errorf("ASCII=%v: want %q in %q", tc.ascii, tc.want, b.String())
		}
	}
}
