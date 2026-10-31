package urnettools

import "testing"

const userUnitCgroup = "0::/user.slice/user-1000.slice/user@1000.service/app.slice/sn-canary.service\n"

// Runs on every build target: unitFromCgroup is pure text parsing.
func TestUnitFromCgroup(t *testing.T) {
	cases := []struct {
		name, cg, unit string
		user           bool
	}{
		{"user unit", userUnitCgroup, "sn-canary.service", true},
		{"system unit", "0::/system.slice/urnetwork-native.service\n", "urnetwork-native.service", false},
		{"session scope", "0::/user.slice/user-1000.slice/session-3.scope\n", "", false},
		{"empty", "", "", false},
		// The per-user manager is infrastructure, not the workload.
		{"user manager itself", "0::/user.slice/user-1000.slice/user@1000.service\n", "", false},
		{"user manager init scope", "0::/user.slice/user-1000.slice/user@1000.service/init.scope\n", "", false},
		{"user manager under session", "0::/user.slice/user-1000.slice/user@1000.service/session.slice\n", "", false},
		// Only the unified and name=systemd lines are read.
		{"non-systemd v1 controller", "4:cpu,cpuacct:/system.slice/foo.service\n", "", false},
		{"v1 systemd line", "1:name=systemd:/system.slice/urnetwork-native.service\n", "urnetwork-native.service", false},
		{"v1 controller then systemd", "4:cpu:/system.slice/wrong.service\n1:name=systemd:/system.slice/right.service\n", "right.service", false},
		{"slash in a non-path field", "0:/x.service:\n", "", false},
		{"no leading slash", "0::system.slice/foo.service\n", "", false},
		{"missing hierarchy id", ":name=systemd:/system.slice/foo.service\n", "", false},
		{"bare suffix", "0::/system.slice/.service\n", "", false},
	}
	for _, c := range cases {
		unit, user := unitFromCgroup(c.cg)
		if unit != c.unit || user != c.user {
			t.Errorf("%s: got (%q,%v), want (%q,%v)", c.name, unit, user, c.unit, c.user)
		}
	}
}
