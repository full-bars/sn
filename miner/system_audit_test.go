package miner

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// system_audit_test.go drives the host audit through observations, because a
// test cannot lower the RLIMIT of the process it runs in, and the values a
// fleet node has must not be baked into the expectations.

func hostAuditUint64Ptr(value uint64) *uint64 { return &value }
func hostAuditInt64Ptr(value int64) *int64    { return &value }

// hostAuditHostile is a host that fails every check.
func hostAuditHostile() hostAuditObservations {
	return hostAuditObservations{
		FdLimit:                     hostAuditUint64Ptr(1024),
		ConntrackMax:                hostAuditInt64Ptr(65536),
		ConntrackMaxTarget:          hostAuditInt64Ptr(2097152),
		ConntrackTimeoutEstablished: hostAuditInt64Ptr(432000),
	}
}

// withHostAuditStartup installs a fake reader and a captured log for one
// startup test, and restores the process-wide once afterwards so the next
// test (or the next -count run) is not skipped as "already audited".
func withHostAuditStartup(t *testing.T, gather func() hostAuditObservations) *bytes.Buffer {
	t.Helper()
	oldGather := hostAuditGather
	hostAuditGather = gather
	oldWriter := proxyManagerLogWriter
	buffer := &bytes.Buffer{}
	proxyManagerLogWriter = buffer
	hostAuditOnce = sync.Once{}
	t.Cleanup(func() {
		hostAuditGather = oldGather
		proxyManagerLogWriter = oldWriter
		hostAuditOnce = sync.Once{}
	})
	return buffer
}

// A failing check names the value the host has and the value it should have,
// which is the whole difference between an actionable warning and "suboptimal".
func TestHostAuditReportsObservedAndExpectedValues(t *testing.T) {
	obs := hostAuditHostile()

	findings := auditHost(obs)
	want := []hostAuditFinding{
		{Check: "File Descriptors", Observed: "1024", Expected: "1048576+"},
		{Check: "Conntrack Max", Observed: "65536", Expected: "2097152"},
		{Check: "Conntrack Timeout", Observed: "432000s", Expected: "3600s"},
	}
	if len(findings) != len(want) {
		t.Fatalf("findings = %v, want %v", findings, want)
	}
	for i := range want {
		if findings[i] != want[i] {
			t.Errorf("finding[%d] = %+v, want %+v", i, findings[i], want[i])
		}
	}

	var buffer bytes.Buffer
	if reported := reportHostAudit(&buffer, obs); reported != len(want) {
		t.Fatalf("reportHostAudit reported %d findings, want %d", reported, len(want))
	}
	output := buffer.String()
	for _, finding := range want {
		if !strings.Contains(output, finding.line()) {
			t.Errorf("output %q is missing %q", output, finding.line())
		}
	}
}

// A tuned host is silent: no lines, no hints, no count.
func TestHostAuditCleanHostReportsNothing(t *testing.T) {
	obs := hostAuditObservations{
		FdLimit:                     hostAuditUint64Ptr(hostAuditFdTarget),
		ConntrackMax:                hostAuditInt64Ptr(2097152),
		ConntrackMaxTarget:          hostAuditInt64Ptr(2097152),
		ConntrackTimeoutEstablished: hostAuditInt64Ptr(hostAuditConntrackTimeoutTarget),
	}
	if findings := auditHost(obs); len(findings) != 0 {
		t.Fatalf("clean host produced findings %v", findings)
	}
	var buffer bytes.Buffer
	if reported := reportHostAudit(&buffer, obs); reported != 0 {
		t.Fatalf("clean host reported %d findings, want 0", reported)
	}
	if buffer.Len() != 0 {
		t.Fatalf("clean host wrote %q, want nothing", buffer.String())
	}
}

// "Cannot read" is not "bad": a missing /proc entry, or a conntrack max that
// cannot be graded because MemTotal is unreadable, produces no warning.
func TestHostAuditUnreadableValuesAreSilent(t *testing.T) {
	if findings := auditHost(hostAuditObservations{}); len(findings) != 0 {
		t.Fatalf("unobservable host produced findings %v", findings)
	}

	obs := hostAuditObservations{ConntrackMax: hostAuditInt64Ptr(1)}
	if findings := auditHost(obs); len(findings) != 0 {
		t.Fatalf("conntrack max without a target produced findings %v", findings)
	}

	// The checks that are readable still grade while the others stay silent.
	obs.ConntrackTimeoutEstablished = hostAuditInt64Ptr(86400)
	findings := auditHost(obs)
	if len(findings) != 1 || findings[0].Check != "Conntrack Timeout" {
		t.Fatalf("findings = %v, want only the readable timeout check", findings)
	}
}

// The provider's gates are kept: at the gate is acceptable, one past it is
// not. These numbers are what an operator sees before a node stops accepting
// connections, so a drifting boundary is a real behavior change.
func TestHostAuditGates(t *testing.T) {
	cases := []struct {
		name string
		obs  hostAuditObservations
		want int
	}{
		{"fd at the floor is acceptable", hostAuditObservations{FdLimit: hostAuditUint64Ptr(hostAuditFdFloor)}, 0},
		{"fd below the floor", hostAuditObservations{FdLimit: hostAuditUint64Ptr(hostAuditFdFloor - 1)}, 1},
		{"timeout at four hours is acceptable", hostAuditObservations{ConntrackTimeoutEstablished: hostAuditInt64Ptr(hostAuditConntrackTimeoutLimit)}, 0},
		{"timeout above four hours", hostAuditObservations{ConntrackTimeoutEstablished: hostAuditInt64Ptr(hostAuditConntrackTimeoutLimit + 1)}, 1},
		{"conntrack at the RAM target", hostAuditObservations{ConntrackMax: hostAuditInt64Ptr(2097152), ConntrackMaxTarget: hostAuditInt64Ptr(2097152)}, 0},
		{"conntrack under the RAM target", hostAuditObservations{ConntrackMax: hostAuditInt64Ptr(2097151), ConntrackMaxTarget: hostAuditInt64Ptr(2097152)}, 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := len(auditHost(testCase.obs)); got != testCase.want {
				t.Fatalf("%s: %d findings, want %d", testCase.name, got, testCase.want)
			}
		})
	}
}

// Each hint names the fix that actually applies. `urnet-tools optimize` does
// not raise RLIMIT_NOFILE, so pointing a descriptor warning at it would send
// the operator to a command that leaves the warning firing.
func TestHostAuditHintsNameTheFixThatApplies(t *testing.T) {
	descriptors := strings.Join(hostAuditHints([]hostAuditFinding{{Check: "File Descriptors"}}, false), "")
	if !strings.Contains(descriptors, "LimitNOFILE") {
		t.Errorf("descriptor hint does not name the per-process fix: %q", descriptors)
	}
	if strings.Contains(descriptors, "urnet-tools optimize") {
		t.Errorf("descriptor hint points at optimize, which does not raise RLIMIT_NOFILE: %q", descriptors)
	}

	conntrack := []hostAuditFinding{{Check: "Conntrack Max"}, {Check: "Conntrack Timeout"}}
	hostHints := strings.Join(hostAuditHints(conntrack, false), "")
	if !strings.Contains(hostHints, "urnet-tools optimize") {
		t.Errorf("conntrack hint does not name the fix: %q", hostHints)
	}
	if strings.Contains(hostHints, "LimitNOFILE") {
		t.Errorf("conntrack hint names the descriptor fix: %q", hostHints)
	}

	containerHints := strings.Join(hostAuditHints(conntrack, true), "")
	if !strings.Contains(containerHints, "Container is isolated") {
		t.Errorf("container hint does not explain the isolation: %q", containerHints)
	}

	if hints := hostAuditHints(nil, false); len(hints) != 0 {
		t.Errorf("clean host produced hints %v", hints)
	}
}

// The startup path must not wait for the check: a wedged reader cannot hold up
// a provider launch. The launch is guarded by a package-level sync.Once, so the
// reset below is what lets the next test (or -count=2) run it again.
func TestStartupHostAuditDoesNotBlockStartup(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAudit := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAudit)

	entered := make(chan struct{})
	buffer := withHostAuditStartup(t, func() hostAuditObservations {
		close(entered)
		<-release // a read that never returns until the test lets it
		return hostAuditHostile()
	})

	launched := make(chan (<-chan struct{}), 1)
	go func() { launched <- startStartupHostAudit() }()
	var done <-chan struct{}
	select {
	case done = <-launched:
	case <-time.After(2 * time.Second):
		t.Fatal("startup waited for the host audit; the check must not block a launch")
	}
	if done == nil {
		t.Fatal("the audit was skipped without a reason")
	}

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the audit goroutine never ran")
	}
	releaseAudit()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the audit goroutine never finished")
	}
	if output := buffer.String(); !strings.Contains(output, "Suboptimal") {
		t.Fatalf("the failing check reported nothing: %q", output)
	}

	if again := startStartupHostAudit(); again != nil {
		t.Fatal("a second launch started another audit; the once must hold")
	}
}

// URNETWORK_SKIP_AUDIT=1 skips the check entirely, as it skips the disk half.
func TestStartupHostAuditHonoursSkip(t *testing.T) {
	t.Setenv("URNETWORK_SKIP_AUDIT", "1")
	read := false
	buffer := withHostAuditStartup(t, func() hostAuditObservations {
		read = true
		return hostAuditHostile()
	})
	if done := startStartupHostAudit(); done != nil {
		t.Fatal("URNETWORK_SKIP_AUDIT=1 still started the audit")
	}
	if read {
		t.Fatal("URNETWORK_SKIP_AUDIT=1 still read the host")
	}
	if buffer.Len() != 0 {
		t.Fatalf("URNETWORK_SKIP_AUDIT=1 wrote %q, want nothing", buffer.String())
	}
}

// A skip is not a run: the skipped startup must not consume hostAuditOnce, or
// a long-lived process whose first provide cycle ran with the variable set
// could never audit again. This is the bug where skip-once became
// skip-forever.
func TestStartupHostAuditSkipDoesNotPoisonTheOnce(t *testing.T) {
	t.Setenv("URNETWORK_SKIP_AUDIT", "1")
	buffer := withHostAuditStartup(t, func() hostAuditObservations {
		return hostAuditHostile()
	})

	if done := startStartupHostAudit(); done != nil {
		t.Fatal("URNETWORK_SKIP_AUDIT=1 still started the audit")
	}
	// The variable cleared, as it would be on the next provide cycle.
	if err := os.Unsetenv("URNETWORK_SKIP_AUDIT"); err != nil {
		t.Fatal(err)
	}

	done := startStartupHostAudit()
	if done == nil {
		t.Fatal("the skipped first run consumed hostAuditOnce; the audit can never run in this process")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the audit after the skip never finished")
	}
	if output := buffer.String(); !strings.Contains(output, "Suboptimal") {
		t.Fatalf("the audit after the skip reported nothing: %q", output)
	}
}

// A panic in the audit goroutine must not end the process: an unrecovered
// panic here would take the miner down during startup. The recovery still has
// to be visible, so the panic value goes to the audit's panic log rather than
// vanishing.
func TestStartupHostAuditRecoversFromAPanic(t *testing.T) {
	buffer := withHostAuditStartup(t, func() hostAuditObservations {
		panic("audit reader exploded")
	})
	panicLog := &bytes.Buffer{}
	oldLog := hostAuditPanicLog
	hostAuditPanicLog = panicLog
	t.Cleanup(func() { hostAuditPanicLog = oldLog })

	done := startStartupHostAudit()
	if done == nil {
		t.Fatal("the audit was skipped without a reason")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the panic left the audit channel open; a waiter would hang")
	}

	if !strings.Contains(panicLog.String(), "audit reader exploded") {
		t.Fatalf("the recovered panic was not logged: %q", panicLog.String())
	}
	if !strings.Contains(panicLog.String(), "[audit]") {
		t.Fatalf("the recovered panic is not greppable as an audit line: %q", panicLog.String())
	}
	if buffer.Len() != 0 {
		t.Fatalf("a panicking audit reported %q, want nothing", buffer.String())
	}
}

// The audit goroutine takes the writer it was started with. Reassigning the
// package variable afterwards is the race the snapshot exists to prevent, and
// the line must land in the writer that was installed when the audit started.
func TestStartupHostAuditReadsTheWriterOnce(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	buffer := withHostAuditStartup(t, func() hostAuditObservations {
		close(entered)
		<-release
		return hostAuditHostile()
	})

	done := startStartupHostAudit()
	if done == nil {
		t.Fatal("the audit was skipped without a reason")
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the audit goroutine never ran")
	}

	// A diagnostics path redirects the package writer while the audit is in
	// flight. The audit already took its snapshot.
	replacement := &bytes.Buffer{}
	proxyManagerLogWriter = replacement
	close(release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the audit goroutine never finished")
	}
	if !strings.Contains(buffer.String(), "Suboptimal") {
		t.Fatalf("the audit did not use the writer it started with: %q", buffer.String())
	}
	if replacement.Len() != 0 {
		t.Fatalf("the audit wrote through a writer installed after it started: %q", replacement.String())
	}
}

// A nil sink must not turn a finding into a nil-pointer panic. The fallback is
// stderr, so the test asserts the report happened rather than crashing.
func TestHostAuditReportWithNoWriterDoesNotPanic(t *testing.T) {
	if reported := reportHostAudit(nil, hostAuditHostile()); reported != 3 {
		t.Fatalf("reportHostAudit(nil, hostile) reported %d findings, want 3", reported)
	}
}

// The marker scan reports a container when any marker exists, and the live
// check agrees with the files this host actually has.
func TestHostAuditInContainerMarkers(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, ".dockerenv")
	absent := filepath.Join(dir, "absent")

	if hostAuditInContainerAt(nil) {
		t.Fatal("no markers reported a container")
	}
	if hostAuditInContainerAt([]string{absent}) {
		t.Fatal("a missing marker reported a container")
	}
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !hostAuditInContainerAt([]string{marker}) {
		t.Fatal("an existing marker was not detected")
	}
	// The second marker counts: a scan that only ever tests the first would
	// miss Podman.
	if !hostAuditInContainerAt([]string{absent, marker}) {
		t.Fatal("a marker in the second position was not detected")
	}

	want := false
	for _, live := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(live); err == nil {
			want = true
		}
	}
	if got := hostAuditInContainer(); got != want {
		t.Fatalf("hostAuditInContainer() = %v, want %v for this host", got, want)
	}
}
