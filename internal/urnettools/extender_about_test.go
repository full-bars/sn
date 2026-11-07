package urnettools

import (
	"bytes"
	"strings"
	"testing"
)

func TestExtenderAbout(t *testing.T) {
	var out bytes.Buffer
	cmd := newExtenderCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"about"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"extender setup", "extender status", "TCP 443", "UDP 443", "UDP 4053", "UDP 53"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("about output missing %q", want)
		}
	}
}

func TestExtenderAboutRejectsArguments(t *testing.T) {
	cmd := newExtenderCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"about", "unexpected"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected unexpected argument to be rejected")
	}
}
