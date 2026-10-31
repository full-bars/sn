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
	}
	for _, c := range cases {
		unit, user := unitFromCgroup(c.cg)
		if unit != c.unit || user != c.user {
			t.Errorf("%s: got (%q,%v), want (%q,%v)", c.name, unit, user, c.unit, c.user)
		}
	}
}
