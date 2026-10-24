//go:build linux

package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The kernel's own layout for the entries the audit reads, written out here
// instead of being derived from the production strings. The old fixtures built
// their tree from the same relative keys the reader joined, so a path that was
// wrong in both places — the missing "sys/" segment this port shipped once —
// passed the test and failed against a real host. These literals are the
// independent side of the comparison; the production side lives in
// hostAuditProcFiles.
const (
	hostAuditCanonicalConntrackMax     = "/proc/sys/net/netfilter/nf_conntrack_max"
	hostAuditCanonicalConntrackTimeout = "/proc/sys/net/netfilter/nf_conntrack_tcp_timeout_established"
	hostAuditCanonicalMeminfo          = "/proc/meminfo"
)

// hostAuditProcFixture builds a fake /proc from absolute paths the test wrote
// down itself: the fixture is keyed by where the kernel publishes a file, not
// by the string the reader joins, so the reader and the fixture have to agree
// for the read to succeed.
func hostAuditProcFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for absolute, content := range files {
		relative, ok := strings.CutPrefix(absolute, hostAuditProcFS+"/")
		if !ok {
			t.Fatalf("fixture path %q is not under %s", absolute, hostAuditProcFS)
		}
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func withHostAuditProcRoot(t *testing.T, root string) {
	t.Helper()
	old := hostAuditProcRoot
	hostAuditProcRoot = root
	t.Cleanup(func() { hostAuditProcRoot = old })
}

func withHostAuditFileLimit(t *testing.T, limit *uint64) {
	t.Helper()
	old := hostAuditFileLimit
	hostAuditFileLimit = func() *uint64 { return limit }
	t.Cleanup(func() { hostAuditFileLimit = old })
}

// hostAuditProcPath resolves a relative procfs path the way the reader does.
func hostAuditProcPath(relative string) string {
	return filepath.Join(hostAuditProcFS, relative)
}

// The reader's paths have to be the kernel's paths, not merely self-consistent.
// Each case compares the production relative path with an absolute path written
// down in this file, then — when the host actually publishes that entry — runs
// the real reader against the real /proc, which is the check a fixture cannot
// make.
func TestHostAuditProcPathsMatchTheKernelLayout(t *testing.T) {
	cases := []struct {
		name     string
		relative string
		absolute string
		observed func(hostAuditObservations) bool
	}{
		{"nf_conntrack_max", hostAuditProcFiles.ConntrackMax, hostAuditCanonicalConntrackMax,
			func(obs hostAuditObservations) bool { return obs.ConntrackMax != nil }},
		{"nf_conntrack_tcp_timeout_established", hostAuditProcFiles.ConntrackTimeout, hostAuditCanonicalConntrackTimeout,
			func(obs hostAuditObservations) bool { return obs.ConntrackTimeoutEstablished != nil }},
		{"meminfo", hostAuditProcFiles.Meminfo, hostAuditCanonicalMeminfo,
			func(obs hostAuditObservations) bool { return obs.ConntrackMaxTarget != nil }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := hostAuditProcPath(testCase.relative); got != testCase.absolute {
				t.Fatalf("reader path %q resolves to %q, want the kernel's %q", testCase.relative, got, testCase.absolute)
			}
			if _, err := os.Stat(testCase.absolute); err != nil {
				t.Skipf("this host does not publish %s: %v", testCase.absolute, err)
			}
			withHostAuditProcRoot(t, hostAuditProcFS)
			withHostAuditFileLimit(t, nil)
			if obs := gatherHostAuditObservations(); !testCase.observed(obs) {
				t.Fatalf("the reader found nothing at %s on a host that publishes it: observations %+v", testCase.absolute, obs)
			}
		})
	}
}

// Proof that the guard and the fixture have teeth. The historical bug was a
// reader path missing its "sys/" segment while the fixture agreed with it; both
// halves of that are exercised here against the bad path.
func TestHostAuditFixtureCatchesAWrongProductionPath(t *testing.T) {
	const historical = "net/netfilter/nf_conntrack_max"

	// The guard rejects the path the old reader used, even though it is
	// self-consistent with the fixture the old test built.
	if got := hostAuditProcPath(historical); got == hostAuditCanonicalConntrackMax {
		t.Fatalf("the path guard cannot tell %q from %q", historical, hostAuditCanonicalConntrackMax)
	}

	// And a fixture built from the kernel's layout does not satisfy a reader
	// pointed at the bad path: it looks for a file the fixture never wrote.
	root := hostAuditProcFixture(t, map[string]string{
		hostAuditCanonicalConntrackMax: "131072\n",
	})
	oldFiles := hostAuditProcFiles
	hostAuditProcFiles.ConntrackMax = historical
	t.Cleanup(func() { hostAuditProcFiles = oldFiles })
	withHostAuditProcRoot(t, root)
	withHostAuditFileLimit(t, nil)

	if obs := gatherHostAuditObservations(); obs.ConntrackMax != nil {
		t.Fatalf("a fixture built from the kernel path satisfied the wrong reader path %q: observations %+v", historical, obs)
	}
}

// A host that is short on every limit is read from /proc and graded with the
// values that were actually there.
func TestGatherHostAuditObservationsGradesAFixtureHost(t *testing.T) {
	root := hostAuditProcFixture(t, map[string]string{
		hostAuditCanonicalConntrackMax:     "131072\n",
		hostAuditCanonicalConntrackTimeout: "432000\n",
		// 32 GiB exactly is the first bracket that asks for 4M entries.
		hostAuditCanonicalMeminfo: "MemTotal:       33554432 kB\n",
	})
	withHostAuditProcRoot(t, root)
	withHostAuditFileLimit(t, hostAuditUint64Ptr(1024))

	obs := gatherHostAuditObservations()
	if obs.FdLimit == nil || *obs.FdLimit != 1024 {
		t.Fatalf("FdLimit = %v, want 1024", obs.FdLimit)
	}
	if obs.ConntrackMax == nil || *obs.ConntrackMax != 131072 {
		t.Fatalf("ConntrackMax = %v, want 131072", obs.ConntrackMax)
	}
	if obs.ConntrackMaxTarget == nil || *obs.ConntrackMaxTarget != 4194304 {
		t.Fatalf("ConntrackMaxTarget = %v, want 4194304 (32 GiB)", obs.ConntrackMaxTarget)
	}
	if obs.ConntrackTimeoutEstablished == nil || *obs.ConntrackTimeoutEstablished != 432000 {
		t.Fatalf("ConntrackTimeoutEstablished = %v, want 432000", obs.ConntrackTimeoutEstablished)
	}

	want := []hostAuditFinding{
		{Check: "File Descriptors", Observed: "1024", Expected: "1048576+"},
		{Check: "Conntrack Max", Observed: "131072", Expected: "4194304"},
		{Check: "Conntrack Timeout", Observed: "432000s", Expected: "3600s"},
	}
	findings := auditHost(obs)
	if len(findings) != len(want) {
		t.Fatalf("findings = %v, want %v", findings, want)
	}
	for i := range want {
		if findings[i] != want[i] {
			t.Errorf("finding[%d] = %+v, want %+v", i, findings[i], want[i])
		}
	}
}

// A host whose every value is unreadable is unobservable, not unhealthy: the
// audit must not turn a missing /proc entry into a warning about a zero.
func TestGatherHostAuditObservationsIsSilentAboutWhatItCannotRead(t *testing.T) {
	withHostAuditProcRoot(t, t.TempDir())
	withHostAuditFileLimit(t, nil)

	obs := gatherHostAuditObservations()
	if obs.FdLimit != nil || obs.ConntrackMax != nil || obs.ConntrackMaxTarget != nil || obs.ConntrackTimeoutEstablished != nil {
		t.Fatalf("observations = %+v, want every field unobservable", obs)
	}
	if findings := auditHost(obs); len(findings) != 0 {
		t.Fatalf("unreadable host produced findings %v", findings)
	}
}

// A conntrack max that is readable but has no RAM-derived target (MemTotal
// unreadable) is left alone rather than graded against a made-up number.
func TestGatherHostAuditObservationsWithoutMemTotalHasNoConntrackTarget(t *testing.T) {
	root := hostAuditProcFixture(t, map[string]string{
		hostAuditCanonicalConntrackMax: "1024\n",
	})
	withHostAuditProcRoot(t, root)
	withHostAuditFileLimit(t, nil)

	obs := gatherHostAuditObservations()
	if obs.ConntrackMax == nil || *obs.ConntrackMax != 1024 {
		t.Fatalf("ConntrackMax = %v, want 1024", obs.ConntrackMax)
	}
	if obs.ConntrackMaxTarget != nil {
		t.Fatalf("ConntrackMaxTarget = %v, want nil without MemTotal", obs.ConntrackMaxTarget)
	}
	if findings := auditHost(obs); len(findings) != 0 {
		t.Fatalf("findings = %v, want none without a target", findings)
	}
}

// The real getrlimit path, which every other test replaces with the seam. A
// process always has a soft RLIMIT_NOFILE on Linux; zero would mean the read
// silently produced an empty limit and the fd check would never fire.
func TestHostAuditLiveFileLimitReadsTheProcess(t *testing.T) {
	limit := hostAuditLiveFileLimit()
	if limit == nil {
		t.Fatal("Getrlimit(RLIMIT_NOFILE) failed on Linux")
	}
	if *limit == 0 {
		t.Fatal("soft RLIMIT_NOFILE read as 0, want the process's real limit")
	}
}

// These brackets are the ones `urnet-tools optimize` writes
// (internal/urnettools/legacy_cmds.go conntrackMaxForRAMKB). The hint names
// that command, so a change here that is not a change there is a regression:
// the warning would keep firing after the operator ran the fix.
func TestHostAuditConntrackMaxForRAMMatchesOptimizeBrackets(t *testing.T) {
	const mb = int64(1024)
	cases := []struct {
		ramKB int64
		want  int64
	}{
		{512 * mb, 131072},
		{1024*mb - 1, 131072},
		{1024 * mb, 262144},
		{4*1024*mb - 1, 262144},
		{4 * 1024 * mb, 524288},
		{8*1024*mb - 1, 524288},
		{8 * 1024 * mb, 1048576},
		{16*1024*mb - 1, 1048576},
		{16 * 1024 * mb, 2097152},
		{32*1024*mb - 1, 2097152},
		{32 * 1024 * mb, 4194304},
		{128 * 1024 * mb, 4194304},
	}
	for _, testCase := range cases {
		if got := hostAuditConntrackMaxForRAM(testCase.ramKB); got != testCase.want {
			t.Errorf("hostAuditConntrackMaxForRAM(%d kB) = %d, want %d", testCase.ramKB, got, testCase.want)
		}
	}
}
