package urnettools

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

// stubPortRoute pins the host reads behind detectBindRoute so a test never
// looks at the real kernel or runs getcap.
func stubPortRoute(t *testing.T, platform, portStart string, getcap string) {
	t.Helper()
	oldP, oldR, oldE := portRoutePlatformFn, portRouteReadFileFn, portRouteExecCmdFn
	t.Cleanup(func() { portRoutePlatformFn, portRouteReadFileFn, portRouteExecCmdFn = oldP, oldR, oldE })
	portRoutePlatformFn = func() string { return platform }
	portRouteReadFileFn = func(path string) ([]byte, error) {
		if path == extenderSysctlPortStartPath {
			return []byte(portStart), nil
		}
		return nil, os.ErrNotExist
	}
	portRouteExecCmdFn = func(name string, args ...string) ([]byte, error) {
		if name != "getcap" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		return []byte(getcap), nil
	}
}

func TestExtenderBlockPortRoute(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reading ExtenderReading
		want    string
	}{
		{"active", ExtenderReading{Source: SourceControlSocket, RoleState: "active", BindRoute: "capability on the binary (cap_net_bind_service)"}, "Port route:        capability on the binary (cap_net_bind_service)"},
		{"unavailable", ExtenderReading{Source: SourceUnknown, RoleState: "unavailable", BindRoute: "neither"}, "Port route:        neither"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			formatExtenderBlock(&out, tc.reading)
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("output missing %q:\n%s", tc.want, out.String())
			}
		})
	}
	var out bytes.Buffer
	formatExtenderBlock(&out, ExtenderReading{Source: SourceControlSocket, RoleState: "active"})
	if strings.Contains(out.String(), "Port route") {
		t.Fatalf("a reading with no route must not print one:\n%s", out.String())
	}
}

func TestDetectBindRouteSysctlWithCapabilityWarns(t *testing.T) {
	got := detectBindRouteWith("linux", "/bin/provider", "provider",
		func(string) ([]byte, error) { return []byte("443\n"), nil },
		func(name string, args ...string) ([]byte, error) {
			if name != "getcap" {
				t.Fatalf("unexpected command %s %v", name, args)
			}
			return []byte("/bin/provider cap_net_bind_service=ep"), nil
		})
	for _, want := range []string{"sysctl", "ip_unprivileged_port_start=443", "sudo setcap -r /bin/provider", "restart", "non-dumpable"} {
		if !strings.Contains(got, want) {
			t.Fatalf("route %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "removes ALL") {
		t.Fatalf("the plain capability must not carry the other-capabilities warning: %q", got)
	}
}

func TestDetectBindRouteSysctlWithOtherCapabilitiesWarnsAboutLoss(t *testing.T) {
	got := detectBindRouteWith("linux", "/bin/provider", "provider",
		func(string) ([]byte, error) { return []byte("80\n"), nil },
		func(string, ...string) ([]byte, error) {
			return []byte("/bin/provider cap_chown,cap_net_bind_service=ep"), nil
		})
	if !strings.Contains(got, "removes ALL file capabilities") {
		t.Fatalf("route %q must warn that setcap -r drops every capability", got)
	}
}

func TestDetectBindRouteCases(t *testing.T) {
	read := func(v string) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			if path == extenderSysctlPortStartPath {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		}
	}
	exec := func(out string, err error) func(string, ...string) ([]byte, error) {
		return func(string, ...string) ([]byte, error) { return []byte(out), err }
	}
	for _, tc := range []struct {
		name     string
		platform string
		user     string
		read     func(string) ([]byte, error)
		exec     func(string, ...string) ([]byte, error)
		want     string
		contains string
	}{
		{name: "sysctl without capability", platform: "linux", user: "p", read: read("443\n"), exec: exec("", nil), contains: "sysctl: port is available without a file capability (ip_unprivileged_port_start=443)"},
		{name: "capability only", platform: "linux", user: "p", read: read("1024\n"), exec: exec("/bin/urnetwork cap_net_bind_service=ep\n", nil), want: "capability on the binary (cap_net_bind_service)"},
		{name: "neither", platform: "linux", user: "p", read: read("1024\n"), exec: exec("", nil), want: "neither"},
		{name: "threshold unreadable and getcap fails", platform: "linux", user: "p", read: func(string) ([]byte, error) { return nil, os.ErrNotExist }, exec: exec("", errors.New("no getcap")), want: "neither"},
		{name: "root provider", platform: "linux", user: "root", read: read("1024\n"), exec: exec("", nil), contains: "root"},
		{name: "freebsd", platform: "freebsd", user: "p", want: ""},
		{name: "windows", platform: "windows", user: "p", want: ""},
		{name: "darwin", platform: "darwin", user: "p", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := detectBindRouteWith(tc.platform, "/bin/urnetwork", tc.user, tc.read, tc.exec)
			if tc.contains != "" {
				if !strings.Contains(got, tc.contains) {
					t.Fatalf("route %q missing %q", got, tc.contains)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("route %q, want %q", got, tc.want)
			}
		})
	}
}

// `extender status` prints the route and the capability warning for a
// provider that answers, and still prints the route for one that does not.
func TestRunExtenderStatusPrintsPortRoute(t *testing.T) {
	stubPortRoute(t, "linux", "443\n", "/opt/a/miner cap_net_bind_service=ep")
	oldS := socketExtenderFn
	t.Cleanup(func() { socketExtenderFn = oldS })
	socketExtenderFn = func(p Provider) (*ExtenderReading, error) {
		if p.StateDir == "/down" {
			return nil, errors.New("provider not running: no control socket")
		}
		return &ExtenderReading{Source: SourceControlSocket, RoleState: "active"}, nil
	}
	up := Provider{Unit: "a.service", StateDir: "/up", User: "alice", Binary: "/opt/a/miner"}
	down := Provider{Unit: "b.service", StateDir: "/down", User: "alice", Binary: "/opt/a/miner"}

	var out bytes.Buffer
	if err := runExtenderStatus(&out, []Provider{up}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Port route:", "sysctl", "sudo setcap -r /opt/a/miner"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status output missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	err := runExtenderStatus(&out, []Provider{down})
	if err == nil {
		t.Fatal("an unavailable provider must still fail the command")
	}
	if !strings.Contains(out.String(), "Extender: unavailable") || !strings.Contains(out.String(), "Port route:") || !strings.Contains(out.String(), "sudo setcap -r /opt/a/miner") {
		t.Fatalf("unavailable provider must still print the route and warning:\n%s", out.String())
	}
}

func TestRunExtenderStatusOffLinuxPrintsNoRoute(t *testing.T) {
	stubPortRoute(t, "darwin", "443\n", "")
	oldS := socketExtenderFn
	t.Cleanup(func() { socketExtenderFn = oldS })
	socketExtenderFn = func(Provider) (*ExtenderReading, error) {
		return &ExtenderReading{Source: SourceControlSocket, RoleState: "active"}, nil
	}
	var out bytes.Buffer
	if err := runExtenderStatus(&out, []Provider{{StateDir: "/x", User: "u"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Port route") {
		t.Fatalf("non-Linux hosts have no route to report:\n%s", out.String())
	}
}

// `urnet-tools status` carries the same route line in its extender block.
func TestStatusExtenderBlockPrintsPortRoute(t *testing.T) {
	stubPortRoute(t, "linux", "1024\n", "")
	oldS := socketExtenderFn
	t.Cleanup(func() { socketExtenderFn = oldS })
	socketExtenderFn = func(Provider) (*ExtenderReading, error) {
		return &ExtenderReading{Source: SourceControlSocket, RoleState: "active"}, nil
	}
	got := captureStdout(t, func() { printExtenderBlock(Provider{StateDir: "/x", User: "u", Binary: "/opt/a/miner"}) })
	if !strings.Contains(got, "Port route:        neither") {
		t.Fatalf("status block missing the route:\n%s", got)
	}
}
