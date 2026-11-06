package urnettools

import "strings"

// The Windows supervisor ("service run urnetwork-provider") is per user, and
// a bare provider (started by hand, or by an older install) can run next to
// it from the very same executable. Discovery sees both as providers, so a
// matching binary path proves nothing about a RUNNING process. A running
// provider belongs to the service only when it is the supervisor or one of
// its descendants (the miner). A stopped provider has no process to test, so
// the saved service config is the only evidence, and it must match on both
// the executable and the state dir: two installs can share one executable
// while each keeps its own state.

// maxProcessAncestry bounds the parent walk. Windows parent ids can be reused
// and form cycles, so the walk must terminate on its own.
const maxProcessAncestry = 64

// processDescendsFrom reports whether pid is ancestor or runs below it.
// parentOf returns a process's parent id, and false when the process is
// unknown (already gone).
func processDescendsFrom(pid, ancestor int, parentOf func(int) (int, bool)) bool {
	if pid <= 0 || ancestor <= 0 {
		return false
	}
	for i := 0; i < maxProcessAncestry; i++ {
		if pid == ancestor {
			return true
		}
		parent, ok := parentOf(pid)
		if !ok || parent <= 0 || parent == pid {
			return false
		}
		pid = parent
	}
	return false
}

// sameWindowsPath compares two Windows paths the way the filesystem does:
// case-insensitively, with either slash.
func sameWindowsPath(a, b string) bool {
	norm := func(s string) string {
		return strings.TrimRight(strings.ToLower(strings.ReplaceAll(s, "/", `\`)), `\`)
	}
	return a != "" && b != "" && norm(a) == norm(b)
}

// serviceOwnsProvider decides whether the saved service (config executable
// and state dir, and the pid of its running supervisor, 0 when none) owns the
// selected provider.
func serviceOwnsProvider(p Provider, configExecutable, configStateDir string, supervisorPid int, parentOf func(int) (int, bool)) bool {
	if p.PID > 0 {
		return processDescendsFrom(p.PID, supervisorPid, parentOf)
	}
	return !p.Running && sameWindowsPath(p.Binary, configExecutable) && sameWindowsPath(p.StateDir, configStateDir)
}
