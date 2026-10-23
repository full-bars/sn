//go:build linux

package miner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// These tests pin the four fixes the relay's audit already carries:
//   - bounded, safe, skipped where it cannot help (never a false "slow" from a
//     busy disk, a container, or a disabled run);
//   - the byte cap is rounded to whole chunks;
//   - a state dir several missing levels deep is measured through its nearest
//     existing parent;
//   - a path no level can measure is unknown, never a full disk.

// fakeBenchFile lets a test script how a write behaves.
type fakeBenchFile struct {
	write  func(p []byte) (int, error)
	closed atomic.Bool
}

func (f *fakeBenchFile) Write(p []byte) (int, error) { return f.write(p) }
func (f *fakeBenchFile) Close() error                { f.closed.Store(true); return nil }

func withBenchOpen(t *testing.T, open func(path string) (diskBenchFile, error)) {
	t.Helper()
	orig := diskBenchOpen
	diskBenchOpen = open
	t.Cleanup(func() { diskBenchOpen = orig })
}

// withBenchEnv pins the three skip inputs (container, io pressure, the
// operator override) so no test depends on whether the host running it is a
// container, is busy, or has the benchmark disabled.
func withBenchEnv(t *testing.T, container bool, psi float64, psiOK bool) {
	t.Helper()
	t.Setenv("URNETWORK_DISK_BENCH", "")
	oc, op := diskBenchInContainer, diskBenchIOPressure
	diskBenchInContainer = func() bool { return container }
	diskBenchIOPressure = func() (float64, bool) { return psi, psiOK }
	t.Cleanup(func() { diskBenchInContainer, diskBenchIOPressure = oc, op })
}

// withFreeSpace makes every statfs answer with the given free MiB, unless the
// injected function overrides a level.
func withFreeSpace(t *testing.T, freeMiB int64) {
	t.Helper()
	orig := diskSpaceStatfs
	diskSpaceStatfs = func(path string, st *syscall.Statfs_t) error {
		st.Bsize = 4096
		st.Bavail = freeMiB * 1024 * 1024 / 4096
		return nil
	}
	t.Cleanup(func() { diskSpaceStatfs = orig })
}

func withStatfs(t *testing.T, fn func(path string, st *syscall.Statfs_t) error) {
	t.Helper()
	orig := diskSpaceStatfs
	diskSpaceStatfs = fn
	t.Cleanup(func() { diskSpaceStatfs = orig })
}

func TestCheckDiskSpaceReportsFreeAndLowSpace(t *testing.T) {
	withFreeSpace(t, 5000)
	if got, low := CheckDiskSpace(t.TempDir()); got != 5000 || low {
		t.Fatalf("got (%d, %v), want (5000, false)", got, low)
	}
	withFreeSpace(t, 100)
	var got int64
	var low bool
	out := captureStdout(t, func() { got, low = CheckDiskSpace(t.TempDir()) })
	if got != 100 || !low {
		t.Fatalf("got (%d, %v), want (100, true)", got, low)
	}
	if !strings.Contains(out, "CRITICAL") {
		t.Fatalf("low space printed %q, want a CRITICAL line", out)
	}
}

// ghostPath returns a path of n levels that do not exist below base.
func ghostPath(base string, n int) string {
	for i := 0; i < n; i++ {
		base = filepath.Join(base, "ghost-"+string(rune('a'+i)))
	}
	return base
}

// A state dir several missing levels deep must still be measured through its
// nearest existing parent, not reported as a full disk. The statfs is injected
// so the expected value cannot drift with the real disk, and the probes show
// which level answered.
func TestCheckDiskSpaceWalksUpPastMissingLevels(t *testing.T) {
	base := t.TempDir()
	var probed []string
	withStatfs(t, func(path string, st *syscall.Statfs_t) error {
		probed = append(probed, path)
		if path != base {
			return syscall.ENOENT
		}
		st.Bsize = 4096
		st.Bavail = 5000 * 1024 * 1024 / 4096
		return nil
	})

	var got int64
	var low bool
	out := captureStdout(t, func() { got, low = CheckDiskSpace(ghostPath(base, 12)) })
	if got != 5000 || low {
		t.Fatalf("deep missing path: got (%d, %v), want (5000, false)", got, low)
	}
	if strings.Contains(out, "CRITICAL") {
		t.Fatalf("a measurable dir printed a CRITICAL line: %q", out)
	}
	if len(probed) != 13 || probed[len(probed)-1] != base {
		t.Fatalf("probed %d paths ending at %q, want 12 missing levels then %q", len(probed), probed[len(probed)-1], base)
	}
}

// A path that cannot be measured at any level is unknown, not low: (0, false)
// and no output, so a wrong path can never raise a false alert.
func TestCheckDiskSpaceUnmeasurablePathIsQuiet(t *testing.T) {
	withStatfs(t, func(string, *syscall.Statfs_t) error { return syscall.ENOENT })
	var got int64
	var low bool
	out := captureStdout(t, func() { got, low = CheckDiskSpace(ghostPath(t.TempDir(), 12)) })
	if got != 0 || low {
		t.Fatalf("unmeasurable path: got (%d, %v), want (0, false)", got, low)
	}
	if out != "" {
		t.Fatalf("unmeasurable path printed %q, want silence", out)
	}
}

// An unmeasurable path must not read as a full disk in the benchmark's own
// skip message either: unknown is not zero.
func TestRunDiskAuditUnmeasurablePathIsNotReportedAsAFullDisk(t *testing.T) {
	withBenchEnv(t, false, 0, true)
	withStatfs(t, func(string, *syscall.Statfs_t) error { return syscall.ENOENT })
	dir := filepath.Join(t.TempDir(), "state", "deep")

	var slow, low bool
	out := captureStdout(t, func() { slow, low = RunDiskAudit(dir) })
	if slow || low {
		t.Fatalf("unmeasurable path: got (slow %v, low %v), want (false, false)", slow, low)
	}
	if !strings.Contains(out, "could not measure free space") || strings.Contains(out, "only 0 MiB free") {
		t.Fatalf("skip message = %q, want the could-not-measure wording and never 0 MiB free", out)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the benchmark created the state dir although it could not measure it")
	}
}

// The cap is whole chunks, because every write is a full chunk, and never more
// than 1/64 of free space.
func TestDiskBenchCapIsWholeChunksAndNeverAboveOneSixtyFourthOfFree(t *testing.T) {
	cases := []struct{ freeMiB, wantBytes int64 }{
		{300, 4 << 20},     // 4.69 MiB rounds down, not up to 5
		{256, 4 << 20},     // the smallest disk that is benchmarked
		{1000, 15 << 20},   // 15.6 MiB
		{1024, 16 << 20},   // exactly the byte cap
		{500000, 16 << 20}, // large disks stay at the byte cap
	}
	for _, c := range cases {
		got := diskBenchCap(c.freeMiB)
		if got != c.wantBytes {
			t.Errorf("free %d MiB: cap %d, want %d", c.freeMiB, got, c.wantBytes)
		}
		if got%diskBenchChunk != 0 || got > c.freeMiB*1024*1024/diskBenchFreeDiv {
			t.Errorf("free %d MiB: cap %d is not whole chunks within free/64", c.freeMiB, got)
		}
	}
}

// A fabricated slow filesystem must yield nothing worth keeping: the audit
// stops early instead of writing the cap, judges it slow, and records a
// bounded result. A disk that takes 200 ms per MiB is ~5 MB/s, an order of
// magnitude under the slow line.
func TestRunDiskAuditOnAFabricatedSlowFilesystemStopsEarlyAndIsSlow(t *testing.T) {
	withBenchEnv(t, false, 0, true)
	withFreeSpace(t, 500000) // plenty of room: the cap is the byte cap
	dir := t.TempDir()
	var written atomic.Int64
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		return &fakeBenchFile{write: func(p []byte) (int, error) {
			time.Sleep(200 * time.Millisecond)
			written.Add(int64(len(p)))
			return len(p), nil
		}}, nil
	})

	start := time.Now()
	var slow, low bool
	captureStdout(t, func() { slow, low = RunDiskAudit(dir) })
	took := time.Since(start)
	if !slow {
		t.Fatal("a 5 MB/s filesystem was not judged slow")
	}
	if low {
		t.Fatal("a disk with 500 GB free was reported as low on space")
	}
	if got := written.Load(); got >= diskBenchMaxBytes {
		t.Fatalf("wrote %d bytes: the audit ran to the cap instead of stopping once the rate was clearly slow", got)
	}
	if took > diskBenchBudget+diskBenchGrace+time.Second {
		t.Fatalf("audit took %s, past the budget plus grace", took)
	}

	mbps, cachedSlow, _, ok := DiskBenchStatus(dir)
	if !ok || !cachedSlow {
		t.Fatalf("cache = (%.1f, %v, ok %v), want the slow result recorded", mbps, cachedSlow, ok)
	}
	if mbps >= diskBenchSlowMBps {
		t.Fatalf("recorded %.1f MB/s, want under %.0f", mbps, diskBenchSlowMBps)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, diskBenchPrefix+"*")); len(left) != 0 {
		t.Fatalf("test file left behind: %v", left)
	}
}

// A small filesystem gets a cap of free/64 in whole chunks, so the audit can
// never write more than a rounding error of what is left.
func TestRunDiskAuditOnALimitedFilesystemStaysWithinTheChunkRoundedCap(t *testing.T) {
	withBenchEnv(t, false, 0, true)
	withFreeSpace(t, 300) // 4.69 MiB available to the audit, rounded to 4 MiB
	dir := t.TempDir()
	var written atomic.Int64
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		return &fakeBenchFile{write: func(p []byte) (int, error) {
			written.Add(int64(len(p)))
			return len(p), nil
		}}, nil
	})

	var slow bool
	captureStdout(t, func() { slow, _ = RunDiskAudit(dir) })
	if slow {
		t.Fatal("a fast, small filesystem was judged slow")
	}
	if got := written.Load(); got != 4<<20 {
		t.Fatalf("wrote %d bytes on a 300 MiB filesystem, want the 4 MiB chunk-rounded cap", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, diskBenchPrefix+"*")); len(left) != 0 {
		t.Fatalf("test file left behind: %v", left)
	}
}

// A disk under the free-space floor is not benchmarked at all: the audit
// refuses to consume the space it was asked to report on.
func TestRunDiskAuditSkipsADiskBelowTheFreeSpaceFloor(t *testing.T) {
	withBenchEnv(t, false, 0, true)
	withFreeSpace(t, 100)
	dir := t.TempDir()
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		t.Fatal("benchmark ran on a disk with 100 MiB free")
		return nil, nil
	})
	var slow, low bool
	captureStdout(t, func() { slow, low = RunDiskAudit(dir) })
	if slow {
		t.Fatal("a disk that was not benchmarked was reported slow")
	}
	if !low {
		t.Fatal("100 MiB free was not reported as low space")
	}
}

// A fresh result is reused, so a restart loop cannot repeat the measurement.
func TestRunDiskAuditUsesTheCacheWithoutWriting(t *testing.T) {
	withBenchEnv(t, false, 0, true)
	withFreeSpace(t, 500000)
	dir := t.TempDir()
	dev, ok := diskBenchDevice(dir)
	writeDiskBenchCache(dir, dev, ok, diskBenchResult{MBps: 3, Bytes: 4 << 20, Slow: true}, time.Now())
	var opens atomic.Int32
	withBenchOpen(t, func(path string) (diskBenchFile, error) { opens.Add(1); return nil, errors.New("must not open") })

	var slow bool
	captureStdout(t, func() { slow, _ = RunDiskAudit(dir) })
	if !slow {
		t.Fatal("cached slow result not honoured")
	}
	if opens.Load() != 0 {
		t.Fatal("the audit wrote to disk despite a fresh cached result")
	}
}

func TestRunDiskAuditNeverWritesOutsideTheStateDir(t *testing.T) {
	withBenchEnv(t, false, 0, true)
	withFreeSpace(t, 500000)
	cwd := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	t.Chdir(cwd)
	var paths []string
	var mu sync.Mutex
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		mu.Lock()
		paths = append(paths, path)
		mu.Unlock()
		return &fakeBenchFile{write: func(p []byte) (int, error) { return len(p), nil }}, nil
	})
	captureStdout(t, func() { RunDiskAudit(state) })
	if len(paths) != 1 || !strings.HasPrefix(paths[0], state+string(os.PathSeparator)) {
		t.Fatalf("benchmark file paths %v are not inside the state dir %s", paths, state)
	}
	if left, _ := os.ReadDir(cwd); len(left) != 0 {
		t.Fatalf("working dir was touched: %v", left)
	}
}

// Debris a killed process left behind is swept; a fresh file (this process)
// and anything that is not a bench file are kept.
func TestRunDiskAuditSweepsStaleDebrisOnly(t *testing.T) {
	withBenchEnv(t, false, 0, true)
	withFreeSpace(t, 500000)
	dir := t.TempDir()
	old := time.Now().Add(-10 * time.Minute)
	mk := func(name string, mtime time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, 1024), 0600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mtime, mtime)
		return p
	}
	stale := mk(diskBenchPrefix+"-4242", old)
	fresh := mk(diskBenchPrefix+"-9999", time.Now())
	other := mk("jwt", old)
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		return &fakeBenchFile{write: func(p []byte) (int, error) { return len(p), nil }}, nil
	})
	captureStdout(t, func() { RunDiskAudit(dir) })
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale debris %s was not removed", stale)
	}
	for _, p := range []string{fresh, other} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s must not be removed: %v", filepath.Base(p), err)
		}
	}
}

// The skip reasons are the relay's: a container without the override, a
// disabled benchmark, and a busy host disk all mean no writes and no "slow"
// verdict.
func TestSkippedBenchmarkNeverWritesAndIsNeverSlow(t *testing.T) {
	cases := []struct {
		name      string
		container bool
		psi       float64
		psiOK     bool
		override  string
	}{
		{name: "container", container: true, psiOK: true},
		{name: "disabled", override: "0", psiOK: true},
		{name: "busy disk", psi: 35, psiOK: true, override: "1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withBenchEnv(t, c.container, c.psi, c.psiOK)
			t.Setenv("URNETWORK_DISK_BENCH", c.override)
			withFreeSpace(t, 500000)
			dir := t.TempDir()
			withBenchOpen(t, func(path string) (diskBenchFile, error) {
				t.Fatal("benchmark wrote although it had a reason to skip")
				return nil, nil
			})
			var slow bool
			out := captureStdout(t, func() { slow, _ = RunDiskAudit(dir) })
			if slow {
				t.Fatal("a skipped benchmark was reported as a slow disk")
			}
			if !strings.Contains(out, "Disk benchmark skipped") {
				t.Fatalf("output = %q, want the skip reason named", out)
			}
			if left, _ := os.ReadDir(dir); len(left) != 0 {
				t.Fatalf("skipped benchmark left files: %v", left)
			}
		})
	}
}

func TestDiskBenchSkipReasonNeedsNeitherContainerNorBusyDiskWhenForced(t *testing.T) {
	withBenchEnv(t, true, 0, true)
	if diskBenchSkipReason() == "" {
		t.Fatal("benchmark would run in a container by default")
	}
	t.Setenv("URNETWORK_DISK_BENCH", "1")
	if r := diskBenchSkipReason(); r != "" {
		t.Fatalf("forced benchmark on a quiet container still skipped: %s", r)
	}
	withBenchEnv(t, false, 0, false)
	if r := diskBenchSkipReason(); r != "" {
		t.Fatalf("no pressure info (old kernel) must not block the benchmark: %s", r)
	}
}

// A write that hangs in the kernel cannot block the caller past the budget
// plus grace; the hung write is itself the answer.
func TestRunDiskBenchHungWriteDoesNotBlockAndIsSlow(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	opened := make(chan struct{}, 1)
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		os.WriteFile(path, []byte("x"), 0600)
		opened <- struct{}{}
		return &fakeBenchFile{write: func(p []byte) (int, error) { <-release; return len(p), nil }}, nil
	})
	start := time.Now()
	r := runDiskBench(dir, 16<<20, 300*time.Millisecond)
	took := time.Since(start)
	close(release)
	<-opened
	if took > 300*time.Millisecond+diskBenchGrace+300*time.Millisecond {
		t.Fatalf("a hung write blocked the caller for %s", took)
	}
	if !r.Slow || !r.TimedOut {
		t.Fatalf("hung write must report slow and timed out: %+v", r)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		left, _ := filepath.Glob(filepath.Join(dir, diskBenchPrefix+"*"))
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("test file left behind: %v", left)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDiskBenchCacheExpiresAndIsPerDevice(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeDiskBenchCache(dir, 7, true, diskBenchResult{MBps: 120, Bytes: 16 << 20}, now)
	if r, ok := readDiskBenchCache(dir, 7, true, now.Add(time.Hour)); !ok || r.MBps != 120 {
		t.Fatalf("fresh fast result not reused: %+v ok=%v", r, ok)
	}
	if _, ok := readDiskBenchCache(dir, 7, true, now.Add(25*time.Hour)); ok {
		t.Fatal("fast result reused past its TTL")
	}
	if _, ok := readDiskBenchCache(dir, 8, true, now.Add(time.Minute)); ok {
		t.Fatal("result reused for a different device")
	}
	writeDiskBenchCache(dir, 7, true, diskBenchResult{MBps: 4, Bytes: 4 << 20, Slow: true}, now)
	if _, ok := readDiskBenchCache(dir, 7, true, now.Add(30*time.Minute)); !ok {
		t.Fatal("fresh slow result not reused")
	}
	if _, ok := readDiskBenchCache(dir, 7, true, now.Add(2*time.Hour)); ok {
		t.Fatal("slow result must be rechecked after an hour")
	}
	if _, ok := readDiskBenchCache(dir, 7, true, now.Add(-time.Minute)); ok {
		t.Fatal("a cache dated in the future (clock moved) was trusted")
	}
}

// The startup gate only asks for the write benchmark when something acts on
// it: the auto profile without RAM logs already forced.
func TestStartupDiskBenchmarkWantedOnlyForTheAutoProfile(t *testing.T) {
	cases := []struct {
		profile string
		ramlogs string
		want    bool
	}{
		{profile: profileAuto, want: true},
		{profile: profileAuto, ramlogs: "1", want: false},
		{profile: profileLowmem, want: false},
		{profile: profileEco, want: false},
		{profile: profileTurboV4, want: false},
		{profile: "", want: false},
	}
	for _, c := range cases {
		t.Setenv("URNETWORK_PROFILE", c.profile)
		t.Setenv("URNETWORK_RAMLOGS", c.ramlogs)
		if got := startupDiskBenchmarkWanted(); got != c.want {
			t.Errorf("profile %q ramlogs %q: got %v, want %v", c.profile, c.ramlogs, got, c.want)
		}
	}
}

// The startup path must not wait for the audit: the launch returns before any
// write happens, the result lands in the state dir for /metrics, and a second
// launch does not repeat the benchmark.
func TestStartStartupDiskAuditDoesNotBlockStartup(t *testing.T) {
	withBenchEnv(t, false, 0, true)
	withFreeSpace(t, 500000)
	t.Setenv("URNETWORK_PROFILE", profileAuto)
	t.Setenv("URNETWORK_RAMLOGS", "")
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	var opens atomic.Int32
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		opens.Add(1)
		return &fakeBenchFile{write: func(p []byte) (int, error) {
			time.Sleep(20 * time.Millisecond) // slow enough to prove the launch did not wait
			return len(p), nil
		}}, nil
	})

	start := time.Now()
	startStartupDiskAudit()
	if took := time.Since(start); took > 250*time.Millisecond {
		t.Fatalf("launch blocked for %s; startup must not wait for the audit", took)
	}

	var cache []byte
	deadline := time.Now().Add(10 * time.Second)
	for len(cache) == 0 {
		cache, _ = os.ReadFile(filepath.Join(dir, "disk-bench.json"))
		if time.Now().After(deadline) {
			t.Fatal("the audit never recorded a result in the state dir")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(string(cache), "\"result\"") {
		t.Fatalf("cache = %q, want the recorded benchmark result", cache)
	}

	startStartupDiskAudit()
	if got := opens.Load(); got != 1 {
		t.Fatalf("benchmark opened the test file %d times, want exactly once", got)
	}
}

// URNETWORK_SKIP_AUDIT=1 and the no-state-dir case both do nothing at all.
func TestRunStartupDiskAuditSkipsWithoutAStateDirOrOnRequest(t *testing.T) {
	withBenchEnv(t, false, 0, true)
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		t.Fatal("benchmark ran with no state dir or with the audit skipped")
		return nil, nil
	})
	if slow, low := runStartupDiskAudit("", false); slow || low {
		t.Fatalf("no state dir: got (slow %v, low %v), want (false, false)", slow, low)
	}
	t.Setenv("URNETWORK_SKIP_AUDIT", "1")
	if slow, low := runStartupDiskAudit(t.TempDir(), true); slow || low {
		t.Fatalf("URNETWORK_SKIP_AUDIT: got (slow %v, low %v), want (false, false)", slow, low)
	}
}

// On a profile that does not benchmark, the startup audit still reports free
// space, without writing anything.
func TestRunStartupDiskAuditWithoutBenchmarkOnlyChecksSpace(t *testing.T) {
	withBenchEnv(t, false, 0, true)
	withFreeSpace(t, 100)
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		t.Fatal("benchmark ran although it was not requested")
		return nil, nil
	})
	var slow, low bool
	captureStdout(t, func() { slow, low = runStartupDiskAudit(t.TempDir(), false) })
	if slow {
		t.Fatal("the space-only audit reported a slow disk")
	}
	if !low {
		t.Fatal("100 MiB free was not reported as low space")
	}
}
