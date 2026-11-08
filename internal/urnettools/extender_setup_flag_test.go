package urnettools

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfoundation/sn/internal/tui"
)

func TestExtenderSetupRejectsFlags(t *testing.T) {
	for _, flag := range []string{"--enable", "-e", "--json", "--non-interactive", "--user", "positional"} {
		t.Run("setup/"+flag, func(t *testing.T) {
			if err := Run([]string{"extender", "setup", flag}); err == nil {
				t.Fatalf("setup accepted %s", flag)
			}
		})
	}
	for _, flag := range []string{"--enable", "-e", "--json", "--non-interactive", "--force", "--dry-run", "--user", "positional"} {
		t.Run("status/"+flag, func(t *testing.T) {
			if err := Run([]string{"extender", "status", flag}); err == nil {
				t.Fatalf("status accepted %s", flag)
			}
		})
	}
}

func TestExtenderStatusNoSocketExitsNonzero(t *testing.T) {
	if os.Getenv("TEST_EXTENDER_STATUS_CHILD") == "1" {
		discoverProvidersFn = func() []Provider { return nil } // a host with no provider
		if err := Run([]string{"extender", "status"}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestExtenderStatusNoSocketExitsNonzero$")
	cmd.Env = append(os.Environ(), "TEST_EXTENDER_STATUS_CHILD=1", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "provider not running: no control socket") {
		t.Fatalf("exit=%v output=%q", err, out)
	}
}

func TestExtenderSocketAndTop(t *testing.T) {
	dir := t.TempDir()
	startAuditSocket(t, filepath.Join(dir, "provider.sock"), func(req controlRequest) string {
		if req.Cmd != "extender" {
			return `{"ok":false}`
		}
		return `{"ok":true,"extender":{"known":true,"enabled":true,"listening":true,"activated_v4":true,"ipv4":"198.51.100.1","dns_ports":"4053"}}`
	})
	p := Provider{StateDir: dir, Running: true}
	r, err := queryExtenderFromSocket(p)
	if err != nil || r.Source != SourceControlSocket || !r.ActivatedV4 || r.Ipv4 != "198.51.100.1" || r.DnsPorts != "4053" {
		t.Fatalf("reading=%+v err=%v", r, err)
	}
	var out bytes.Buffer
	formatExtenderBlock(&out, *r)
	if !strings.Contains(out.String(), "198.51.100.1") || strings.Contains(out.String(), "Next attempt") {
		t.Fatalf("status: %s", out.String())
	}
	for _, tc := range []struct {
		p    Provider
		want string
	}{{p, "active"}, {Provider{StateDir: t.TempDir()}, "unavailable"}} {
		m := newTestModel(tui.DefaultTheme(), []Provider{tc.p}, &fakeClock{t: topBase})
		m.showExtender = true
		gen, p, ok := m.wantExtender(m.now())
		if !ok {
			t.Fatal("no extender fetch requested")
		}
		m.applyExtender(gen, ReadExtenderStatus(p))
		b := tui.New(120, 40)
		m.render(b)
		if s := b.String(); !strings.Contains(s, tc.want) {
			t.Fatalf("top missing %s: %s", tc.want, s)
		}
	}
}
