package miner

// hotswap_test.go covers the platform-neutral half of the hotswap port: the
// candidate arg sanitizer, the IPC framing, the executable-path resolution and
// its refusal cases, the coordinator yield, the handoff drain's exit contract,
// and the metrics/state-dir seams the handoff reads.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	session := &HotswapParentSession{
		Reader: bufio.NewReader(strings.NewReader(`{"type":"ERROR","error":"nope"}` + "\n")),
		Writer: io.Discard,
	}
	if err := handoffBatonSendTakeover(session, 1, 2); err == nil {
		t.Fatal("expected the handoff to abort when the candidate does not ACK")
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
