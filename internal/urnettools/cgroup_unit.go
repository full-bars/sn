package urnettools

import "strings"

// unitFromCgroup returns the .service unit that owns a process, taken from
// the contents of /proc/<pid>/cgroup, and whether that unit lives in a user
// manager (its path runs through user@<uid>.service). Only the cgroup v2
// ("0::/path") and v1 name=systemd paths carry a unit; the innermost .service
// path component is the process's own unit. Returns "" when none is present.
// Pure text parsing, so it behaves identically on every build target.
func unitFromCgroup(cg string) (unit string, user bool) {
	for _, line := range strings.Split(cg, "\n") {
		i := strings.Index(line, "/")
		if i < 0 {
			continue
		}
		comps := strings.Split(line[i:], "/")
		for j := len(comps) - 1; j >= 0; j-- {
			if !strings.HasSuffix(comps[j], ".service") {
				continue
			}
			for _, outer := range comps[:j] {
				if strings.HasPrefix(outer, "user@") && strings.HasSuffix(outer, ".service") {
					user = true
				}
			}
			return comps[j], user
		}
	}
	return "", false
}
