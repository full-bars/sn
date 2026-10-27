package miner

// hotswap_test.go covers the platform-neutral half of the hotswap port: the
// candidate arg sanitizer, the IPC framing, the executable-path resolution and
// its refusal cases, the coordinator yield, the handoff drain's exit contract,
// and the metrics/state-dir seams the handoff reads.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"go.etcd.io/bbolt"
)

func TestSanitizeCandidateArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"auth-provide drops the consumed auth code", []string{"auth-provide", "abc123secret", "--user"}, []string{"provide", "--user"}},
		{"auth-provide keeps a flag-style next arg", []string{"auth-provide", "-f", "--user"}, []string{"provide", "--user"}},
		{"-f stripped", []string{"provide", "-f", "--port", "8080"}, []string{"provide", "--port", "8080"}},
		{"separated credentials stripped with their values", []string{"provide", "--user_auth", "admin", "--password", "secret"}, []string{"provide"}},
		{"joined credentials stripped", []string{"provide", "--user_auth=admin", "--password=secret"}, []string{"provide"}},
		{"all identity arguments at once", []string{"auth-provide", "ABC123secret", "-f", "--user_auth", "admin", "--password", "secret"}, []string{"provide"}},
		{"unrelated flags survive", []string{"provide", "--port", "8080", "--user"}, []string{"provide", "--port", "8080", "--user"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeCandidateArgs(tc.in); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("sanitizeCandidateArgs(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestHotswapMessageRoundTrip(t *testing.T) {
	var buf strings.Builder
	if err := writeHotswapMessage(&buf, HotswapMessage{Type: HotswapMsgReady, PID: 42, Version: "vX"}); err != nil {
		t.Fatalf("writeHotswapMessage: %v", err)
	}
	msg, err := readHotswapMessage(bufio.NewReader(strings.NewReader(buf.String())))
	if err != nil {
		t.Fatalf("readHotswapMessage: %v", err)
	}
	if msg.Type != HotswapMsgReady || msg.PID != 42 || msg.Version != "vX" || msg.Timestamp.IsZero() {
		t.Fatalf("round trip lost fields: %+v", msg)
	}
	if _, err := readHotswapMessage(bufio.NewReader(strings.NewReader("not json\n"))); err == nil {
		t.Fatal("expected an error for a malformed line")
	}
}

func TestHotSwapExecutablePathAbortsOnUnusableInstallPath(t *testing.T) {
	origInstall, origExec, origStat, origAccess := installPath, executableFunc, statFunc, syscallAccess
	defer func() {
		installPath, executableFunc, statFunc, syscallAccess = origInstall, origExec, origStat, origAccess
	}()

	installPath = filepath.Join(t.TempDir(), "missing-binary")
	executableFunc = func() (string, error) { return "/proc/self/exe", nil }
	syscallAccess = func(string) error { return nil }
	statFunc = os.Stat

	// A handoff exists to change version. When the install path cannot be
	// launched the handoff must abort rather than re-execute the running
	// image, so an updater's verify loop sees the mismatch.
	if _, err := hotSwapExecutablePath(); err == nil {
		t.Fatal("expected an error when the install path is unusable")
	}
}

func TestHotSwapExecutablePathPrefersTheCapturedInstallPath(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "urnet-miner")
	if err := os.WriteFile(installed, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	origInstall, origExec, origStat, origAccess := installPath, executableFunc, statFunc, syscallAccess
	defer func() {
		installPath, executableFunc, statFunc, syscallAccess = origInstall, origExec, origStat, origAccess
	}()
	installPath = installed
	// The updater moved the running image aside: os.Executable now names a
	// backup path. The handoff must launch what is installed at the captured
	// path, not the old build.
	executableFunc = func() (string, error) { return installed + ".backup", nil }
	syscallAccess = func(string) error { return nil }
	statFunc = os.Stat

	exe, err := hotSwapExecutablePath()
	if err != nil {
		t.Fatalf("hotSwapExecutablePath: %v", err)
	}
	if exe != installed {
		t.Fatalf("exe = %q, want the captured install path %q", exe, installed)
	}
}

func TestHandoffBatonSendTakeoverRejectsANonAckMessage(t *testing.T) {
	// The baton releases the process-wide store before it sends TAKEOVER and
	// an aborted handoff reopens it, so the test keeps its own state
	// directory instead of the host's.
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	closeProviderStore()
	t.Cleanup(closeProviderStore)

	session := &HotswapParentSession{
		Reader: bufio.NewReader(strings.NewReader(`{"type":"ERROR","error":"nope"}` + "\n")),
		Writer: io.Discard,
	}
	if err := handoffBatonSendTakeover(session, 1, 2); err == nil {
		t.Fatal("expected the handoff to abort when the candidate does not ACK")
	}
}

// The candidate touches no provider state until TAKEOVER, so its first open of
// provider_state.db comes right after it. That open must not meet the parent's
// bbolt lock: the parent releases the store before it sends TAKEOVER.
func TestHandoffBatonReleasesTheStateStoreBeforeTakeover(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	closeProviderStore()
	t.Cleanup(closeProviderStore)

	// The parent holds the store, as a running provider does.
	if providerStoreInstance() == nil {
		t.Fatal("the parent's store did not open")
	}

	parentConn, candidateConn := net.Pipe()
	t.Cleanup(func() { _ = candidateConn.Close() })
	session := &HotswapParentSession{Reader: bufio.NewReader(parentConn), Writer: parentConn}

	opened := make(chan error, 1)
	go func() {
		msg, err := readHotswapMessage(bufio.NewReader(candidateConn))
		if err != nil || msg.Type != HotswapMsgTakeover {
			opened <- fmt.Errorf("candidate saw %q (%v), want TAKEOVER", msg.Type, err)
			return
		}
		// The candidate opens the store as its run starts, which is after this
		// message. The parent is still alive and draining.
		store, openErr := openProviderStore(filepath.Join(dir, providerStoreFileName))
		if store != nil {
			_ = store.Close()
		}
		// ACK either way, so a slow open is a test failure and not the
		// parent's ACK timeout.
		_, _ = io.WriteString(candidateConn, `{"type":"ACK"}`+"\n")
		opened <- openErr
	}()

	if err := handoffBatonSendTakeover(session, os.Getpid(), 4242); err != nil {
		t.Fatalf("handoff: %s", err)
	}
	if openErr := <-opened; openErr != nil {
		t.Fatalf("the candidate could not open the store the parent released: %s", openErr)
	}
}

// A handoff aborts when the TAKEOVER write fails, and the parent keeps
// serving. The store it released for a candidate that never got the message
// must come back, or every state write for the rest of the parent's life would
// fail: activation records, host capability, every marker.
func TestHandoffBatonRestoresTheStateStoreWhenTakeoverFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	closeProviderStore()
	t.Cleanup(closeProviderStore)

	if err := providerStoreInstance().save(providerStoreBucketState, "probe", map[string]int{"count": 1}); err != nil {
		t.Fatalf("seed the parent's record: %s", err)
	}

	// The candidate's pipe is gone by the time TAKEOVER is written.
	session := &HotswapParentSession{Writer: brokenIPCEWriter{errors.New("broken pipe")}}
	if err := handoffBatonSendTakeover(session, os.Getpid(), 4244); err == nil {
		t.Fatal("the handoff was expected to abort when the TAKEOVER write fails")
	}

	if err := providerStoreInstance().save(providerStoreBucketState, "probe", map[string]int{"count": 2}); err != nil {
		t.Fatalf("save after the aborted handoff = %v, want nil: the parent's state writes stayed dead", err)
	}
	var got map[string]int
	if !providerStoreInstance().load(providerStoreBucketState, "probe", &got) || got["count"] != 2 {
		t.Fatalf("record after the aborted handoff = %+v, want count 2: the write did not land", got)
	}
}

// The candidate may already hold provider_state.db when the handoff aborts. The
// parent then keeps serving with its state writes failing, and the abort has to
// say who holds the file instead of pretending the store came back.
func TestHandoffBatonReportsAStoreTheCandidateStillHolds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	closeProviderStore()
	t.Cleanup(closeProviderStore)

	if err := providerStoreInstance().save(providerStoreBucketState, "probe", map[string]int{"count": 1}); err != nil {
		t.Fatalf("seed the parent's record: %s", err)
	}

	// The candidate takes the file as it acts on TAKEOVER, then the write
	// fails: the handoff aborts with the bbolt lock still taken.
	candidate := &lockHoldingWriter{t: t, path: filepath.Join(dir, providerStoreFileName), err: errors.New("broken pipe")}
	t.Cleanup(candidate.close)
	session := &HotswapParentSession{Writer: candidate}

	out := captureStdout(t, func() {
		if err := handoffBatonSendTakeover(session, os.Getpid(), 4245); err == nil {
			t.Fatal("the handoff was expected to abort when the TAKEOVER write fails")
		}
	})

	// The reopen cannot take a lock the candidate holds: the failure is the
	// lock timeout, the store stays unavailable and the line says why.
	if _, err := openProviderStoreForProcess(); !errors.Is(err, bbolt.ErrTimeout) {
		t.Fatalf("reopen while the candidate holds the file = %v, want %v", err, bbolt.ErrTimeout)
	}
	if store := providerStoreInstance(); store != nil {
		t.Fatal("the parent reopened the file the candidate holds")
	}
	if !strings.Contains(out, "candidate still holds") {
		t.Fatalf("the abort did not say the candidate holds %s: %q", providerStoreFileName, out)
	}
}

// brokenIPCEWriter fails every write, the way a candidate that died mid
// handshake does.
type brokenIPCEWriter struct{ err error }

func (w brokenIPCEWriter) Write([]byte) (int, error) { return 0, w.err }

// lockHoldingWriter fails the TAKEOVER write after taking provider_state.db,
// the way a candidate that already acted on the message and exited badly would
// leave it.
type lockHoldingWriter struct {
	t    *testing.T
	path string
	err  error
	db   *bbolt.DB
}

func (w *lockHoldingWriter) Write([]byte) (int, error) {
	db, err := bbolt.Open(w.path, 0o600, &bbolt.Options{Timeout: 100 * time.Millisecond})
	if err != nil {
		w.t.Errorf("the candidate could not take %s: %s", w.path, err)
	} else {
		w.db = db
	}
	return 0, w.err
}

func (w *lockHoldingWriter) close() {
	if w.db != nil {
		_ = w.db.Close()
	}
}

func TestYieldCoordinatorSessionRunsEveryCloserOnce(t *testing.T) {
	ResetHotSwapStateForTest()
	t.Cleanup(ResetHotSwapStateForTest)

	var first, second atomic.Int64
	unregFirst := RegisterCoordinatorCloser(func() { first.Add(1) })
	RegisterCoordinatorCloser(func() { second.Add(1) })

	yieldCoordinatorSession()
	if first.Load() != 1 || second.Load() != 1 {
		t.Fatalf("closers ran %d/%d times, want 1/1", first.Load(), second.Load())
	}

	unregFirst()
	yieldCoordinatorSession()
	if first.Load() != 1 {
		t.Fatalf("unregistered closer ran again (%d)", first.Load())
	}
	if second.Load() != 2 {
		t.Fatalf("second closer ran %d times, want 2", second.Load())
	}
}

func TestHandoffDrainParentExitsCleanlyOnCancelledContext(t *testing.T) {
	ResetHotSwapStateForTest()
	t.Cleanup(ResetHotSwapStateForTest)

	origExit := exitFunc
	defer func() { exitFunc = origExit }()
	exitCh := make(chan int, 1)
	exitFunc = func(code int) { exitCh <- code }

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a shutdown during the handoff ends the drain immediately

	handoffDrainParent(ctx, func() {}, &HotswapParentSession{}, os.Getpid())

	select {
	case code := <-exitCh:
		if code != 0 {
			t.Fatalf("drain exit code = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the drain never reached its exit")
	}
	if isHotSwapDraining.Load() {
		t.Fatal("draining state not cleared")
	}
	// The drain released its waiters. A run that unwound its slots at the
	// yield blocks here, so this must not block after the drain.
	waitForHotSwapDrain()
}

func TestReadHotswapDeclinesFromDisk(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	if got := readHotswapDeclinesFromDisk(); got != nil {
		t.Fatalf("missing file returned %v, want nil", got)
	}

	data, err := json.Marshal(map[string]any{"counts": map[string]int64{"low_memory": 3, "trigger_signal": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, ".hotswap_declines.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	counts := readHotswapDeclinesFromDisk()
	if counts["low_memory"] != 3 || counts["trigger_signal"] != 1 {
		t.Fatalf("counts = %v, want low_memory=3 trigger_signal=1", counts)
	}
}

func TestCritLogWritesToTheStateDir(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	critLog("FATAL: test handoff failure %d", 7)

	data, err := os.ReadFile(filepath.Join(stateDir, "events.log"))
	if err != nil {
		t.Fatalf("read events.log: %v", err)
	}
	if !strings.Contains(string(data), "FATAL: test handoff failure 7") {
		t.Fatalf("events.log missing the record: %q", string(data))
	}
}

func TestHotSwapControlSocketOwnerFinishIsIdempotentWithoutASocket(t *testing.T) {
	ResetHotSwapStateForTest()
	t.Cleanup(ResetHotSwapStateForTest)

	owner := &hotSwapControlSocketOwner{ctx: context.Background()}
	// A run that never bound a socket (a candidate before takeover) must be
	// able to finish without cleaning up a socket it does not own.
	owner.finish()
	owner.finish()
}

// TestHotSwapHandoffPanicEndsTheProcess covers the entry point both triggers use
// (SIGUSR2 and the control socket's `hotswap` command). A panic in the handoff
// used to be swallowed by the socket goroutine's barrier: the coordinator session
// was already yielded, the socket quiesced and metrics persistence stopped, so
// the parent looked alive and served nothing, and nothing ended it.
func TestHotSwapHandoffPanicEndsTheProcess(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	origExit := exitFunc
	origHandoff := runHotSwapParentHandoffFunc
	t.Cleanup(func() {
		exitFunc = origExit
		runHotSwapParentHandoffFunc = origHandoff
	})

	exitCh := make(chan int, 1)
	exitFunc = func(code int) { exitCh <- code }
	runHotSwapParentHandoffFunc = func(context.Context, context.CancelFunc, docopt.Opts) error {
		panic("handoff exploded past the commit point")
	}

	hotSwapHandoff(context.Background(), func() {}, nil)

	select {
	case code := <-exitCh:
		if code != 1 {
			t.Fatalf("exit code = %d, want 1 (a panicked handoff must end the process non-zero)", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a panic in the handoff neither returned nor exited: the process would live on with a half-yielded run")
	}
}

// TestHotSwapHandoffReturnsErrorsUnchanged makes sure the guard only owns panics:
// the handoff's own abort paths (a candidate that never came READY, an unusable
// install path) must still come back as errors without exiting.
func TestHotSwapHandoffReturnsErrorsUnchanged(t *testing.T) {
	origExit := exitFunc
	origHandoff := runHotSwapParentHandoffFunc
	t.Cleanup(func() {
		exitFunc = origExit
		runHotSwapParentHandoffFunc = origHandoff
	})

	exited := make(chan int, 1)
	exitFunc = func(code int) { exited <- code }
	want := errors.New("candidate pre-flight timeout")
	runHotSwapParentHandoffFunc = func(context.Context, context.CancelFunc, docopt.Opts) error {
		return want
	}

	if err := hotSwapHandoff(context.Background(), func() {}, nil); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	select {
	case code := <-exited:
		t.Fatalf("an aborted handoff exited with %d, want no exit", code)
	default:
	}
}

// TestMonitorCandidateExitReportsAPanic is the candidate-death signal: a panic in
// session.Wait used to send nothing, so the drain monitor's select never fired on
// the child channel, candidateDied stayed false, and the parent exited 0 on the
// drain timeout with a dead candidate.
func TestMonitorCandidateExitReportsAPanic(t *testing.T) {
	ch := monitorCandidateExit("hotswap-test-candidate-wait", func() error {
		panic("wait exploded")
	})
	select {
	case err := <-ch:
		if !errors.Is(err, errCandidateWaitPanicked) {
			t.Fatalf("wait error = %v, want %v", err, errCandidateWaitPanicked)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a panicking candidate wait reported nothing: the drain would read a dead candidate as alive")
	}

	// A normal exit still reports its own result, and a wait error is passed
	// through rather than dressed up as a panic.
	ch = monitorCandidateExit("hotswap-test-candidate-wait", func() error { return nil })
	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("wait error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a normal candidate exit was not reported")
	}

	waitErr := errors.New("exit status 3")
	ch = monitorCandidateExit("hotswap-test-candidate-wait", func() error { return waitErr })
	select {
	case err := <-ch:
		if !errors.Is(err, waitErr) {
			t.Fatalf("wait error = %v, want %v", err, waitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a candidate wait error was not reported")
	}
}
