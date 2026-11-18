package urnettools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urfoundation/sn/internal/tui"
)

func TestTopDiscoveryOptionalFields(t *testing.T) {
	cases := []struct {
		name   string
		wire   string
		want   []string
		absent []string
	}{
		{"absent", `{"known":true}`, nil, []string{"discovery", "records", "gossip messages", "last"}},
		{"partial", `{"known":true,"discovery":{"state":"running"}}`, []string{"discovery running"}, []string{"records", "gossip messages", "last"}},
		{"zero", `{"known":true,"discovery":{"active_records":0,"messages_accepted":0}}`, []string{"records 0", "gossip messages 0"}, []string{"last"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var observation ExtenderObservation
			if err := json.Unmarshal([]byte(tc.wire), &observation); err != nil {
				t.Fatal(err)
			}
			m := newTopModel(nil, 0, 0, tui.Theme{}, nil)
			m.extender = ExtenderReading{Source: SourceControlSocket, Discovery: observation.Discovery}
			b := tui.New(80, 6)
			m.drawCarriers(b)
			var text strings.Builder
			for y := 0; y < b.Height(); y++ {
				for x := 0; x < b.Width(); x++ {
					text.WriteRune(b.Cell(x, y).Rune)
				}
				text.WriteByte('\n')
			}
			for _, want := range tc.want {
				if !strings.Contains(text.String(), want) {
					t.Errorf("missing %q: %s", want, text.String())
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(text.String(), absent) {
					t.Errorf("unexpected %q: %s", absent, text.String())
				}
			}
		})
	}
}
