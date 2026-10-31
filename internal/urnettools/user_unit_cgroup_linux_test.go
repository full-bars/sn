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
