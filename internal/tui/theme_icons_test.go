package tui

import "testing"

func TestThemeStateIconsDistinct(t *testing.T) {
	for _, name := range ThemeNames() {
		th, _ := ThemeByName(name)
		if th.IconOK == 0 || th.IconWarn == 0 || th.IconBad == 0 || th.IconOK == th.IconWarn || th.IconWarn == th.IconBad || th.IconOK == th.IconBad {
			t.Errorf("%s: invalid state icons %q %q %q", name, th.IconOK, th.IconWarn, th.IconBad)
		}
	}
}

func TestColorblindThemePalette(t *testing.T) {
	th, ok := ThemeByName("colorblind")
	if !ok {
		t.Fatal("colorblind theme is missing")
	}
	for _, tc := range []struct {
		style   Style
		r, g, b uint8
	}{
		{th.OK, 0x00, 0x72, 0xb2}, {th.Warn, 0xe6, 0x9f, 0x00}, {th.Bad, 0xf0, 0xe4, 0x42},
	} {
		if r, g, b := tc.style.FG.Components(); r != tc.r || g != tc.g || b != tc.b {
			t.Errorf("color = %02x%02x%02x, want %02x%02x%02x", r, g, b, tc.r, tc.g, tc.b)
		}
	}
}
