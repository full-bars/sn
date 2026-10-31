package urnettools

import "testing"

func probeFor(fresh []string, open map[int][]string, exists ...string) ramlogProbe {
	ex := make(map[string]bool, len(exists))
	for _, p := range exists {
		ex[p] = true
	}
	return ramlogProbe{
		fresh:  freshness(fresh...),
		open:   func(pid int) []string { return open[pid] },
		exists: func(path string) bool { return ex[path] },
	}
}

func always(bool) func(Provider) bool { return func(Provider) bool { return true } }

// With the ramlogs env on but no evidence for any buffer, neither selection
// path may hand back the shared file.
func TestRamlogNoCandidateYieldsNoBuffer(t *testing.T) {
	p := Provider{PID: 10, User: "u", Binary: "/opt/a/provider", Running: true}
	rival := Provider{PID: 11, User: "v", Binary: "/opt/b/provider", Running: true}
	all := []Provider{p, rival}
	probe := probeFor([]string{sharedRamlogPath}, nil)

	if got := ramlogStreamPathWith(p, all, probe); got != "" {
		t.Fatalf("stream path = %q, want none", got)
	}
	if use, path := ramlogStreamChoiceWith(p, all, probe, always(true)); use || path != "" {
		t.Fatalf("choice = %v %q, want no buffer", use, path)
	}
}

// The provider itself holding a non-shared buffer means it is not a candidate
// for the shared one, even when that buffer is fresh.
func TestRamlogSelfHoldingNonSharedIsNotCandidate(t *testing.T) {
	p := Provider{PID: 10, User: "u", Binary: "/opt/a/provider", Running: true}
	open := func(pid int) []string {
		if pid == 10 {
			return []string{"/dev/shm/custom.log"}
		}
		return nil
	}
	fresh := freshness(sharedRamlogPath, "/dev/shm/custom.log")
	if ramlogSharedOwnedByOpen(p, []Provider{p}, fresh, open) {
		t.Fatal("a provider holding its own fresh buffer must not claim the shared one")
	}
}

// A holder of a non-shared buffer has its own buffer even if that buffer has
// not been written inside the freshness window.
func TestRamlogOtherHoldingStaleNonSharedHasOwn(t *testing.T) {
	p := Provider{PID: 10, User: "u", Binary: "/opt/a/provider", Running: true}
	other := Provider{PID: 11, User: "v", Binary: "/opt/b/provider", Running: true}
	open := func(pid int) []string {
		if pid == 11 {
			return []string{"/dev/shm/custom.log"}
		}
		return nil
	}
	// custom.log is stale: only the shared buffer is fresh.
	if !ramlogSharedOwnedByOpen(p, []Provider{p, other}, freshness(sharedRamlogPath), open) {
		t.Fatal("a stale-looking holder of a non-shared buffer must not be a rival")
	}
}

// Both selection paths apply the same ownership rule: when it says the shared
// buffer is ambiguous, neither returns it; when it says it is owned, both do.
func TestRamlogSelectionPathsAgree(t *testing.T) {
	p := Provider{PID: 10, User: "u", Binary: "/opt/a/provider", Running: true}
	rival := Provider{PID: 11, User: "v", Binary: "/opt/b/provider", Running: true}
	holder := Provider{PID: 11, User: "v", Binary: "/opt/b/provider", Running: true}
	cases := []struct {
		name string
		all  []Provider
		open map[int][]string
		want string
	}{
		{"ambiguous", []Provider{p, rival}, nil, ""},
		{"sole candidate", []Provider{p}, nil, sharedRamlogPath},
		{"rival holds own buffer", []Provider{p, holder}, map[int][]string{11: {"/dev/shm/custom.log"}}, sharedRamlogPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := probeFor([]string{sharedRamlogPath}, tc.open)
			path := ramlogStreamPathWith(p, tc.all, probe)
			use, choice := ramlogStreamChoiceWith(p, tc.all, probe, always(true))
			if path != tc.want {
				t.Errorf("stream path = %q, want %q", path, tc.want)
			}
			if choice != tc.want || use != (tc.want != "") {
				t.Errorf("choice = %v %q, want %q", use, choice, tc.want)
			}
		})
	}
}

// The two entry points must give the same answer for the same inputs, in
// precedence as well as in the ownership rule. This is the guard against the
// order of checks drifting apart between them again.
func TestRamlogEntryPointsAgreeOnPrecedence(t *testing.T) {
	staleBinary := "/dev/shm/provider.log"
	p := Provider{PID: 10, User: "u", Binary: "/opt/a/provider", Running: true}
	rival := Provider{PID: 11, User: "v", Binary: "/opt/b/other", Running: true}
	cases := []struct {
		name   string
		all    []Provider
		fresh  []string
		open   map[int][]string
		exists []string
		want   string
	}{
		{"stale binary buffer, sole owner of shared", []Provider{p}, []string{sharedRamlogPath}, nil, []string{staleBinary}, sharedRamlogPath},
		{"stale binary buffer, shared contested by rival", []Provider{p, rival}, []string{sharedRamlogPath}, nil, []string{staleBinary}, staleBinary},
		{"contested shared, both hold it", []Provider{p, rival}, []string{sharedRamlogPath}, map[int][]string{10: {sharedRamlogPath}, 11: {sharedRamlogPath}}, nil, ""},
		{"contested shared, only rival holds it", []Provider{p, rival}, []string{sharedRamlogPath}, map[int][]string{11: {sharedRamlogPath}}, nil, ""},
		{"provider holds only the shared descriptor", []Provider{p}, []string{sharedRamlogPath}, map[int][]string{10: {sharedRamlogPath}}, nil, sharedRamlogPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := probeFor(tc.fresh, tc.open, tc.exists...)
			path := ramlogStreamPathWith(p, tc.all, probe)
			use, choice := ramlogStreamChoiceWith(p, tc.all, probe, always(true))
			if path != tc.want {
				t.Errorf("stream path = %q, want %q", path, tc.want)
			}
			if choice != path || use != (path != "") {
				t.Errorf("choice = %v %q disagrees with stream path %q", use, choice, path)
			}
		})
	}
}
