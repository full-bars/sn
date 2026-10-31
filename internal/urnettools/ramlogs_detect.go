package urnettools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ramlogsFreshWindow is how recently the RAM buffer must have been written for
// its existence to count as evidence. The provider syncs it twice a second
// while running, so a live redirect is always well inside this. The window
// exists so a stale file left behind by a provider that has since been
// reconfigured, or by a container that no longer runs, is not mistaken for an
// active redirect.
const ramlogsFreshWindow = 2 * time.Minute

// ramlogsEnvKey is the environment variable the provider itself reads to
// decide whether to redirect its output into /dev/shm.
const ramlogsEnvKey = "URNETWORK_RAMLOGS"

// sharedRamlogPath is the buffer older builds and some launches write regardless
// of the binary name.
const sharedRamlogPath = "/dev/shm/urnetwork.log"

// ramLogPathsFor returns the RAM buffer paths to consider for a provider,
// newest convention first. The provider writes /dev/shm/<binary basename>.log
// so multi-provider boxes do not conflate outputs, and older builds wrote the
// shared /dev/shm/urnetwork.log.
func ramLogPathsFor(p Provider) []string {
	var paths []string
	if p.Binary != "" {
		paths = append(paths, "/dev/shm/"+filepath.Base(p.Binary)+".log")
	}
	// Only include the legacy shared path when no binary is specified.
	// When the binary basename is "urnetwork" the per-binary path above
	// already matches it; any other named binary must not fall back to the
	// shared path because that causes multi-provider false positives.
	if p.Binary == "" {
		paths = append(paths, sharedRamlogPath)
	}
	return paths
}

// ramlogPathsOpenByProcess returns the /dev/shm/*.log files the running
// provider actually has open. The binary basename is only a guess at the
// buffer name: a provider names its buffer from its own settings, so a binary
// called "provider" can still write the shared /dev/shm/urnetwork.log, and a
// guess-only check then reports "no ramlog" while the buffer is live. The
// process itself is the fact, and /proc/<pid>/fd needs no privileges for the
// provider's own user.
func ramlogPathsOpenByProcess(pid int) []string {
	if pid <= 0 {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
	if err != nil {
		return nil
	}
	seen := make(map[string]bool, len(entries))
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		dest, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", entry.Name()))
		if err != nil {
			continue
		}
		if !strings.HasPrefix(dest, "/dev/shm/") || !strings.HasSuffix(dest, ".log") {
			continue
		}
		if seen[dest] {
			continue
		}
		seen[dest] = true
		paths = append(paths, dest)
	}
	return paths
}

// ramlogProbe bundles the filesystem facts the buffer-selection rules read, so
// the rules can be exercised without writing into the host's /dev/shm or
// reading another process's descriptors.
type ramlogProbe struct {
	fresh  func(path string, running bool) bool
	open   func(pid int) []string
	exists func(path string) bool
}

var liveRamlogProbe = ramlogProbe{
	fresh:  ramlogFileFresh,
	open:   ramlogPathsOpenByProcess,
	exists: pathExists,
}

// ramlogStreamPath returns the buffer to stream for a provider, or "" when no
// evidence supports any buffer. It follows the same order as ramlogFileActive so
// the file that was detected as active is the file that gets streamed: the
// per-binary name first, then whatever the running process actually has open.
// The shared legacy path is returned only when this provider owns it under the
// same rule ramlogStreamChoice uses (ramlogSharedOwnedByOpen); it is never a
// default, because handing it back unowned streams another provider's output.
func ramlogStreamPath(p Provider, all []Provider) string {
	return ramlogStreamPathWith(p, all, liveRamlogProbe)
}

func ramlogStreamPathWith(p Provider, all []Provider, probe ramlogProbe) string {
	for _, path := range ramLogPathsFor(p) {
		// The shared path is not a per-binary name, so a fresh file there is
		// not evidence about this provider; ownership below decides it.
		if path != sharedRamlogPath && probe.fresh(path, p.Running) {
			return path
		}
	}
	for _, path := range probe.open(p.PID) {
		if probe.fresh(path, p.Running) {
			return path
		}
	}
	if p.Binary != "" {
		if path := "/dev/shm/" + filepath.Base(p.Binary) + ".log"; path != sharedRamlogPath && probe.exists(path) {
			return path
		}
	}
	if ramlogSharedOwnedByOpen(p, all, probe.fresh, probe.open) {
		return sharedRamlogPath
	}
	return ""
}

// pathExists reports whether path exists at all, without the freshness rules.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ramlogSharedFallbackActive reports whether the legacy shared buffer is this
// provider's log even though its binary name does not match it.
//
// This happens when the provider redirects to the shared path regardless of its
// name, and the mismatch has a second consequence: a binary carrying file
// capabilities runs non-dumpable, so /proc/<pid>/fd is root-only and the process
// cannot be asked which file it holds. The basename is the only hint left, and it
// is wrong.
//
// The shared buffer is ambiguous when more than one running provider could be
// writing it, so it is used only when exactly one could. A provider with a fresh
// buffer of its own is writing that buffer and is not a candidate; what makes the
// shared buffer ambiguous is a second provider that likewise has no buffer of its
// own, because either of them could be the writer.
func ramlogSharedFallbackActive(p Provider, all []Provider) bool {
	return ramlogSharedOwnedBy(p, all, func(path string, running bool) bool {
		return ramlogFileFresh(path, running)
	})
}

// ramlogSharedOwnedBy is ramlogSharedFallbackActive with the freshness test
// injected, so the ownership rule can be exercised without writing into the
// host's /dev/shm.
func ramlogSharedOwnedBy(p Provider, all []Provider, fresh func(path string, running bool) bool) bool {
	return ramlogSharedOwnedByOpen(p, all, fresh, ramlogPathsOpenByProcess)
}

// ramlogSharedOwnedByOpen is the single ownership rule for the shared buffer;
// ramlogStreamChoice and ramlogStreamPath both reach it through here.
func ramlogSharedOwnedByOpen(p Provider, all []Provider, fresh func(path string, running bool) bool, open func(int) []string) bool {
	if !p.Running {
		return false
	}
	shared := sharedRamlogPath
	if !fresh(shared, true) {
		return false
	}
	candidates := 0
	soleCandidate := false
	for _, other := range all {
		if !other.Running {
			continue
		}
		opened := open(other.PID)
		if !sameProvider(other, p) {
			// An open shared buffer blocks the claim even if another buffer is fresh.
			for _, path := range opened {
				if path == shared {
					return false
				}
			}
		}
		if ramlogHasOwnBuffer(other, fresh, opened, shared) {
			continue
		}
		candidates += 1
		if sameProvider(other, p) {
			soleCandidate = true
		}
	}
	return candidates == 1 && soleCandidate
}

// ramlogHasOwnBuffer reports whether a provider writes a buffer of its own
// rather than the shared one: a fresh per-binary file, or any non-shared
// /dev/shm/*.log it holds open. An open descriptor settles it regardless of
// write freshness, since a holder that looks idle inside the window still has
// somewhere other than the shared file to write.
func ramlogHasOwnBuffer(p Provider, fresh func(path string, running bool) bool, opened []string, shared string) bool {
	for _, path := range opened {
		if path != shared {
			return true
		}
	}
	return ramlogHasOwnFresh(p, fresh, shared)
}

// ramlogHasOwnFresh reports whether a provider is writing a buffer of its own
// rather than the shared one.
func ramlogHasOwnFresh(p Provider, fresh func(path string, running bool) bool, shared string) bool {
	for _, path := range ramLogPathsFor(p) {
		if path != shared && fresh(path, p.Running) {
			return true
		}
	}
	return false
}

// sameProvider reports whether two discoveries describe the same provider. PIDs
// are reused and can be zero for a provider that is not running, so fall back to
// the identity discovery already proved.
func sameProvider(a Provider, b Provider) bool {
	if 0 < a.PID && 0 < b.PID {
		return a.PID == b.PID
	}
	return a.User == b.User && a.Binary == b.Binary
}

// ramlogStreamChoice decides whether a RAM buffer should be streamed and which
// one. When the shared buffer is this provider's, that is the file to stream: the
// per-binary name is missing or stale, which is why the fallback was needed, so
// preferring it here would silently follow an unrelated or dead file.
func ramlogStreamChoice(p Provider, all []Provider) (bool, string) {
	return ramlogStreamChoiceWith(p, all, liveRamlogProbe, providerUsesRamlogs)
}

func ramlogStreamChoiceWith(p Provider, all []Provider, probe ramlogProbe, usesRamlogs func(Provider) bool) (bool, string) {
	if ramlogSharedOwnedByOpen(p, all, probe.fresh, probe.open) {
		return true, sharedRamlogPath
	}
	if usesRamlogs(p) {
		// No evidence for any buffer means the journal, not a guessed file.
		if path := ramlogStreamPathWith(p, all, probe); path != "" {
			return true, path
		}
	}
	return false, ""
}

// ramlogFileActive reports whether a RAM buffer for this provider exists and
// has been written to recently.
//
// This is the provider's own observable behavior rather than an inference from
// stored configuration, which is why it is consulted before the environment:
// whatever the config says, a file being appended to twice a second is where
// the logs are actually going. Only meaningful for a running provider, since a
// stopped one is not writing anywhere.
func ramlogFileActive(p Provider) bool {
	for _, path := range ramLogPathsFor(p) {
		if ramlogFileFresh(path, p.Running) {
			return true
		}
	}
	// The buffer name above is derived from the binary basename, which only
	// matches when the provider happens to name its buffer after its binary.
	// Ask the process instead: a running provider has its buffer open, and that
	// answers the question regardless of naming.
	for _, path := range ramlogPathsOpenByProcess(p.PID) {
		if ramlogFileFresh(path, p.Running) {
			return true
		}
	}
	return false
}

// ramlogFileFresh holds the decision rules for one path so they can be tested
// without writing into the host's /dev/shm: the provider must be running, the
// buffer must exist with content, and it must have been written to inside
// ramlogsFreshWindow.
func ramlogFileFresh(path string, running bool) bool {
	if !running {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() || fi.Size() == 0 {
		return false
	}
	elapsed := time.Since(fi.ModTime())
	// Guard against clock skew: a future ModTime yields a negative elapsed.
	return elapsed >= 0 && elapsed <= ramlogsFreshWindow
}

// unitEnvironment returns the unit's effective Environment= assignments, which
// cover the unit body and every drop-in. It deliberately does not try to read
// EnvironmentFile= contents; systemd does not expand those into this property,
// and the file check above already covers a provider that is redirecting for
// any reason at all.
func unitEnvironment(p Provider) (string, error) {
	if p.Unit == "" {
		return "", nil
	}
	var args []string
	if isUserUnit(p.Unit) {
		args = append([]string{"systemctl"}, systemctlUserArgs(p.User)...)
		args = append(args, "show", "-p", "Environment", "--value", p.Unit)
	} else {
		args = []string{"systemctl", "show", "-p", "Environment", "--value", p.Unit}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ramlogsEnvEnabled parses a systemd Environment= value and reports whether it
// turns RAMLOGS on. The property is a single space-separated line of KEY=VALUE
// pairs, with values quoted when they contain spaces.
//
// Override semantics: the last assignment to URNETWORK_RAMLOGS wins, matching
// how systemd overlays drop-in files on top of the unit body.
func ramlogsEnvEnabled(environment string) bool {
	enabled := false
	for _, field := range strings.Fields(environment) {
		field = strings.Trim(field, `"'`)
		key, value, found := strings.Cut(field, "=")
		if !found || strings.TrimSpace(key) != ramlogsEnvKey {
			continue
		}
		enabled = truthyOn(strings.ToLower(strings.Trim(strings.TrimSpace(value), `"'`)))
	}
	return enabled
}
