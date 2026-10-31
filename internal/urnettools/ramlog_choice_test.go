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

// Both selection paths apply the same ownership rule: when it says the shared
// buffer is ambiguous, neither returns it; when it says it is owned, both do.
func TestRamlogSelectionPathsAgree(t *testing.T) {
	p := Provider{PID: 10, User: "u", Binary: "/opt/a/provider", Running: true}
	rival := Provider{PID: 11, User: "v", Binary: "/opt/b/provider", Running: true}
	cases := []struct {
		name string
		all  []Provider
		open map[int][]string
		want string
	}{
		{"ambiguous", []Provider{p, rival}, nil, ""},
		{"sole candidate", []Provider{p}, nil, sharedRamlogPath},
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
