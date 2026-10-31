//go:build linux

package urnettools

import (
	"errors"
	"strings"
	"testing"
)

func stubUserUnitOwns(t *testing.T, ok bool) *[]string {
	t.Helper()
	var asked []string
	old := userUnitOwns
	userUnitOwns = func(p Provider, unit string) bool {
		asked = append(asked, p.User+":"+unit)
		return ok
	}
	t.Cleanup(func() { userUnitOwns = old })
	return &asked
}

// TestAttachUnitFromCgroupUserUnit: a user unit whose name does not look like
// a provider is associated once the owning user's manager confirms it.
func TestAttachUnitFromCgroupUserUnit(t *testing.T) {
	asked := stubUserUnitOwns(t, true)
	p := Provider{User: "user", PID: 1, Binary: "/home/user/sn-canary/provider"}
	attachUnitFromCgroup(&p, userUnitCgroup)
	if p.Unit != "sn-canary.service" {
		t.Fatalf("Unit = %q, want sn-canary.service", p.Unit)
	}
	if len(*asked) != 1 || (*asked)[0] != "user:sn-canary.service" {
		t.Errorf("confirmation asked = %v", *asked)
	}
}

// An unconfirmed user unit, and a non-provider-named SYSTEM unit, stay unattached.
func TestAttachUnitFromCgroupUnconfirmed(t *testing.T) {
	stubUserUnitOwns(t, false)
	p := Provider{User: "user", PID: 1}
	attachUnitFromCgroup(&p, userUnitCgroup)
	if p.Unit != "" {
		t.Errorf("unconfirmed user unit attached: %q", p.Unit)
	}
	stubUserUnitOwns(t, true)
	attachUnitFromCgroup(&p, "0::/system.slice/sn-canary.service\n")
	if p.Unit != "" {
		t.Errorf("system unit with a non-provider name attached: %q", p.Unit)
	}
}

// TestUnreadableStdoutDiagnosisUnitless: the genuinely unitless case still
// gets an honest, actionable message naming the paths checked.
func TestUnreadableStdoutDiagnosisUnitless(t *testing.T) {
	p := Provider{User: "bob", PID: -5}
	msg := unreadableStdoutDiagnosis(p, "/proc/-5/fd/1", errors.New("permission denied"))
	for _, want := range []string{"/proc/-5/fd/1", "permission denied", "/proc/-5/cgroup", "as the provider's owner (bob) or root", "systemd unit"} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnosis missing %q: %s", want, msg)
		}
	}
}

// An unreadable ExecStart must fail closed: the cgroup says which unit a process
// belongs to, not that the unit starts this provider's binary.
func TestUserUnitBinaryIsProviderFailsClosed(t *testing.T) {
	p := Provider{User: "user", Binary: "/home/user/sn-canary/provider"}
	cases := []struct {
		name, bin string
		want      bool
	}{
		{"unreadable ExecStart", "", false},
		{"this provider's binary", "/home/user/sn-canary/provider", true},
		{"provider argument form", "provider", true},
		{"unrelated binary", "/usr/bin/curl", false},
	}
	for _, c := range cases {
		if got := userUnitBinaryIsProvider(p, c.bin); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// stubUserUnitProperty replaces the systemctl lookup under userUnitOwns with
// canned values and records every (scope, unit, prop) asked.
func stubUserUnitProperty(t *testing.T, vals map[string]string) *[]string {
	t.Helper()
	var asked []string
	old := userUnitProperty
	userUnitProperty = func(scope []string, unit, prop string) string {
		asked = append(asked, strings.Join(scope, " ")+"|"+unit+"|"+prop)
		return vals[prop]
	}
	t.Cleanup(func() { userUnitProperty = old })
	return &asked
}

// userUnitOwns itself, driven through the property seam.
func TestUserUnitOwns(t *testing.T) {
	const bin = "/home/user/sn-canary/provider"
	cases := []struct {
		name string
		user string
		vals map[string]string
		want bool
	}{
		{"id and binary match", "user", map[string]string{"Id": "sn-canary.service", "ExecStart": "{ path=" + bin + " ; argv[]=" + bin + " provide ; }"}, true},
		{"manager does not know the unit", "user", map[string]string{"Id": "", "ExecStart": "{ path=" + bin + " ; }"}, false},
		{"id differs from the cgroup unit", "user", map[string]string{"Id": "other.service", "ExecStart": "{ path=" + bin + " ; }"}, false},
		{"unreadable ExecStart fails closed", "user", map[string]string{"Id": "sn-canary.service"}, false},
		{"unrelated binary", "user", map[string]string{"Id": "sn-canary.service", "ExecStart": "{ path=/usr/bin/curl ; argv[]=/usr/bin/curl ; }"}, false},
	}
	for _, c := range cases {
		stubUserUnitProperty(t, c.vals)
		p := Provider{User: c.user, Binary: bin}
		if got := userUnitOwns(p, "sn-canary.service"); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Pinned: a provider with no recorded owner is never confirmed, and systemd is
// not even asked (there is no user manager to address, and "-M @" is not one).
func TestUserUnitOwnsEmptyUserFailsClosed(t *testing.T) {
	asked := stubUserUnitProperty(t, map[string]string{"Id": "sn-canary.service", "ExecStart": "{ path=/p ; }"})
	if userUnitOwns(Provider{User: "", Binary: "/p"}, "sn-canary.service") {
		t.Fatal("empty p.User was confirmed as owner")
	}
	if len(*asked) != 0 {
		t.Errorf("systemd consulted with no owner: %v", *asked)
	}
}

// The lookup is scoped to the owning user's manager, never the system one.
func TestUserUnitOwnsQueriesOwnersManager(t *testing.T) {
	asked := stubUserUnitProperty(t, map[string]string{"Id": "x.service"})
	userUnitOwns(Provider{User: "definitely-not-me-xyz", Binary: "/p"}, "x.service")
	if len(*asked) == 0 || !strings.HasPrefix((*asked)[0], "--user -M definitely-not-me-xyz@|x.service|Id") {
		t.Errorf("unexpected scope: %v", *asked)
	}
}
