package urnettools

import "testing"

func TestExtenderSwitchCommandsAndOffValue(t *testing.T) {
	cmd := newExtenderCmd()
	for _, name := range []string{"on", "off", "enable", "disable"} {
		found, _, err := cmd.Find([]string{name})
		if err != nil || found == cmd {
			t.Fatalf("%s not registered: %v", name, err)
		}
	}
	key, ok := canonicalControlKey("extender")
	if !ok || treatsOffAsClear(key) {
		t.Fatal("extender off must be a persisted value")
	}
	if err := validateControlValue(key, "off"); err != nil {
		t.Fatal(err)
	}
}
