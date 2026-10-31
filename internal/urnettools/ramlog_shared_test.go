package urnettools

import "testing"

// freshness returns a predicate that treats exactly the given paths as fresh.
func freshness(paths ...string) func(string, bool) bool {
	fresh := make(map[string]bool, len(paths))
	for _, p := range paths {
		fresh[p] = true
	}
	return func(path string, running bool) bool {
		return running && fresh[path]
	}
}

// The shared buffer belongs to the only provider that could be writing it.
func TestRamlogSharedOwnedBySoleCandidate(t *testing.T) {
	p := Provider{PID: 10, User: "u", Binary: "/opt/a/provider", Running: true}
	// p has no buffer of its own; another provider does, so it is not a rival.
	other := Provider{PID: 11, User: "u", Binary: "/opt/b/other", Running: true}
	all := []Provider{p, other}

	ok := ramlogSharedOwnedBy(p, all, freshness(sharedRamlogPath, "/dev/shm/other.log"))
	if !ok {
		t.Fatal("sole candidate for a fresh shared buffer must own it")
	}
}

// A provider with its own fresh buffer is writing that buffer, not the shared
// one, so it must not disqualify the real owner. This is the inversion the first
// version of this change had.
func TestRamlogSharedNotDisqualifiedByProviderWithOwnBuffer(t *testing.T) {
	p := Provider{PID: 10, User: "u", Binary: "/opt/a/provider", Running: true}
	// a sibling with a distinct buffer name, so it writes its own file and is no
	// rival for the shared one
	selfHosted := Provider{PID: 11, User: "u", Binary: "/opt/b/other", Running: true}
	ok := ramlogSharedOwnedBy(p, []Provider{p, selfHosted}, freshness(sharedRamlogPath, "/dev/shm/other.log"))
	if !ok {
		t.Fatal("a provider writing its own buffer must not make the shared buffer ambiguous")
	}
}

// A sibling whose binary is literally "urnetwork" writes the shared path by name,
// so it owns that file and another provider must not claim it.
func TestRamlogSharedYieldsToBinaryNamedUrnetwork(t *testing.T) {
	p := Provider{PID: 10, User: "u", Binary: "/opt/a/provider", Running: true}
	named := Provider{PID: 11, User: "u", Binary: "/opt/b/urnetwork", Running: true}
	if ramlogSharedOwnedBy(p, []Provider{p, named}, freshness(sharedRamlogPath)) {
		t.Fatal("a provider whose buffer name is the shared path must win it")
	}
}

// Two providers that both lack a buffer of their own are indistinguishable, so
// neither may claim the shared buffer.
func TestRamlogSharedAmbiguousWithTwoCandidates(t *testing.T) {
	a := Provider{PID: 10, User: "u", Binary: "/opt/a/provider", Running: true}
	b := Provider{PID: 11, User: "u", Binary: "/opt/b/provider", Running: true}
	if ramlogSharedOwnedBy(a, []Provider{a, b}, freshness(sharedRamlogPath)) {
		t.Fatal("two candidates must not resolve to either owner")
	}
}

// A second candidate that is not the provider under test blocks the claim, even
// when the first candidate is the provider.
func TestRamlogSharedRequiresSoleCandidate(t *testing.T) {
	p := Provider{PID: 10, User: "u", Binary: "/opt/a/provider", Running: true}
	rival := Provider{PID: 11, User: "v", Binary: "/opt/b/provider", Running: true}
	if ramlogSharedOwnedBy(p, []Provider{rival, p}, freshness(sharedRamlogPath)) {
		t.Fatal("a rival candidate must block the claim")
	}
}

// A stopped provider has no log, and a stale buffer is not evidence of one.
func TestRamlogSharedNeedsRunningProviderAndFreshBuffer(t *testing.T) {
	p := Provider{PID: 10, User: "u", Binary: "/opt/a/provider", Running: true}
	if ramlogSharedOwnedBy(Provider{PID: 10, Running: false}, []Provider{p}, freshness(sharedRamlogPath)) {
		t.Fatal("a stopped provider must not claim the shared buffer")
	}
	if ramlogSharedOwnedBy(p, []Provider{p}, freshness()) {
		t.Fatal("a stale shared buffer must not be claimed")
	}
}

func TestRamlogSharedAccountsForOtherOpenBuffers(t *testing.T) {
	p := Provider{PID: 10, User: "u", Binary: "/opt/a/provider", Running: true}
	other := Provider{PID: 11, User: "v", Binary: "/opt/b/other", Running: true}
	cases := []struct {
		name  string
		open  []string
		fresh []string
		want  bool
	}{
		{"shared and named buffer open", []string{sharedRamlogPath, "/dev/shm/other.log"}, []string{sharedRamlogPath, "/dev/shm/other.log"}, false},
		{"custom buffer open", []string{"/dev/shm/custom.log"}, []string{sharedRamlogPath, "/dev/shm/custom.log"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			open := func(pid int) []string {
				if pid == other.PID {
					return tc.open
				}
				return nil
			}
			if got := ramlogSharedOwnedByOpen(p, []Provider{p, other}, freshness(tc.fresh...), open); got != tc.want {
				t.Errorf("ownership = %v, want %v", got, tc.want)
			}
		})
	}
}

// Discovery can report a provider without a usable pid, so identity falls back to
// user and binary rather than matching on a zero pid.
func TestSameProviderFallsBackToIdentity(t *testing.T) {
	a := Provider{User: "u", Binary: "/opt/a/provider"}
	b := Provider{User: "u", Binary: "/opt/a/provider"}
	c := Provider{User: "v", Binary: "/opt/a/provider"}
	if !sameProvider(a, b) {
		t.Fatal("same user and binary must be the same provider when pids are unknown")
	}
	if sameProvider(a, c) {
		t.Fatal("different users must not be the same provider")
	}
}
