//go:build !linux

package urnettools

// unitFromCgroup has no cgroup to read off Linux, so no unit can be named.
// Non-Linux callers (legacy_cmds.go) get the same "unknown" answer as a
// Linux process outside any .service: an empty unit and not a user unit.
func unitFromCgroup(cg string) (unit string, user bool) {
	return "", false
}
