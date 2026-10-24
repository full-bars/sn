package miner

import (
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

const testMib = connect.ByteCount(1024 * 1024)
const testGib = 1024 * testMib

// capturePlan builds a plan with importantLogf captured, so a test can assert
// on the warning without writing it to the ramlog and the events log.
func capturePlan(
	t *testing.T,
	explicitMaxMemory connect.ByteCount,
	hostByteCount connect.ByteCount,
	providerCount int,
) (providerMemoryPlan, string) {
	t.Helper()
	var plan providerMemoryPlan
	logged := captureImportantLog(t, func() {
		plan = newProviderMemoryPlan(explicitMaxMemory, hostByteCount, providerCount)
	})
	return plan, logged
}

// TestProviderMemoryPlanNoFlagDefault: without --max-memory the provider has
// no ceiling. Each provider's target is (4/5 x host) / (3 x count), the soft
// limit is 3 x count x target (four fifths of the host), and the process
// budget is the soft limit; never below the previous 20 MiB target; an unknown
// host keeps the previous 64 MiB. Where the floor lifts the target past the
// share the soft limit is the host heap instead, and that is warned about.
// Pools are untouched.
func TestProviderMemoryPlanNoFlagDefault(t *testing.T) {
	opts := parseArgsForTest(t, []string{"provide"})
	if explicit := parseProviderMaxMemory(opts); explicit != 0 {
		t.Fatalf("absent flag parsed as %d", explicit)
	}
	// the 80% composition, written out: four fifths of the host, a third of
	// that per provider
	heap := func(host connect.ByteCount) connect.ByteCount { return host * 4 / 5 }
	// a platform without a host memory reader (provider_host_memory_other.go:
	// FreeBSD, Windows) reports zero, so the plan bounds the soft limit with
	// the documented unknown-host assumption instead of the host
	assumedHeap := providerUnknownHostAssumedHostByteCount * 4 / 5
	for _, c := range []struct {
		host   connect.ByteCount
		count  int
		target connect.ByteCount
		soft   connect.ByteCount
	}{
		// unknown host: the previous default, unchanged while it fits the
		// documented assumption (8 x 192 MiB is still under its heap)
		{0, 1, 64 * testMib, 192 * testMib},
		{0, 0, 64 * testMib, 192 * testMib},
		{0, 4, 64 * testMib, 768 * testMib},
		{0, 8, 64 * testMib, 1536 * testMib},
		// and bounded once 3 x count x 64 MiB outgrows it
		{0, 9, 64 * testMib, assumedHeap},
		{0, 2000, 64 * testMib, assumedHeap},
		// a host reading whose 4/5 heap truncates to zero: no limit is
		// installed at all, and it is warned about rather than silent
		{1, 1, 20 * testMib, 0},
		// known hosts: four fifths of the host over three per provider, no cap
		{4 * testGib, 1, heap(4*testGib) / 3, heap(4*testGib) / 3 * 3},
		{4 * testGib, 4, heap(4*testGib) / 12, heap(4*testGib) / 12 * 12},
		// 8 GiB x 4/5 = 6871947673, over 3
		{8 * testGib, 1, 2290649224, 3 * 2290649224},
		{8 * testGib, 4, heap(8*testGib) / 12, heap(8*testGib) / 12 * 12},
		{32 * testGib, 1, heap(32*testGib) / 3, heap(32*testGib) / 3 * 3},
		{32 * testGib, 4, heap(32*testGib) / 12, heap(32*testGib) / 12 * 12},
		{256 * testGib, 1, heap(256*testGib) / 3, heap(256*testGib) / 3 * 3},
		{256 * testGib, 4, heap(256*testGib) / 12, heap(256*testGib) / 12 * 12},
		// 2 GiB x 4/5 / (3 x 16) = 34.13 MiB
		{2 * testGib, 16, heap(2*testGib) / 48, heap(2*testGib) / 48 * 48},
		// 8 GiB x 4/5 / (3 x 20) = 109.4 MiB, over the floor: unchanged
		{8 * testGib, 20, heap(8*testGib) / 60, heap(8*testGib) / 60 * 60},
		// the floor binds below these; the soft limit is the heap, never more
		{8 * testGib, 200, 20 * testMib, heap(8 * testGib)},
		{8 * testGib, 2000, 20 * testMib, heap(8 * testGib)},
		{1 * testGib, 50, 20 * testMib, heap(1 * testGib)},
		{128 * testMib, 1, heap(128*testMib) / 3, heap(128*testMib) / 3 * 3},
		{32 * testMib, 1, 20 * testMib, heap(32 * testMib)},
	} {
		plan, logged := capturePlan(t, 0, c.host, c.count)
		want := providerMemoryPlan{
			DeviceMemoryTargetByteCount: c.target,
			SoftLimitByteCount:          c.soft,
			MemoryBudgetByteCount:       c.soft,
		}
		if plan != want {
			t.Errorf("host %d count %d: plan %+v, want %+v", c.host, c.count, plan, want)
		}
		count := connect.ByteCount(max(1, c.count))
		overSubscribed := 0 < c.host &&
			providerRuntimeBytesPerLiveByte*count*c.target > heap(c.host)
		unknownHostBounded := c.host <= 0 &&
			assumedHeap < providerRuntimeBytesPerLiveByte*count*c.target
		switch {
		case overSubscribed:
			if !strings.Contains(logged, "over-subscribed") {
				t.Errorf("host %d count %d: over-subscribed plan did not warn: %q",
					c.host, c.count, logged)
			}
		case unknownHostBounded:
			if !strings.Contains(logged, "host memory unknown") {
				t.Errorf("host %d count %d: unknown-host bound did not warn: %q",
					c.host, c.count, logged)
			}
		case logged != "":
			t.Errorf("host %d count %d: unexpected warning %q", c.host, c.count, logged)
		}
		assertProviderMemoryPlanConstraints(t, plan, c.host, c.count)
	}

	// no ceiling: the target keeps growing with the host. A cap at any
	// constant would flatten this sequence.
	previous := connect.ByteCount(0)
	for _, host := range []connect.ByteCount{4 * testGib, 8 * testGib, 32 * testGib, 256 * testGib} {
		target := newProviderMemoryPlan(0, host, 1).DeviceMemoryTargetByteCount
		if target <= previous {
			t.Errorf("host %d: target %d did not grow past %d", host, target, previous)
		}
		previous = target
	}
}

// TestProviderMemoryPlanOverSubscribedClampsToHostHeap is the failure this plan
// exists to prevent: an 8 GiB host with 2000 proxies used to compose a 117 GiB
// soft limit (3 x 2000 x the 20 MiB floor), which the runtime treats as "no
// limit" on an 8 GiB box. The clamped limit is the heap, the target stays at
// the floor, and the warning names the count, the share, the floor and the heap.
func TestProviderMemoryPlanOverSubscribedClampsToHostHeap(t *testing.T) {
	for _, c := range []struct {
		host    connect.ByteCount
		count   int
		share   connect.ByteCount
		unclamp connect.ByteCount
	}{
		{8 * testGib, 2000, 8 * testGib * 4 / 5 / 6000, 3 * 2000 * 20 * testMib},
		{8 * testGib, 200, 8 * testGib * 4 / 5 / 600, 3 * 200 * 20 * testMib},
		{1 * testGib, 50, 1 * testGib * 4 / 5 / 150, 3 * 50 * 20 * testMib},
	} {
		heap := c.host * providerHostMemoryFractionNumerator / providerHostMemoryFractionDenominator
		plan, logged := capturePlan(t, 0, c.host, c.count)

		if plan.SoftLimitByteCount != heap {
			t.Errorf("host %d count %d: soft limit %d, want the heap %d", c.host, c.count, plan.SoftLimitByteCount, heap)
		}
		// the row is only over-subscribed if the old formula went over the heap
		if c.unclamp <= heap {
			t.Fatalf("host %d count %d: %d is not over the heap %d", c.host, c.count, c.unclamp, heap)
		}
		if plan.MemoryBudgetByteCount != plan.SoftLimitByteCount {
			t.Errorf("host %d count %d: budget %d is not the clamped soft limit %d",
				c.host, c.count, plan.MemoryBudgetByteCount, plan.SoftLimitByteCount)
		}
		// the device target is untouched by the clamp: it stays at the floor
		if plan.DeviceMemoryTargetByteCount != providerMinDeviceMemoryTargetByteCount {
			t.Errorf("host %d count %d: target %d, want the %d floor",
				c.host, c.count, plan.DeviceMemoryTargetByteCount, providerMinDeviceMemoryTargetByteCount)
		}
		for _, want := range []string{
			fmt.Sprintf("%d providers", c.count),
			fmt.Sprintf("%.1fMiB each", mibf(c.share)),
			fmt.Sprintf("%.0fMiB floor", mibf(providerMinDeviceMemoryTargetByteCount)),
			fmt.Sprintf("%.0fMiB host heap", mibf(heap)),
			fmt.Sprintf("clamped to the %.0fMiB heap", mibf(heap)),
		} {
			if !strings.Contains(logged, want) {
				t.Errorf("host %d count %d: warning %q does not name %q", c.host, c.count, logged, want)
			}
		}
	}
}

// TestProviderMemoryPlanUnknownHostIsBounded covers the platforms whose host
// memory reader reports zero (provider_host_memory_other.go: FreeBSD, Windows,
// everything without /proc/meminfo or sysctl). The old default composed
// 3 x count x 64 MiB with nothing to check it against, so 2000 providers asked
// for 375 GiB on any host -- a limit above physical memory is no limit at all,
// and it was silent. The zero is injected here, which is exactly what
// hostMemoryByteCount returns on those platforms, so the behaviour is pinned
// without one: the per-device target keeps the previous 64 MiB, counts that fit
// the documented assumption are unchanged, and beyond it the limit is the
// assumption's heap and the plan says the bound is assumed, not measured.
func TestProviderMemoryPlanUnknownHostIsBounded(t *testing.T) {
	assumedHeap := providerUnknownHostAssumedHostByteCount * 4 / 5
	for _, c := range []struct {
		count   int
		soft    connect.ByteCount
		bounded bool
	}{
		// under the assumption: byte for byte the previous plan
		{1, 192 * testMib, false},
		{4, 768 * testMib, false},
		{8, 1536 * testMib, false},
		// over it: the composed limit is replaced by the assumption's heap
		{9, assumedHeap, true},
		{20, assumedHeap, true},
		{2000, assumedHeap, true},
	} {
		plan, logged := capturePlan(t, 0, 0, c.count)
		if plan.DeviceMemoryTargetByteCount != providerUnknownHostDeviceMemoryTargetByteCount {
			t.Errorf("count %d: target %d, want the %d unknown-host default",
				c.count, plan.DeviceMemoryTargetByteCount, providerUnknownHostDeviceMemoryTargetByteCount)
		}
		if plan.SoftLimitByteCount != c.soft {
			t.Errorf("count %d: soft limit %d, want %d", c.count, plan.SoftLimitByteCount, c.soft)
		}
		if plan.MemoryBudgetByteCount != plan.SoftLimitByteCount {
			t.Errorf("count %d: budget %d is not the soft limit %d",
				c.count, plan.MemoryBudgetByteCount, plan.SoftLimitByteCount)
		}
		if assumedHeap < plan.SoftLimitByteCount {
			t.Errorf("count %d: soft limit %d is over the %d assumption", c.count, plan.SoftLimitByteCount, assumedHeap)
		}
		if !c.bounded {
			if logged != "" {
				t.Errorf("count %d: unexpected warning %q", c.count, logged)
			}
			continue
		}
		for _, want := range []string{
			"host memory unknown",
			fmt.Sprintf("%d providers", c.count),
			fmt.Sprintf("compose a %.0fMiB soft limit", mibf(providerRuntimeBytesPerLiveByte*connect.ByteCount(c.count)*providerUnknownHostDeviceMemoryTargetByteCount)),
			fmt.Sprintf("bounded to the %.0fMiB heap", mibf(assumedHeap)),
			fmt.Sprintf("%.0fMiB unknown-host assumption", mibf(providerUnknownHostAssumedHostByteCount)),
			"not a reading of this host",
			"--max-memory",
		} {
			if !strings.Contains(logged, want) {
				t.Errorf("count %d: warning %q does not name %q", c.count, logged, want)
			}
		}
		assertProviderMemoryPlanConstraints(t, plan, 0, c.count)
	}
}

// TestProviderMemoryPlanDegenerateHostIsNotSilent: a host reading whose 4/5
// heap truncates to zero (one byte) has nothing to install, so the plan leaves
// the limit unset instead of returning the unclamped composition -- and warns,
// where the previous `0 < heap` guard skipped both the clamp and the warning.
func TestProviderMemoryPlanDegenerateHostIsNotSilent(t *testing.T) {
	previousSoftLimit := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(previousSoftLimit)

	plan, logged := capturePlan(t, 0, 1, 1)
	if plan.DeviceMemoryTargetByteCount != providerMinDeviceMemoryTargetByteCount {
		t.Errorf("target %d, want the %d floor", plan.DeviceMemoryTargetByteCount, providerMinDeviceMemoryTargetByteCount)
	}
	if plan.SoftLimitByteCount != 0 || plan.MemoryBudgetByteCount != 0 {
		t.Errorf("1-byte host plan %+v, want no derived limit", plan)
	}
	if !strings.Contains(logged, "over-subscribed") {
		t.Errorf("1-byte host did not warn: %q", logged)
	}
	if !strings.Contains(logged, "0MiB host heap") {
		t.Errorf("1-byte host warning does not name the empty heap: %q", logged)
	}
	// zero means "leave it alone", not "clear the limit"
	debug.SetMemoryLimit(math.MaxInt64)
	applyProviderProcessMemory(plan)
	if got := debug.SetMemoryLimit(-1); got != math.MaxInt64 {
		t.Errorf("degenerate plan installed the limit %d", got)
	}

	// a heap that truncates to a handful of bytes still clamps to it
	plan, logged = capturePlan(t, 0, 5, 1)
	if plan.SoftLimitByteCount != 4 || plan.MemoryBudgetByteCount != 4 {
		t.Errorf("5-byte host plan %+v, want the 4-byte heap", plan)
	}
	if !strings.Contains(logged, "clamped to the 0MiB heap") {
		t.Errorf("5-byte host did not warn: %q", logged)
	}
}

// TestProviderMemoryPlanConstraintsCatchBudgetRegression proves the narrowed
// exemption still catches what the old blanket `!overSubscribed` skipped: the
// 8 GiB / 2000 provider row is over-subscribed, yet a budget collapsed below
// 3 x target (or below 20/34 of a target) is a regression, not an exempt state.
func TestProviderMemoryPlanConstraintsCatchBudgetRegression(t *testing.T) {
	plan, _ := capturePlan(t, 0, 8*testGib, 2000)
	pass := &recordingPlanFailer{}
	assertProviderMemoryPlanConstraints(pass, plan, 8*testGib, 2000)
	if len(pass.messages) != 0 {
		t.Fatalf("the real over-subscribed plan was reported: %v", pass.messages)
	}

	// over-subscribed (so the exemption is in play), but with the budget
	// collapsed from the heap to 10 MiB instead of derived from it
	broken := plan
	broken.MemoryBudgetByteCount = 10 * testMib
	broken.SoftLimitByteCount = broken.MemoryBudgetByteCount
	recorder := &recordingPlanFailer{}
	assertProviderMemoryPlanConstraints(recorder, broken, 8*testGib, 2000)
	reported := strings.Join(recorder.messages, "\n")
	for _, want := range []string{"is under 3 x target", "is over 20/34 of budget"} {
		if !strings.Contains(reported, want) {
			t.Errorf("helper did not report %q for a collapsed budget: %v", want, recorder.messages)
		}
	}

	// the exemption itself is still required: a host whose heap is under the
	// constraint gets its clamped budget, not a failure
	tiny, _ := capturePlan(t, 0, 32*testMib, 1)
	recorder = &recordingPlanFailer{}
	assertProviderMemoryPlanConstraints(recorder, tiny, 32*testMib, 1)
	if len(recorder.messages) != 0 {
		t.Fatalf("a host too small for its own heap was reported: %v", recorder.messages)
	}
}

// recordingPlanFailer collects the constraint helper's reports so a test can
// assert on them.
type recordingPlanFailer struct {
	messages []string
}

func (f *recordingPlanFailer) Helper() {}

func (f *recordingPlanFailer) Errorf(format string, args ...any) {
	f.messages = append(f.messages, fmt.Sprintf(format, args...))
}

// TestProviderMemoryPlanFloorStillSetsTheDeviceTarget: the clamp bounds the
// process soft limit, not the per-provider target. A host too small for the
// floor still gets the SDK's 20 MiB target.
func TestProviderMemoryPlanFloorStillSetsTheDeviceTarget(t *testing.T) {
	plan, _ := capturePlan(t, 0, 64*testMib, 4)
	if plan.DeviceMemoryTargetByteCount != providerMinDeviceMemoryTargetByteCount {
		t.Fatalf("target = %d, want the %d floor", plan.DeviceMemoryTargetByteCount, providerMinDeviceMemoryTargetByteCount)
	}
	if heap := 64 * testMib * 4 / 5; plan.SoftLimitByteCount != heap {
		t.Fatalf("soft limit = %d, want the heap %d", plan.SoftLimitByteCount, heap)
	}
}

// TestProviderMemoryPlanLeavesHostHeadroom: the property asked for. At every
// host size and provider count the composed soft limit is at most four fifths
// of the host, leaving a fifth to the operating system and co-resident
// processes. Where the 20 MiB floor makes the targets not fit, the limit is
// exactly the heap and the plan warns; where it does not bind, the limit is the
// full four-fifths composition.
func TestProviderMemoryPlanLeavesHostHeadroom(t *testing.T) {
	floorBound := 0
	clamped := 0
	for _, host := range []connect.ByteCount{
		256 * testMib, 512 * testMib, 1 * testGib, 2 * testGib, 4 * testGib,
		8 * testGib, 16 * testGib, 32 * testGib, 64 * testGib, 128 * testGib, 256 * testGib,
	} {
		for _, count := range []int{1, 2, 3, 4, 8, 16, 64, 200, 2000} {
			plan, logged := capturePlan(t, 0, host, count)
			heap := host * 4 / 5
			if heap < plan.SoftLimitByteCount {
				t.Errorf("host %d count %d: soft limit %d is over four fifths of the host (%d)",
					host, count, plan.SoftLimitByteCount, heap)
			}
			target := plan.DeviceMemoryTargetByteCount
			if target == providerMinDeviceMemoryTargetByteCount {
				floorBound += 1
			}
			if heap < providerRuntimeBytesPerLiveByte*connect.ByteCount(count)*target {
				// the floor over-subscribes the host: the targets cannot fit,
				// so the limit is clamped to the heap and says so
				clamped += 1
				if plan.SoftLimitByteCount != heap {
					t.Errorf("host %d count %d: over-subscribed soft limit %d, want the heap %d",
						host, count, plan.SoftLimitByteCount, heap)
				}
				if !strings.Contains(logged, "over-subscribed") {
					t.Errorf("host %d count %d: no over-subscription warning: %q", host, count, logged)
				}
				continue
			}
			if logged != "" {
				t.Errorf("host %d count %d: warning for a plan that fits: %q", host, count, logged)
			}
			if plan.SoftLimitByteCount < heap-3*connect.ByteCount(count) {
				// integer division loses at most 3 x count bytes; anything
				// more is a formula that is not the 80% composition
				t.Errorf("host %d count %d: soft limit %d is not four fifths of the host (%d)",
					host, count, plan.SoftLimitByteCount, heap)
			}
		}
	}
	// both branches exist in the grid: 256 MiB across 64 providers is 3.2 MiB
	// each, under the 20 MiB floor, and 256 MiB across 1 provider is not
	if floorBound == 0 {
		t.Error("no floor-bound row; the grid should include a host too small for its providers")
	}
	if clamped == 0 {
		t.Error("no clamped row; the grid should include a host too small for its providers")
	}
	if plan, _ := capturePlan(t, 0, 256*testMib, 64); plan.DeviceMemoryTargetByteCount != providerMinDeviceMemoryTargetByteCount {
		t.Errorf("256 MiB / 64 providers: target %d, want the %d floor", plan.DeviceMemoryTargetByteCount, providerMinDeviceMemoryTargetByteCount)
	}
}

// planFailer is the slice of *testing.T the constraint helper uses, so a test
// can prove the helper still reports a regression rather than trusting that it
// accepts real plans.
type planFailer interface {
	Helper()
	Errorf(format string, args ...any)
}

// assertProviderMemoryPlanConstraints checks the two constraints every
// derived plan keeps where they can be satisfied: the budget is at least three
// times one target (the runtime amplification), and one target is at most 20/34
// of the budget (the SDK's pool : target split of a process budget). It also
// checks the soft limit never exceeds four fifths of a known host.
//
// An over-subscribed plan (the floor pushed 3 x targets over the heap) keeps
// the target at the floor and the budget at the heap instead of at 3 x the
// summed targets. Both constraints above are still per-device and still hold
// there, except on a host too small for its own heap to satisfy them: under
// 75 MiB for 3 x a 20 MiB target, and under 42.5 MiB for 20/34 of one. Only
// that specific miss is forgiven. Where the heap could satisfy the constraint,
// a budget that does not is a regression -- a collapsed or mis-derived budget
// on a large over-subscribed host must not hide behind the exemption.
func assertProviderMemoryPlanConstraints(
	t planFailer,
	plan providerMemoryPlan,
	host connect.ByteCount,
	count int,
) {
	t.Helper()
	target := plan.DeviceMemoryTargetByteCount
	budget := plan.MemoryBudgetByteCount
	heapByteCount := host *
		providerHostMemoryFractionNumerator /
		providerHostMemoryFractionDenominator
	clampedToHeap := 0 < host && budget == heapByteCount
	if budget < providerRuntimeBytesPerLiveByte*target &&
		!(clampedToHeap && heapByteCount < providerRuntimeBytesPerLiveByte*target) {
		t.Errorf("host %d count %d: budget %d is under 3 x target %d", host, count, budget, target)
	}
	if budget/34*20 < target && !(clampedToHeap && heapByteCount/34*20 < target) {
		t.Errorf("host %d count %d: target %d is over 20/34 of budget %d", host, count, target, budget)
	}
	if budget != plan.SoftLimitByteCount {
		t.Errorf("host %d count %d: budget %d is not the soft limit %d", host, count, budget, plan.SoftLimitByteCount)
	}
	if 0 < host && heapByteCount < plan.SoftLimitByteCount {
		t.Errorf("host %d count %d: soft limit %d exceeds four fifths of the host",
			host, count, plan.SoftLimitByteCount)
	}
}

// TestProviderMemoryPlanExplicitFlagKeepsMeaning: --max-memory is still the
// soft limit and is divided into the per-provider targets, whatever the host;
// it is also the process budget.
func TestProviderMemoryPlanExplicitFlagKeepsMeaning(t *testing.T) {
	opts := parseArgsForTest(t, []string{"provide", "--max-memory=2gib"})
	explicit := parseProviderMaxMemory(opts)
	for _, c := range []struct {
		count  int
		target connect.ByteCount
	}{
		{1, 2 * testGib},
		{4, 512 * testMib},
		{0, 2 * testGib},
	} {
		for _, host := range []connect.ByteCount{0, 512 * testMib, 64 * testGib} {
			plan := newProviderMemoryPlan(explicit, host, c.count)
			want := providerMemoryPlan{
				DeviceMemoryTargetByteCount: c.target,
				SoftLimitByteCount:          2 * testGib,
				MemoryBudgetByteCount:       2 * testGib,
			}
			if plan != want {
				t.Errorf("count %d host %d: plan %+v, want %+v", c.count, host, plan, want)
			}
		}
	}
}

// TestProviderAuthMemoryPlanKeepsPoolSizing: auth keeps the /8 pool cap and
// the soft limit for an explicit flag, and does nothing without one.
func TestProviderAuthMemoryPlanKeepsPoolSizing(t *testing.T) {
	if plan := newProviderAuthMemoryPlan(0); plan != (providerMemoryPlan{}) {
		t.Fatalf("no-flag auth plan = %+v", plan)
	}
	want := providerMemoryPlan{SoftLimitByteCount: 512 * testMib, MessagePoolByteCount: 64 * testMib}
	if plan := newProviderAuthMemoryPlan(512 * testMib); plan != want {
		t.Fatalf("auth plan = %+v, want %+v", plan, want)
	}
}

// TestApplyProviderMemoryPlan installs the soft limit and the process budget,
// and resizes pools only when the plan says so; the auth plan sets no budget.
func TestApplyProviderMemoryPlan(t *testing.T) {
	// The plan installs the default soft limit; an operator GOMEMLIMIT would
	// (correctly) outrank it, so pin the environment for this assertion.
	t.Setenv("GOMEMLIMIT", "")
	previousSoftLimit := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(previousSoftLimit)
	previousBudget := connect.MemoryBudget()
	defer connect.SetMemoryBudget(previousBudget)
	connect.SetMemoryBudget(0)

	poolCapacity := func() connect.ByteCount {
		return connect.GetMessagePoolAggregateStats().CapacityByteCount
	}
	poolsBefore := poolCapacity()
	applyProviderProcessMemory(newProviderMemoryPlan(0, 0, 2))
	if got := debug.SetMemoryLimit(-1); got != int64(384*testMib) {
		t.Fatalf("soft limit = %d, want %d", got, 384*testMib)
	}
	if got := poolCapacity(); got != poolsBefore {
		t.Fatalf("provide plan resized pools %d -> %d", poolsBefore, got)
	}
	if got := connect.MemoryBudget(); got != 384*testMib {
		t.Fatalf("process budget = %d, want %d", got, 384*testMib)
	}
	// the budget bought no flow caps: the provider NAT profile built under it
	// is the unlimited one the unbudgeted provider had
	if nat := connect.DefaultProviderLocalUserNatSettings(); nat.TcpBufferSettings.GlobalLimit != 0 ||
		nat.UdpBufferSettings.GlobalLimit != 0 ||
		nat.UdpBufferSettings.IdleTimeout != 300*time.Second {
		t.Fatalf("the process budget changed the provider NAT profile: tcp %d udp %d idle %s",
			nat.TcpBufferSettings.GlobalLimit, nat.UdpBufferSettings.GlobalLimit,
			nat.UdpBufferSettings.IdleTimeout)
	}
	connect.SetMemoryBudget(0)

	defer connect.ResizeMessagePools(
		connect.InitialMessagePoolByteCount/3,
		connect.InitialMessagePoolByteCount-connect.InitialMessagePoolByteCount/3,
	)
	applyProviderProcessMemory(newProviderAuthMemoryPlan(512 * testMib))
	if got := poolCapacity(); got == poolsBefore {
		t.Fatal("auth plan did not resize pools")
	}
	if got := connect.MemoryBudget(); got != 0 {
		t.Fatalf("auth plan set process budget %d", got)
	}
}

// TestApplyProviderMemoryPlanKeepsOperatorGOMEMLIMIT: GOMEMLIMIT in the
// environment is the operator's explicit pin and outranks the plan's default
// soft limit (the order profile_tiers.go documents). The Go runtime installs
// the environment value before main, so the plan must leave the runtime limit
// alone - otherwise the startup resource warning reads the plan and stays
// silent on the heavy-pool-under-a-small-pin case it exists for. The rest of
// the plan (the connect process budget) still applies.
func TestApplyProviderMemoryPlanKeepsOperatorGOMEMLIMIT(t *testing.T) {
	preserveRuntimeTuning(t)
	previousBudget := connect.MemoryBudget()
	t.Cleanup(func() { connect.SetMemoryBudget(previousBudget) })
	connect.SetMemoryBudget(0)

	t.Setenv("GOMEMLIMIT", "400MiB")
	debug.SetMemoryLimit(400 * testMib) // what the runtime installed at boot

	plan := newProviderMemoryPlan(0, 8*testGib, 1)
	if plan.SoftLimitByteCount <= 400*testMib {
		t.Fatalf("test needs a plan limit above the operator pin, got %d", plan.SoftLimitByteCount)
	}
	applyProviderProcessMemory(plan)
	if got := debug.SetMemoryLimit(-1); got != 400*testMib {
		t.Fatalf("plan overwrote operator GOMEMLIMIT: limit = %d, want %d", got, 400*testMib)
	}
	if got := connect.MemoryBudget(); got != plan.MemoryBudgetByteCount {
		t.Errorf("process budget = %d, want the plan's %d; only the soft limit yields to the operator", got, plan.MemoryBudgetByteCount)
	}
}

// TestProviderTargetIsAppliedToTheDevice: the plan's target is the device
// target; a zero target keeps the SDK default.
func TestProviderTargetIsAppliedToTheDevice(t *testing.T) {
	settings := sdk.DefaultDeviceLocalSettings()
	sdkDefault := settings.MemoryTargetByteCount
	applyProviderMemoryTarget(settings, 0)
	if settings.MemoryTargetByteCount != sdkDefault {
		t.Fatalf("zero target replaced the SDK default %d with %d", sdkDefault, settings.MemoryTargetByteCount)
	}
	plan := newProviderMemoryPlan(0, 8*testGib, 1)
	applyProviderMemoryTarget(settings, plan.DeviceMemoryTargetByteCount)
	if settings.MemoryTargetByteCount != sdk.ByteCount(plan.DeviceMemoryTargetByteCount) {
		t.Fatalf("device target = %d, want %d", settings.MemoryTargetByteCount, plan.DeviceMemoryTargetByteCount)
	}
}

// TestProviderTargetHasNoWindowCeiling: the premise that justified the old
// 64 MiB cap ("connect's window scale tops out at the 64 MiB reference") is
// false — the H3 windows are fractions of the target and keep growing above
// it — and the text is gone from the plan's source.
func TestProviderTargetHasNoWindowCeiling(t *testing.T) {
	at64 := connect.DefaultPlatformTransportSettingsWithMemoryTarget(64 * testMib)
	at256 := connect.DefaultPlatformTransportSettingsWithMemoryTarget(256 * testMib)
	if at256.H3MaxStreamReceiveWindowByteCount <= at64.H3MaxStreamReceiveWindowByteCount ||
		at256.H3MaxConnectionReceiveWindowByteCount <= at64.H3MaxConnectionReceiveWindowByteCount {
		t.Fatalf("H3 windows do not grow above 64 MiB: stream %d -> %d, conn %d -> %d",
			at64.H3MaxStreamReceiveWindowByteCount, at256.H3MaxStreamReceiveWindowByteCount,
			at64.H3MaxConnectionReceiveWindowByteCount, at256.H3MaxConnectionReceiveWindowByteCount)
	}

	source, err := os.ReadFile("provider_memory.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{"tops out", "buys no larger"} {
		if strings.Contains(string(source), stale) {
			t.Errorf("provider_memory.go still carries the stale premise %q", stale)
		}
	}
	if !strings.Contains(string(source), "no memory ceiling") {
		t.Error("provider_memory.go does not state the no-ceiling default")
	}
}

func TestHostMemoryParsing(t *testing.T) {
	if got := parseMeminfoTotal("MemFree: 1 kB\nMemTotal:       8039936 kB\n"); got != 8039936*1024 {
		t.Fatalf("MemTotal = %d", got)
	}
	if got := parseMeminfoTotal("garbage"); got != 0 {
		t.Fatalf("garbage MemTotal = %d", got)
	}
	for content, want := range map[string]connect.ByteCount{
		"max\n":                 0,
		"":                      0,
		"536870912\n":           536870912,
		"9223372036854771712\n": 0,
	} {
		if got := parseCgroupLimit(content); got != want {
			t.Errorf("cgroup %q = %d, want %d", content, got, want)
		}
	}
	if got := effectiveHostMemory(8*testGib, 512*testMib); got != 512*testMib {
		t.Fatalf("container limit not applied: %d", got)
	}
	if got := effectiveHostMemory(0, 0); got != 0 {
		t.Fatalf("unknown = %d", got)
	}
}
