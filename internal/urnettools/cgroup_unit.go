package urnettools

import "strings"

// cgroupLinePath returns the path of a /proc/<pid>/cgroup line that can name
// a systemd unit: the cgroup v2 unified line ("0::/path") or the v1 systemd
// hierarchy ("N:name=systemd:/path"). Other v1 controller lines and anything
// malformed return ok=false, so a path is only ever read from a line that is
// known to carry one.
func cgroupLinePath(line string) (path string, ok bool) {
	f := strings.SplitN(strings.TrimSpace(line), ":", 3)
	if len(f) != 3 || f[0] == "" || !strings.HasPrefix(f[2], "/") {
		return "", false
	}
	if (f[0] == "0" && f[1] == "") || f[1] == "name=systemd" {
		return f[2], true
	}
	return "", false
}

// isWorkloadUnitComponent reports whether a cgroup path component can be the
// unit a process runs in. user@<uid>.service is the per-user systemd manager
// that parents every user unit; it is infrastructure, never the workload.
func isWorkloadUnitComponent(c string) bool {
	if !strings.HasSuffix(c, ".service") || c == ".service" {
		return false
	}
	return !strings.HasPrefix(c, "user@")
}

// unitFromCgroup returns the .service unit that owns a process, taken from
// the contents of /proc/<pid>/cgroup, and whether that unit lives in a user
// manager (its path runs through user@<uid>.service). The innermost workload
// .service path component is the process's own unit. Returns "" when none is
// present. Pure text parsing, so it behaves identically on every build target
// (off Linux there is simply no cgroup file to feed it).
func unitFromCgroup(cg string) (unit string, user bool) {
	for _, line := range strings.Split(cg, "\n") {
		path, ok := cgroupLinePath(line)
		if !ok {
			continue
		}
		comps := strings.Split(path, "/")
		for j := len(comps) - 1; j >= 0; j-- {
			if !isWorkloadUnitComponent(comps[j]) {
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
