package urnettools

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestExtenderRoleStateRevokedAndActivationFailed(t *testing.T) {
	for body, want := range map[string]string{
		`{"known":true,"enabled":true,"revoked":true,"listening":true}`:           "revoked",
		`{"known":true,"enabled":true,"activation_failed":true,"listening":true}`: "activation failed",
	} {
		dir, _ := os.MkdirTemp("", "ex")
		t.Cleanup(func() { os.RemoveAll(dir) })
		startAuditSocket(t, dir+"/provider.sock", func(controlRequest) string { return `{"ok":true,"extender":` + body + `}` })
		r, err := queryExtenderFromSocket(Provider{StateDir: dir})
		if err != nil || r.RoleState != want {
			t.Fatalf("%s: state=%v err=%v want %s", body, r, err, want)
		}
	}
}

func TestExtenderStatusProvidersAndExitCodes(t *testing.T) {
	oldD, oldS := discoverProvidersFn, socketExtenderFn
	defer func() { discoverProvidersFn, socketExtenderFn = oldD, oldS }()
	a, b := Provider{Unit: "a.service", StateDir: "/a", User: "u1"}, Provider{Unit: "b.service", StateDir: "/b", User: "u2"}

	// A sole provider is asked by its own state dir, not by $HOME.
	discoverProvidersFn = func() []Provider { return []Provider{a} }
	got, err := extenderStatusProviders()
	if err != nil || len(got) != 1 || got[0].StateDir != "/a" {
		t.Fatalf("sole provider: %v %v", got, err)
	}

	// Several providers and no way to choose: every one is asked, and one
	// unavailable socket makes the command fail while the others still print.
	socketExtenderFn = func(p Provider) (*ExtenderReading, error) {
		if p.StateDir == "/b" {
			return nil, errors.New("provider not running: no control socket")
		}
		return &ExtenderReading{Source: SourceControlSocket, RoleState: "active", Ipv4: "198.51.100.1"}, nil
	}
	var out bytes.Buffer
	err = runExtenderStatus(&out, []Provider{a, b})
	if err == nil || !strings.Contains(err.Error(), "1 of 2") || !strings.Contains(out.String(), "198.51.100.1") || !strings.Contains(out.String(), "b.service") {
		t.Fatalf("err=%v out=%s", err, out.String())
	}
	out.Reset()
	if err := runExtenderStatus(&out, []Provider{a}); err != nil || !strings.Contains(out.String(), "active") {
		t.Fatalf("err=%v out=%s", err, out.String())
	}
}
