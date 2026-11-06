package urnettools

import (
	"fmt"
	"slices"
	"strings"
)

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

// serviceOwnership is the answer to "does the saved service run this
// provider?". Unknown is its own answer: a lookup that failed must not read as
// "not owned", or plain stop would kill a miner the supervisor brings back,
// and plain start would run a second provider beside it.
type serviceOwnership int

const (
	serviceNotOwned serviceOwnership = iota
	serviceOwned
	serviceOwnershipUnknown
)

// providerServiceOwnership is what the saved service lookup found for one
// provider.
type providerServiceOwnership struct {
	State serviceOwnership
	// Installed is whether a saved service exists at all.
	Installed bool
	// Err is why ownership is unknown.
	Err error
	// the saved service, and the pid of its running supervisor (0 when none)
	Executable    string
	StateDir      string
	LogPath       string
	SupervisorPid int
	// MinerPid is the service's miner, 0 when it can't be told. ServicePids
	// are the provider processes running under the supervisor, the miner
	// among them. A stop must see every one of them end.
	MinerPid    int
	ServicePids []int
}

func (o providerServiceOwnership) owned() bool { return o.State == serviceOwned }

// serviceProcessEntry is one process in a process snapshot.
type serviceProcessEntry struct {
	Parent int
	Name   string
}

// serviceLookup is the raw result of reading the saved service and the
// process table. The Windows side fills it; classifyServiceOwnership decides,
// so the decision is tested on every platform.
type serviceLookup struct {
	// NotInstalled: no saved service (its config or the profile folder is
	// missing). Any other load failure is in LoadErr.
	NotInstalled bool
	LoadErr      error

	Executable string
	StateDir   string
	LogPath    string

	SupervisorPid int
	SupervisorErr error

	// read only while a supervisor runs
	Processes    map[int]serviceProcessEntry
	ProcessesErr error
	// the miner pid the supervisor recorded, 0 when none
	RecordedMinerPid int
}

func classifyServiceOwnership(p Provider, l serviceLookup) providerServiceOwnership {
	o := providerServiceOwnership{Installed: !l.NotInstalled}
	unknown := func(err error) providerServiceOwnership {
		o.State, o.Err = serviceOwnershipUnknown, err
		return o
	}
	if l.NotInstalled {
		return o
	}
	if l.LoadErr != nil {
		return unknown(fmt.Errorf("read the provider service config: %w", l.LoadErr))
	}
	o.Executable, o.StateDir, o.LogPath = l.Executable, l.StateDir, l.LogPath
	if l.SupervisorErr != nil {
		return unknown(fmt.Errorf("find the running %s service: %w", windowsProviderServiceName, l.SupervisorErr))
	}
	o.SupervisorPid = l.SupervisorPid
	if l.ProcessesErr != nil {
		return unknown(l.ProcessesErr)
	}
	parentOf := func(pid int) (int, bool) {
		entry, ok := l.Processes[pid]
		return entry.Parent, ok
	}
	if !serviceOwnsProvider(p, l.Executable, l.StateDir, l.SupervisorPid, parentOf) {
		return o
	}
	o.State = serviceOwned
	if l.SupervisorPid <= 0 {
		return o
	}
	under := func(pid int) bool {
		return pid != l.SupervisorPid && processDescendsFrom(pid, l.SupervisorPid, parentOf)
	}
	exeName := l.Executable[strings.LastIndexAny(l.Executable, `\/`)+1:]
	for pid, entry := range l.Processes {
		if under(pid) && (isProviderArg(entry.Name) || strings.EqualFold(entry.Name, exeName)) {
			o.ServicePids = append(o.ServicePids, pid)
		}
	}
	slices.Sort(o.ServicePids)
	// the recorded miner, unless it has exited (or its id was reused);
	// else the selected provider when it is the miner and not the supervisor
	switch {
	case under(l.RecordedMinerPid):
		o.MinerPid = l.RecordedMinerPid
	case under(p.PID):
		o.MinerPid = p.PID
	}
	if o.MinerPid > 0 && !slices.Contains(o.ServicePids, o.MinerPid) {
		o.ServicePids = append(o.ServicePids, o.MinerPid)
		slices.Sort(o.ServicePids)
	}
	return o
}

// ownershipUnknownError is the refusal when the saved service could not be
// read. Nothing has been changed at that point.
func ownershipUnknownError(action string, p Provider, o providerServiceOwnership) error {
	return fmt.Errorf("could not %s provider %s: could not tell whether the %s service runs it: %v. Nothing was changed. Fix that, then run \"urnet-tools %s\" again", action, providerLabel(p), windowsProviderServiceName, o.Err, action)
}

// colocatedLiveProvider finds a running provider that shares p's binary or
// state dir, other than p itself and the processes of the service that owns
// p. Uninstall removes that binary and state dir, so it would break such a
// provider; discovery folds same-state-dir rows together, so the caller
// passes the raw process scan.
func colocatedLiveProvider(p Provider, owner providerServiceOwnership, running []Provider) (Provider, bool) {
	for _, other := range running {
		if !other.Running || other.PID <= 0 || other.PID == p.PID {
			continue
		}
		if owner.owned() && (other.PID == owner.SupervisorPid || slices.Contains(owner.ServicePids, other.PID)) {
			continue
		}
		if sameWindowsPath(other.Binary, p.Binary) || sameWindowsPath(other.StateDir, p.StateDir) {
			return other, true
		}
	}
	return Provider{}, false
}
