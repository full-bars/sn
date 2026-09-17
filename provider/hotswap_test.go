//go:build !windows

package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
)

// Adaptation notes:
// - Consolidated test suites from hotswap_common_test.go, hotswap_exec_path_test.go,
//   hotswap_metrics_test.go, and hotswap_test.go into this single test file.
// - Tagged with //go:build unix.
// - Defined withTempHomeHotswap for isolated HOME environments.
// - Added createFakeJWT and createFakeJWTWithClaims test helpers for constructing
//   unverified test tokens.
// - Note: TestClientJWTStoreFlockExclusivity was omitted as client_jwt_store is not part of this repo.

func createFakeHotswapJWTWithClaims(claims map[string]interface{}) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payloadBytes, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return fmt.Sprintf("%s.%s.fakesig", header, payload)
}

func createFakeHotswapJWT(exp int64) string {
	return createFakeHotswapJWTWithClaims(map[string]interface{}{"exp": float64(exp)})
}

func withTempHomeHotswap(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

// ---------------------------------------------------------------------------
// From hotswap_common_test.go: Framing tests
// ---------------------------------------------------------------------------

func TestHotSwapMessageFraming(t *testing.T) {
	var buf bytes.Buffer

	want := HotswapMessage{
		Type:    HotswapMsgReady,
		Version: "v3.23.0-fix.31.0",
		PID:     12345,
	}

	if err := writeHotswapMessage(&buf, want); err != nil {
		t.Fatalf("writeHotswapMessage: %v", err)
	}

	reader := bufio.NewReader(&buf)
	got, err := readHotswapMessage(reader)
	if err != nil {
		t.Fatalf("readHotswapMessage: %v", err)
	}

	if got.Type != want.Type {
		t.Errorf("got.Type = %v, want %v", got.Type, want.Type)
	}
	if got.Version != want.Version {
		t.Errorf("got.Version = %v, want %v", got.Version, want.Version)
	}
	if got.PID != want.PID {
		t.Errorf("got.PID = %v, want %v", got.PID, want.PID)
	}
	if got.Timestamp.IsZero() {
		t.Errorf("expected non-zero Timestamp in received message")
	}
}

// ---------------------------------------------------------------------------
// From hotswap_exec_path_test.go: Executable path resolution tests
// ---------------------------------------------------------------------------

// withExecutableEnv swaps the package's executable-resolution seams and the
// captured install path for the duration of a test.
func withExecutableEnv(t *testing.T, captured string, running string, runningErr error) {
	t.Helper()
	origInstall := installPath
	origExecutable := executableFunc
	origStat := statFunc
	t.Cleanup(func() {
		installPath = origInstall
		executableFunc = origExecutable
		statFunc = origStat
	})
	installPath = captured
	executableFunc = func() (string, error) { return running, runningErr }
	statFunc = os.Stat
}

// The update flow installs the new build at the provider's binary path and
// moves the running one to a backup. From then on os.Executable() names the
// backup, so a handoff must launch the captured install path or it re-executes
// the build it was meant to replace.
func TestHotSwapExecutablePathPrefersInstallPathAfterBinaryMoved(t *testing.T) {
	dir := t.TempDir()
	install := filepath.Join(dir, "urnetwork")
	backup := filepath.Join(dir, "urnetwork.bak-20260911T100157Z")
	for _, p := range []string{install, backup} {
		if err := os.WriteFile(p, []byte("binary"), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	withExecutableEnv(t, install, backup, nil)

	got, err := hotSwapExecutablePath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != install {
		t.Fatalf("expected the handoff to launch the install path %s, got %s", install, got)
	}
}

// Nothing moved: both agree, and the answer is that path.
func TestHotSwapExecutablePathUnchangedBinary(t *testing.T) {
	dir := t.TempDir()
	install := filepath.Join(dir, "urnetwork")
	if err := os.WriteFile(install, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	withExecutableEnv(t, install, install, nil)

	got, err := hotSwapExecutablePath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != install {
		t.Fatalf("expected %s, got %s", install, got)
	}
}

// A missing install path aborts. Falling back to the running image would
// re-execute the build the handoff is meant to replace while reporting a clean
// zero-downtime swap, which is the failure this function exists to remove.
func TestHotSwapExecutablePathAbortsWhenInstallPathGone(t *testing.T) {
	dir := t.TempDir()
	install := filepath.Join(dir, "urnetwork")
	running := filepath.Join(dir, "urnetwork.bak")
	if err := os.WriteFile(running, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	withExecutableEnv(t, install, running, nil)

	got, err := hotSwapExecutablePath()
	if err == nil {
		t.Fatalf("expected an abort when the install path is gone, got %s", got)
	}
	if got != "" {
		t.Fatalf("expected no path on abort, got %s", got)
	}
}

// An install path that exists but lacks execute permission self-heals via
// chmod 0755 on Unix before the handoff proceeds. This path exists at 0644
// (e.g. an updater that wrote the binary but hasn't yet set execute bits).
// The pre-spawn self-heal repairs permissions so the candidate can launch,
// preserving the hotswap-heal repair path rather than hard-aborting.
func TestHotSwapExecutablePathSelfHealsWhenInstallPathNotExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute-bit self-heal is Unix-only")
	}
	dir := t.TempDir()
	install := filepath.Join(dir, "urnetwork")
	if err := os.WriteFile(install, []byte("binary"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	withExecutableEnv(t, install, install, nil)

	got, err := hotSwapExecutablePath()
	if err != nil {
		t.Fatalf("expected self-heal, got error: %v", err)
	}
	if got != install {
		t.Fatalf("expected %s, got %s", install, got)
	}
}

// An install path replaced by a directory aborts rather than being spawned.
func TestHotSwapExecutablePathAbortsWhenInstallPathIsDirectory(t *testing.T) {
	dir := t.TempDir()
	install := filepath.Join(dir, "urnetwork")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	withExecutableEnv(t, install, install, nil)

	if _, err := hotSwapExecutablePath(); err == nil {
		t.Fatal("expected an abort when the install path is a directory")
	}
}

// readlink on /proc/self/exe appends " (deleted)" once the running binary is
// unlinked, and os.Executable hands that back with a nil error. The suffix must
// never reach exec.
func TestHotSwapExecutablePathStripsDeletedSuffix(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "urnetwork")
	if err := os.WriteFile(running, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	withExecutableEnv(t, "", running+" (deleted)", nil)

	got, err := hotSwapExecutablePath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != running {
		t.Fatalf("expected the deleted suffix stripped, got %s", got)
	}
}

// When the running image was unlinked (Linux deletes-in-place), the handoff
// logs an informational message distinguishing "unlinked" from "moved" so
// operators can see when their binary was replaced vs. renamed. The install
// path still launches the new build.
func TestHotSwapExecutablePathLogsUnlinkedRunningImage(t *testing.T) {
	dir := t.TempDir()
	install := filepath.Join(dir, "urnetwork")
	running := filepath.Join(dir, "urnetwork.bak")
	for _, p := range []string{install, running} {
		if err := os.WriteFile(p, []byte("binary"), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	withExecutableEnv(t, install, running+" (deleted)", nil)

	got, err := hotSwapExecutablePath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != install {
		t.Fatalf("expected the handoff to launch the install path %s, got %s", install, got)
	}
}

// Nothing was captured at startup, so the running image is all there is.
func TestHotSwapExecutablePathWithoutCapturedInstallPath(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "urnetwork")
	if err := os.WriteFile(running, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	withExecutableEnv(t, "", running, nil)

	got, err := hotSwapExecutablePath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != running {
		t.Fatalf("expected the running image, got %s", got)
	}
}

// Neither path is resolvable, so the handoff must abort rather than guess.
func TestHotSwapExecutablePathErrorsWhenNothingResolves(t *testing.T) {
	dir := t.TempDir()
	withExecutableEnv(t, filepath.Join(dir, "missing"), "", errors.New("no exe"))

	if _, err := hotSwapExecutablePath(); err == nil {
		t.Fatal("expected an error when neither the install path nor the running image resolves")
	}
}

// The captured path must name the real binary, not a synthetic link, so a
// handoff can stat it and spawn it.
func TestInstallPathCapturedAtStartupIsAResolvedFile(t *testing.T) {
	if installPath == "" {
		t.Skip("no executable path available in this environment")
	}
	if _, err := os.Stat(installPath); err != nil {
		t.Fatalf("captured install path %s is not usable: %v", installPath, err)
	}
	if installPath == "/proc/self/exe" {
		t.Fatal("captured install path must be resolved, not the /proc symlink")
	}
}

// ---------------------------------------------------------------------------
// From hotswap_metrics_test.go: Decline counters
// ---------------------------------------------------------------------------

func TestReadHotswapDeclinesFromDisk(t *testing.T) {
	withTempHomeHotswap(t)
	stateDir := mustStateDir()
	if stateDir == "" {
		t.Skip("mustStateDir returned empty")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a decline file the same way urnet-tools CLI would.
	counts := map[string]int64{"version_old": 3, "unit_not_notify": 1, "success": 5}
	data, _ := json.Marshal(struct {
		Counts map[string]int64 `json:"counts"`
	}{Counts: counts})
	if err := os.WriteFile(filepath.Join(stateDir, ".hotswap_declines.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	result := readHotswapDeclinesFromDisk()
	if result == nil {
		t.Fatal("readHotswapDeclinesFromDisk returned nil for existing file")
	}
	if result["version_old"] != 3 {
		t.Errorf("version_old = %d, want 3", result["version_old"])
	}
	if result["unit_not_notify"] != 1 {
		t.Errorf("unit_not_notify = %d, want 1", result["unit_not_notify"])
	}
	if result["success"] != 5 {
		t.Errorf("success = %d, want 5", result["success"])
	}
}

func TestReadHotswapDeclinesFromDiskMissing(t *testing.T) {
	withTempHomeHotswap(t)
	if mustStateDir() == "" {
		t.Skip("mustStateDir returned empty")
	}

	if result := readHotswapDeclinesFromDisk(); result != nil {
		t.Errorf("readHotswapDeclinesFromDisk with missing file returned %v, want nil", result)
	}
}

// ---------------------------------------------------------------------------
// From hotswap_test.go: Hotswap protocol and handoff lifecycle tests
// ---------------------------------------------------------------------------

func TestHotSwapPreflightValidation(t *testing.T) {
	// Setup isolated HOME
	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)
	os.Setenv("HOME", tempHome)

	urDir := filepath.Join(tempHome, ".urnetwork")
	if err := os.MkdirAll(urDir, 0700); err != nil {
		t.Fatal(err)
	}

	// 1. Missing JWT should fail
	dummyOpts := docopt.Opts{}
	err := runHotSwapChildPreflight(dummyOpts, "https://127.0.0.1:443")
	if err == nil {
		t.Errorf("expected error on missing JWT, got nil")
	}

	// 2. Expired JWT should fail
	expiredToken := createFakeHotswapJWT(time.Now().Add(-1 * time.Hour).Unix())
	jwtPath := filepath.Join(urDir, "jwt")
	if err := os.WriteFile(jwtPath, []byte(expiredToken), 0600); err != nil {
		t.Fatal(err)
	}

	err = runHotSwapChildPreflight(dummyOpts, "https://127.0.0.1:443")
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("expired")) {
		t.Errorf("expected token expired error, got: %v", err)
	}

	// 3. Valid future JWT with local listener should pass
	validToken := createFakeHotswapJWT(time.Now().Add(24 * time.Hour).Unix())
	if err := os.WriteFile(jwtPath, []byte(validToken), 0600); err != nil {
		t.Fatal(err)
	}

	// Start a mock API listener to verify dial
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	mockApiUrl := "http://" + listener.Addr().String()
	err = runHotSwapChildPreflight(dummyOpts, mockApiUrl)
	if err != nil {
		t.Errorf("expected valid preflight to succeed, got: %v", err)
	}
}

func TestHotSwapHandshakeSuccess(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentSock := os.NewFile(uintptr(fds[0]), "parent")
	childSock := os.NewFile(uintptr(fds[1]), "child")
	defer parentSock.Close()
	defer childSock.Close()

	// Setup isolated HOME with valid JWT
	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)
	os.Setenv("HOME", tempHome)

	urDir := filepath.Join(tempHome, ".urnetwork")
	_ = os.MkdirAll(urDir, 0700)
	validToken := createFakeHotswapJWT(time.Now().Add(48 * time.Hour).Unix())
	_ = os.WriteFile(filepath.Join(urDir, "jwt"), []byte(validToken), 0600)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	mockApiUrl := "http://" + listener.Addr().String()

	childDone := make(chan error, 1)

	// Run child handshake in goroutine
	go func() {
		childDone <- runHotSwapChildHandshake(childSock, docopt.Opts{}, mockApiUrl)
	}()

	// Parent side verification
	parentReader := bufio.NewReader(parentSock)

	// 1. Parent reads READY
	msg, err := readHotswapMessage(parentReader)
	if err != nil {
		t.Fatalf("parent read READY: %v", err)
	}
	if msg.Type != HotswapMsgReady {
		t.Fatalf("expected READY, got %s (err=%s)", msg.Type, msg.Error)
	}

	// 2. Parent sends TAKEOVER
	if err := writeHotswapMessage(parentSock, HotswapMessage{
		Type: HotswapMsgTakeover,
		PID:  os.Getpid(),
	}); err != nil {
		t.Fatalf("parent send TAKEOVER: %v", err)
	}

	// 3. Child completes handshake
	select {
	case err := <-childDone:
		if err != nil {
			t.Fatalf("child handshake failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for child handshake")
	}

	// 4. Child sends ACK
	if err := runHotSwapChildAck(childSock); err != nil {
		t.Fatalf("child send ACK: %v", err)
	}

	ackMsg, err := readHotswapMessage(parentReader)
	if err != nil {
		t.Fatalf("parent read ACK: %v", err)
	}
	if ackMsg.Type != HotswapMsgAck {
		t.Fatalf("expected ACK, got %s", ackMsg.Type)
	}
}

func TestHotSwapHandshakeChildFailure(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentSock := os.NewFile(uintptr(fds[0]), "parent")
	childSock := os.NewFile(uintptr(fds[1]), "child")
	defer parentSock.Close()
	defer childSock.Close()

	// Empty HOME with no JWT should trigger ERROR message
	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)
	os.Setenv("HOME", tempHome)

	childDone := make(chan error, 1)
	go func() {
		childDone <- runHotSwapChildHandshake(childSock, docopt.Opts{}, "https://127.0.0.1:443")
	}()

	parentReader := bufio.NewReader(parentSock)
	msg, err := readHotswapMessage(parentReader)
	if err != nil {
		t.Fatalf("parent read message: %v", err)
	}
	if msg.Type != HotswapMsgError {
		t.Fatalf("expected ERROR message, got %s", msg.Type)
	}
	if msg.Error == "" {
		t.Errorf("expected error message content, got empty string")
	}

	select {
	case err := <-childDone:
		if err == nil {
			t.Fatal("expected child handshake to return error, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for child handshake to fail")
	}
}

func TestYieldCoordinatorSession(t *testing.T) {
	ClearCoordinatorClosers()
	called1 := false
	called2 := false
	RegisterCoordinatorCloser(func() {
		called1 = true
	})
	RegisterCoordinatorCloser(func() {
		called2 = true
	})

	yieldCoordinatorSession()

	if !called1 || !called2 {
		t.Errorf("expected all coordinator closers to be called: called1=%v called2=%v", called1, called2)
	}

	// Clearing closers
	ClearCoordinatorClosers()
	called1 = false
	called2 = false
	yieldCoordinatorSession()
	if called1 || called2 {
		t.Errorf("expected no closers to be called after clearing")
	}
}

func TestNotifySystemdMainPID(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "notify.sock")

	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unixgram: %v", err)
	}
	defer l.Close()

	origNotify := os.Getenv("NOTIFY_SOCKET")
	defer os.Setenv("NOTIFY_SOCKET", origNotify)
	os.Setenv("NOTIFY_SOCKET", sockPath)

	if err := notifySystemdMainPID(45678); err != nil {
		t.Fatalf("notifySystemdMainPID: %v", err)
	}

	buf := make([]byte, 128)
	n, err := l.Read(buf)
	if err != nil {
		t.Fatalf("read notify socket: %v", err)
	}

	got := string(buf[:n])
	want := "MAINPID=45678\n"
	if got != want {
		t.Errorf("got notify payload %q, want %q", got, want)
	}
}

func TestNotifySystemdReadyAndStatus(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "notify.sock")

	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unixgram: %v", err)
	}
	defer l.Close()

	origNotify := os.Getenv("NOTIFY_SOCKET")
	defer os.Setenv("NOTIFY_SOCKET", origNotify)
	os.Setenv("NOTIFY_SOCKET", sockPath)

	if err := notifySystemdReady(); err != nil {
		t.Fatalf("notifySystemdReady: %v", err)
	}

	buf := make([]byte, 128)
	n, err := l.Read(buf)
	if err != nil {
		t.Fatalf("read notify socket: %v", err)
	}
	if string(buf[:n]) != "READY=1\n" {
		t.Errorf("got %q, want READY=1\n", string(buf[:n]))
	}

	if err := notifySystemdStatus("healthy"); err != nil {
		t.Fatalf("notifySystemdStatus: %v", err)
	}

	n, err = l.Read(buf)
	if err != nil {
		t.Fatalf("read notify socket: %v", err)
	}
	if string(buf[:n]) != "STATUS=healthy\n" {
		t.Errorf("got %q, want STATUS=healthy\n", string(buf[:n]))
	}
}

func TestHotSwapCanaryHandshake(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentSock := os.NewFile(uintptr(fds[0]), "parent")
	childSock := os.NewFile(uintptr(fds[1]), "child")
	defer parentSock.Close()
	defer childSock.Close()

	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)
	os.Setenv("HOME", tempHome)

	urDir := filepath.Join(tempHome, ".urnetwork")
	_ = os.MkdirAll(urDir, 0700)
	validToken := createFakeHotswapJWT(time.Now().Add(48 * time.Hour).Unix())
	_ = os.WriteFile(filepath.Join(urDir, "jwt"), []byte(validToken), 0600)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	mockApiUrl := "http://" + listener.Addr().String()

	origExit := exitFunc
	exitCalled := make(chan int, 1)
	exitFunc = func(code int) {
		exitCalled <- code
	}
	defer func() { exitFunc = origExit }()

	childDone := make(chan error, 1)
	go func() {
		childDone <- runHotSwapChildHandshake(childSock, docopt.Opts{}, mockApiUrl)
	}()

	parentReader := bufio.NewReader(parentSock)
	msg, err := readHotswapMessage(parentReader)
	if err != nil {
		t.Fatalf("parent read READY: %v", err)
	}
	if msg.Type != HotswapMsgReady {
		t.Fatalf("expected READY, got %s", msg.Type)
	}

	// Send CANARY_DONE from parent
	if err := writeHotswapMessage(parentSock, HotswapMessage{
		Type: HotswapMsgCanaryDone,
		PID:  1,
	}); err != nil {
		t.Fatalf("parent send CANARY_DONE: %v", err)
	}

	select {
	case code := <-exitCalled:
		if code != 0 {
			t.Errorf("expected exit code 0 on CANARY_DONE, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for canary exit on CANARY_DONE")
	}
}

func TestHotSwapParentPID1ExecveSuccess(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "parent")
	childFile := os.NewFile(uintptr(fds[1]), "child")
	defer parentFile.Close()
	defer childFile.Close()

	origGetpid := getpidFunc
	origExecInPlace := execInPlaceFunc
	origSpawn := spawnCandidateFunc
	defer func() {
		getpidFunc = origGetpid
		execInPlaceFunc = origExecInPlace
		spawnCandidateFunc = origSpawn
	}()

	getpidFunc = func() int { return 1 } // simulate Docker PID 1

	execCalled := make(chan struct{}, 1)
	execInPlaceFunc = func(exe string, args []string, env []string) error {
		// Verify URNETWORK_HOTSWAP is stripped from cleanEnv
		for _, e := range env {
			if e == EnvHotSwap+"=1" {
				t.Errorf("cleanEnv should not contain %s", EnvHotSwap)
			}
		}
		execCalled <- struct{}{}
		return nil
	}

	spawnCandidateFunc = func(exe string, args []string) (*HotswapParentSession, error) {
		return &HotswapParentSession{
			childCmd: nil,
			parentFd: parentFile,
			Reader:   bufio.NewReader(parentFile),
			Writer:   parentFile,
		}, nil
	}

	coordinatorYielded := false
	ClearCoordinatorClosers()
	RegisterCoordinatorCloser(func() {
		coordinatorYielded = true
	})

	// Simulate candidate announcing READY on childFile
	go func() {
		childReader := bufio.NewReader(childFile)
		_ = writeHotswapMessage(childFile, HotswapMessage{
			Type:    HotswapMsgReady,
			PID:     2,
			Version: "v3.23.0-fix.31.0",
		})
		// Candidate waits for CANARY_DONE
		resp, _ := readHotswapMessage(childReader)
		if resp.Type != HotswapMsgCanaryDone {
			t.Errorf("candidate expected CANARY_DONE, got %s", resp.Type)
		}
	}()

	err = runHotSwapParentHandoff(context.Background(), func() {}, docopt.Opts{})
	if err != nil {
		t.Fatalf("runHotSwapParentHandoff returned error: %v", err)
	}

	select {
	case <-execCalled:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for in-place execve call")
	}

	if !coordinatorYielded {
		t.Errorf("expected coordinator session to be yielded before execve")
	}
}

func TestHotSwapParentPID1PreflightFailurePreservesProcess(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "parent")
	childFile := os.NewFile(uintptr(fds[1]), "child")
	defer parentFile.Close()
	defer childFile.Close()

	origGetpid := getpidFunc
	origExecInPlace := execInPlaceFunc
	origSpawn := spawnCandidateFunc
	defer func() {
		getpidFunc = origGetpid
		execInPlaceFunc = origExecInPlace
		spawnCandidateFunc = origSpawn
	}()

	getpidFunc = func() int { return 1 }

	execCalled := false
	execInPlaceFunc = func(exe string, args []string, env []string) error {
		execCalled = true
		return nil
	}

	spawnCandidateFunc = func(exe string, args []string) (*HotswapParentSession, error) {
		return &HotswapParentSession{
			childCmd: nil,
			parentFd: parentFile,
			Reader:   bufio.NewReader(parentFile),
			Writer:   parentFile,
		}, nil
	}

	coordinatorYielded := false
	ClearCoordinatorClosers()
	RegisterCoordinatorCloser(func() {
		coordinatorYielded = true
	})

	// Simulate candidate reporting ERROR
	go func() {
		_ = writeHotswapMessage(childFile, HotswapMessage{
			Type:  HotswapMsgError,
			PID:   2,
			Error: "simulated pre-flight failure",
		})
	}()

	err = runHotSwapParentHandoff(context.Background(), func() {}, docopt.Opts{})
	if err == nil {
		t.Fatal("expected error on candidate preflight failure, got nil")
	}

	if execCalled {
		t.Errorf("execve must NOT be called on preflight failure")
	}
	if coordinatorYielded {
		t.Errorf("coordinator must NOT be yielded on preflight failure")
	}
}

func TestHotSwapPreflightPermissionHealing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission healing is unobservable as root (CAP_DAC_OVERRIDE)")
	}

	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)
	os.Setenv("HOME", tempHome)

	urDir := filepath.Join(tempHome, ".urnetwork")
	_ = os.MkdirAll(urDir, 0700)
	validToken := createFakeHotswapJWT(time.Now().Add(48 * time.Hour).Unix())
	jwtPath := filepath.Join(urDir, "jwt")
	_ = os.WriteFile(jwtPath, []byte(validToken), 0600)

	// Restrict permissions to unreadable
	_ = os.Chmod(jwtPath, 0000)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	mockApiUrl := "http://" + listener.Addr().String()

	err = runHotSwapChildPreflight(docopt.Opts{}, mockApiUrl)
	if err != nil {
		t.Fatalf("expected preflight to heal permissions and succeed, got: %v", err)
	}

	fi, err := os.Stat(jwtPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("expected healed permissions 0600, got %v", fi.Mode().Perm())
	}
}

func TestHotSwapParentStandardBatonSuccess(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("NOTIFY_SOCKET", "")

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "parent")
	childFile := os.NewFile(uintptr(fds[1]), "child")
	defer parentFile.Close()
	defer childFile.Close()

	ResetHotSwapStateForTest()
	defer ResetHotSwapStateForTest()

	origGetpid := getpidFunc
	origSpawn := spawnCandidateFunc
	origExit := exitFunc
	defer func() {
		getpidFunc = origGetpid
		spawnCandidateFunc = origSpawn
		exitFunc = origExit
	}()

	getpidFunc = func() int { return 9999 } // Host non-PID-1
	parentExited := make(chan int, 1)
	exitFunc = func(code int) { parentExited <- code }

	spawnCandidateFunc = func(exe string, args []string) (*HotswapParentSession, error) {
		return &HotswapParentSession{
			childCmd: nil,
			parentFd: parentFile,
			Reader:   bufio.NewReader(parentFile),
			Writer:   parentFile,
		}, nil
	}

	coordinatorYielded := false
	ClearCoordinatorClosers()
	unreg := RegisterCoordinatorCloser(func() {
		coordinatorYielded = true
	})
	defer unreg()

	// Child simulation: announce READY -> wait TAKEOVER -> send ACK
	childErr := make(chan error, 1)
	go func() {
		childReader := bufio.NewReader(childFile)
		if err := writeHotswapMessage(childFile, HotswapMessage{
			Type:    HotswapMsgReady,
			PID:     10000,
			Version: "v3.23.0-fix.31.0",
		}); err != nil {
			childErr <- err
			return
		}

		msg, err := readHotswapMessage(childReader)
		if err != nil {
			childErr <- err
			return
		}
		if msg.Type != HotswapMsgTakeover {
			childErr <- fmt.Errorf("child expected TAKEOVER, got %s", msg.Type)
			return
		}

		if err := writeHotswapMessage(childFile, HotswapMessage{
			Type: HotswapMsgAck,
			PID:  10000,
		}); err != nil {
			childErr <- err
			return
		}
		childErr <- nil
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = runHotSwapParentHandoff(ctx, cancel, docopt.Opts{})
	if err != nil {
		t.Fatalf("runHotSwapParentHandoff returned error: %v", err)
	}

	select {
	case err := <-childErr:
		if err != nil {
			t.Fatalf("child simulation error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for child simulation")
	}

	// Cancel context to complete drain sleep immediately in test
	cancel()
	select {
	case code := <-parentExited:
		if code != 0 {
			t.Errorf("expected exit code 0 on drain, got %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for parent exit on drain")
	}

	if !coordinatorYielded {
		t.Errorf("expected coordinator session to be yielded during handoff")
	}
}

func TestHotSwapParentStandardBatonAckTimeoutAborts(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("NOTIFY_SOCKET", "")

	ResetHotSwapStateForTest()
	defer ResetHotSwapStateForTest()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "parent")
	childFile := os.NewFile(uintptr(fds[1]), "child")
	defer parentFile.Close()
	defer childFile.Close()

	origGetpid := getpidFunc
	origSpawn := spawnCandidateFunc
	defer func() {
		getpidFunc = origGetpid
		spawnCandidateFunc = origSpawn
	}()

	getpidFunc = func() int { return 9999 }

	spawnCandidateFunc = func(exe string, args []string) (*HotswapParentSession, error) {
		return &HotswapParentSession{
			childCmd: nil,
			parentFd: parentFile,
			Reader:   bufio.NewReader(parentFile),
			Writer:   parentFile,
		}, nil
	}

	// Child announces READY, receives TAKEOVER, but closes connection post-TAKEOVER (crash)
	go func() {
		childReader := bufio.NewReader(childFile)
		_ = writeHotswapMessage(childFile, HotswapMessage{
			Type:    HotswapMsgReady,
			PID:     10000,
			Version: "v3.23.0-fix.31.0",
		})
		_, _ = readHotswapMessage(childReader)
		// Close childFile to simulate candidate crash post-TAKEOVER
		childFile.Close()
	}()

	err = runHotSwapParentHandoff(context.Background(), func() {}, docopt.Opts{})
	if err == nil {
		t.Fatal("expected error on candidate unconfirmed takeover, got nil")
	}
}

func TestHotSwapParentSystemdMissingNotifySocket(t *testing.T) {
	t.Setenv("INVOCATION_ID", "service-invocation-12345")
	t.Setenv("NOTIFY_SOCKET", "")

	ResetHotSwapStateForTest()
	defer ResetHotSwapStateForTest()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "parent")
	childFile := os.NewFile(uintptr(fds[1]), "child")
	defer parentFile.Close()
	defer childFile.Close()

	origGetpid := getpidFunc
	origSpawn := spawnCandidateFunc
	defer func() {
		getpidFunc = origGetpid
		spawnCandidateFunc = origSpawn
	}()

	getpidFunc = func() int { return 9999 }

	spawnCandidateFunc = func(exe string, args []string) (*HotswapParentSession, error) {
		return &HotswapParentSession{
			childCmd: nil,
			parentFd: parentFile,
			Reader:   bufio.NewReader(parentFile),
			Writer:   parentFile,
		}, nil
	}

	go func() {
		_ = writeHotswapMessage(childFile, HotswapMessage{
			Type:    HotswapMsgReady,
			PID:     10000,
			Version: "v3.23.0-fix.31.0",
		})
	}()

	err = runHotSwapParentHandoff(context.Background(), func() {}, docopt.Opts{})
	if err == nil || !strings.Contains(err.Error(), "zero-downtime hotswap unavailable") {
		t.Fatalf("expected hotswap unavailable error when INVOCATION_ID is set without NOTIFY_SOCKET, got %v", err)
	}
}

func TestRegisterCoordinatorCloserUnregister(t *testing.T) {
	ClearCoordinatorClosers()

	called := false
	unreg := RegisterCoordinatorCloser(func() {
		called = true
	})

	// Before unregister, yield calls closer
	yieldCoordinatorSession()
	if !called {
		t.Fatalf("closer was not called")
	}

	// Unregister
	unreg()
	called = false

	// After unregister, closer is not called
	yieldCoordinatorSession()
	if called {
		t.Fatalf("unregistered closer was unexpectedly called (memory leak)")
	}
}

func TestGetHotSwapChildIPCValidation(t *testing.T) {
	origEnv := os.Getenv(EnvHotSwap)
	defer os.Setenv(EnvHotSwap, origEnv)

	// If env var is not set, returns false
	os.Unsetenv(EnvHotSwap)
	if _, isChild := getHotSwapChildIPC(); isChild {
		t.Errorf("expected isChild=false when %s is unset", EnvHotSwap)
	}

	// If env var is set but fd 3 is not a socket, returns false (F-11)
	os.Setenv(EnvHotSwap, "1")
	// On arbitrary process fd 3 is typically either closed or not a socket
	_ = syscall.Close(3)
	if _, isChild := getHotSwapChildIPC(); isChild {
		t.Errorf("expected isChild=false when fd 3 is invalid/closed")
	}
}

func TestSanitizeCandidateArgs(t *testing.T) {
	// auth-provide with positional auth code should become pure "provide"
	got := sanitizeCandidateArgs([]string{"auth-provide", "abc123secret", "--user"})
	want := []string{"provide", "--user"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("auth-provide + code: got %v, want %v", got, want)
	}

	// auth-provide with flag-style next arg should not skip it
	got = sanitizeCandidateArgs([]string{"auth-provide", "-f", "--user"})
	want = []string{"provide", "--user"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("auth-provide + flags: got %v, want %v", got, want)
	}

	// -f flag stripped
	got = sanitizeCandidateArgs([]string{"provide", "-f", "--port", "8080"})
	want = []string{"provide", "--port", "8080"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("-f strip: got %v, want %v", got, want)
	}

	// --user_auth and --password flags stripped, including their positional values (separated form)
	got = sanitizeCandidateArgs([]string{"provide", "--user_auth", "admin", "--password", "secret"})
	want = []string{"provide"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("auth args strip: got %v, want %v", got, want)
	}

	// --user_auth=val form (value attached) is fully stripped
	got = sanitizeCandidateArgs([]string{"provide", "--user_auth=admin", "--password=secret"})
	want = []string{"provide"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("attached auth args strip: got %v, want %v", got, want)
	}

	// Full auth-provide invocation sanitized to pure provide
	got = sanitizeCandidateArgs([]string{"auth-provide", "ABC123secret", "-f", "--user_auth", "admin", "--password", "secret"})
	want = []string{"provide"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("full auth-provide sanitize: got %v, want %v", got, want)
	}

	// Normal args pass through unchanged
	got = sanitizeCandidateArgs([]string{"provide", "--port", "8080", "--user"})
	want = []string{"provide", "--port", "8080", "--user"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("passthrough: got %v, want %v", got, want)
	}
}

// TestHotSwapSessionWithoutChildIsNotADeadCandidate pins the invariant behind
// the drain-liveness monitor: a session with no child process must never be
// reported as a candidate that died.
func TestHotSwapSessionWithoutChildIsNotADeadCandidate(t *testing.T) {
	if (&HotswapParentSession{}).hasChildProcess() {
		t.Fatal("a session with no childCmd must not report a child process")
	}
	if (&HotswapParentSession{childCmd: &exec.Cmd{}}).hasChildProcess() != true {
		t.Fatal("a session with a childCmd must report a child process")
	}
}

// TestHotSwapParentDrainExitsCleanlyRepeatedly runs the standard baton handoff
// through to the drain exit repeatedly.
func TestHotSwapParentDrainExitsCleanlyRepeatedly(t *testing.T) {
	for i := 0; i < 50; i++ {
		if code := runBatonHandoffOnce(t); code != 0 {
			t.Fatalf("iteration %d: expected exit code 0 on drain, got %d", i, code)
		}
	}
}

// runBatonHandoffOnce drives one parent-side baton handoff with a simulated
// candidate and returns the exit code the parent reported on drain.
func runBatonHandoffOnce(t *testing.T) int {
	t.Helper()
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("NOTIFY_SOCKET", "")

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "parent")
	childFile := os.NewFile(uintptr(fds[1]), "child")
	defer parentFile.Close()
	defer childFile.Close()

	ResetHotSwapStateForTest()
	defer ResetHotSwapStateForTest()

	origGetpid, origSpawn, origExit := getpidFunc, spawnCandidateFunc, exitFunc
	defer func() {
		getpidFunc, spawnCandidateFunc, exitFunc = origGetpid, origSpawn, origExit
	}()

	getpidFunc = func() int { return 9999 }
	parentExited := make(chan int, 1)
	exitFunc = func(code int) { parentExited <- code }
	// childCmd stays nil: this is the shape that used to race the drain select.
	spawnCandidateFunc = func(exe string, args []string) (*HotswapParentSession, error) {
		return &HotswapParentSession{
			parentFd: parentFile,
			Reader:   bufio.NewReader(parentFile),
			Writer:   parentFile,
		}, nil
	}

	ClearCoordinatorClosers()
	unreg := RegisterCoordinatorCloser(func() {})
	defer unreg()

	childErr := make(chan error, 1)
	go func() {
		childReader := bufio.NewReader(childFile)
		if err := writeHotswapMessage(childFile, HotswapMessage{
			Type: HotswapMsgReady, PID: 10000, Version: "v3.23.0-fix.31.0",
		}); err != nil {
			childErr <- err
			return
		}
		msg, err := readHotswapMessage(childReader)
		if err != nil {
			childErr <- err
			return
		}
		if msg.Type != HotswapMsgTakeover {
			childErr <- fmt.Errorf("child expected TAKEOVER, got %s", msg.Type)
			return
		}
		childErr <- writeHotswapMessage(childFile, HotswapMessage{
			Type: HotswapMsgAck, PID: 10000,
		})
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := runHotSwapParentHandoff(ctx, cancel, docopt.Opts{}); err != nil {
		t.Fatalf("runHotSwapParentHandoff returned error: %v", err)
	}
	select {
	case err := <-childErr:
		if err != nil {
			t.Fatalf("child simulation error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for child simulation")
	}

	cancel()
	select {
	case code := <-parentExited:
		return code
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for parent exit on drain")
	}
	return -1
}

// TestHotSwapParentPID1ExecveSanitizesArgs: the Docker in-place execve must
// drop the auth code and credential flags the same way the spawned canary
// does.
func TestHotSwapParentPID1ExecveSanitizesArgs(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "parent")
	childFile := os.NewFile(uintptr(fds[1]), "child")
	defer parentFile.Close()
	defer childFile.Close()

	origArgs := os.Args
	origGetpid := getpidFunc
	origExecInPlace := execInPlaceFunc
	origSpawn := spawnCandidateFunc
	defer func() {
		os.Args = origArgs
		getpidFunc = origGetpid
		execInPlaceFunc = origExecInPlace
		spawnCandidateFunc = origSpawn
	}()

	os.Args = []string{"/app/urnetwork_amd64_stable", "auth-provide", "CONSUMED-CODE", "-f", "--user_auth", "someone", "--password=secret", "--port", "8080"}
	getpidFunc = func() int { return 1 }

	var spawnArgs []string
	execArgs := make(chan []string, 1)
	execInPlaceFunc = func(exe string, args []string, env []string) error {
		execArgs <- args
		return nil
	}
	spawnCandidateFunc = func(exe string, args []string) (*HotswapParentSession, error) {
		spawnArgs = args
		return &HotswapParentSession{
			childCmd: nil,
			parentFd: parentFile,
			Reader:   bufio.NewReader(parentFile),
			Writer:   parentFile,
		}, nil
	}
	ClearCoordinatorClosers()

	go func() {
		childReader := bufio.NewReader(childFile)
		_ = writeHotswapMessage(childFile, HotswapMessage{Type: HotswapMsgReady, PID: 2, Version: "v3.23.0-fix.31.1"})
		_, _ = readHotswapMessage(childReader)
	}()

	if err := runHotSwapParentHandoff(context.Background(), func() {}, docopt.Opts{}); err != nil {
		t.Fatalf("runHotSwapParentHandoff: %v", err)
	}

	select {
	case got := <-execArgs:
		want := []string{"/app/urnetwork_amd64_stable", "provide", "--port", "8080"}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("execve args = %v, want %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for in-place execve call")
	}
	if len(spawnArgs) == 0 {
		t.Fatal("canary spawn was not called")
	}
}

// TestHotSwapParentPID1RestoresStdioBeforeExec: with RAMLOGS on, stdout and
// stderr are a pipe drained by a goroutine that exec discards. They must be
// restored before execve or the new image dies of SIGPIPE on its first write.
func TestHotSwapParentPID1RestoresStdioBeforeExec(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "parent")
	childFile := os.NewFile(uintptr(fds[1]), "child")
	defer parentFile.Close()
	defer childFile.Close()

	origGetpid, origExec, origSpawn, origRestore := getpidFunc, execInPlaceFunc, spawnCandidateFunc, restoreStdioBeforeExecFunc
	defer func() {
		getpidFunc, execInPlaceFunc, spawnCandidateFunc, restoreStdioBeforeExecFunc = origGetpid, origExec, origSpawn, origRestore
	}()
	getpidFunc = func() int { return 1 }

	var order []string
	done := make(chan struct{}, 1)
	restoreStdioBeforeExecFunc = func() { order = append(order, "restore") }
	execInPlaceFunc = func(exe string, args []string, env []string) error {
		order = append(order, "exec")
		done <- struct{}{}
		return nil
	}
	spawnCandidateFunc = func(exe string, args []string) (*HotswapParentSession, error) {
		return &HotswapParentSession{parentFd: parentFile, Reader: bufio.NewReader(parentFile), Writer: parentFile}, nil
	}
	ClearCoordinatorClosers()

	go func() {
		r := bufio.NewReader(childFile)
		_ = writeHotswapMessage(childFile, HotswapMessage{Type: HotswapMsgReady, PID: 2, Version: "v3.23.0-fix.31.1"})
		_, _ = readHotswapMessage(r)
	}()

	if err := runHotSwapParentHandoff(context.Background(), func() {}, docopt.Opts{}); err != nil {
		t.Fatalf("runHotSwapParentHandoff: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for execve")
	}
	if fmt.Sprint(order) != fmt.Sprint([]string{"restore", "exec"}) {
		t.Fatalf("order = %v, want stdio restored before exec", order)
	}
}
