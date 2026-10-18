//go:build unix

package miner

// hotswap_unix_test.go covers the Unix half of the hotswap port: candidate IPC
// descriptor validation, the systemd notify protocol (READY, STATUS, and the
// MAINPID handoff), the executable check's self-healing path, and the control
// socket release wait.

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// mustSocketpair returns a connected pair of unix stream sockets.
func mustSocketpair(t *testing.T) [2]int {
	t.Helper()
	fds, err := hotSwapSocketpair()
	if err != nil {
		t.Fatalf("hotSwapSocketpair: %v", err)
	}
	t.Cleanup(func() {
		syscall.Close(fds[0])
		syscall.Close(fds[1])
	})
	return fds
}

func TestHotSwapSocketpairIsCloseOnExec(t *testing.T) {
	fds := mustSocketpair(t)
	for _, fd := range fds {
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if errno != 0 {
			t.Fatalf("fcntl(F_GETFD, %d): %v", fd, errno)
		}
		if flags&syscall.FD_CLOEXEC == 0 {
			t.Fatalf("descriptor %d is not close-on-exec", fd)
		}
	}
}

func TestGetHotSwapChildIPCRequiresTheCandidateEnv(t *testing.T) {
	fds := mustSocketpair(t)
	t.Setenv(EnvHotSwap, "0")
	if _, isChild := getHotSwapChildIPCFromFd(fds[0]); isChild {
		t.Fatal("claimed candidate mode without URNETWORK_HOTSWAP=1")
	}
}

func TestGetHotSwapChildIPCRejectsANonSocketDescriptor(t *testing.T) {
	readFile, writeFile, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readFile.Close()
	defer writeFile.Close()

	t.Setenv(EnvHotSwap, "1")
	// A candidate that inherits the env marker but not the socket (a slow
	// parent that closed it, or a fork that dropped fd 3) must start as a
	// normal provider rather than hang on a dead descriptor.
	if _, isChild := getHotSwapChildIPCFromFd(int(readFile.Fd())); isChild {
		t.Fatal("claimed candidate mode for a pipe descriptor")
	}
	if _, isChild := getHotSwapChildIPCFromFd(-1); isChild {
		t.Fatal("claimed candidate mode for an invalid descriptor")
	}
}

func TestGetHotSwapChildIPCClaimsASocketDescriptor(t *testing.T) {
	fds := mustSocketpair(t)
	t.Setenv(EnvHotSwap, "1")
	ipc, isChild := getHotSwapChildIPCFromFd(fds[0])
	if !isChild {
		t.Fatal("did not claim candidate mode for a socket descriptor")
	}
	ipc.Close()
}

func TestNotifySystemdProtocol(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	orig := os.Getenv("NOTIFY_SOCKET")
	t.Cleanup(func() { os.Setenv("NOTIFY_SOCKET", orig) })
	os.Setenv("NOTIFY_SOCKET", sockPath)

	read := func() string {
		t.Helper()
		buf := make([]byte, 128)
		n, err := listener.Read(buf)
		if err != nil {
			t.Fatalf("read notify socket: %v", err)
		}
		return string(buf[:n])
	}

	if err := notifySystemdReady(); err != nil {
		t.Fatalf("notifySystemdReady: %v", err)
	}
	if got := read(); got != "READY=1\n" {
		t.Fatalf("READY payload = %q", got)
	}
	if err := notifySystemdMainPID(45678); err != nil {
		t.Fatalf("notifySystemdMainPID: %v", err)
	}
	if got := read(); got != "MAINPID=45678\n" {
		t.Fatalf("MAINPID payload = %q", got)
	}
	if err := notifySystemdStatus("proxy 0 up"); err != nil {
		t.Fatalf("notifySystemdStatus: %v", err)
	}
	if got := read(); got != "STATUS=proxy 0 up\n" {
		t.Fatalf("STATUS payload = %q", got)
	}
}

func TestNotifySystemdWithoutASocketIsNotAnError(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := notifySystemdReady(); err != nil {
		t.Fatalf("notifySystemdReady without NOTIFY_SOCKET: %v", err)
	}
	if err := notifySystemdStatus("x"); err != nil {
		t.Fatalf("notifySystemdStatus without NOTIFY_SOCKET: %v", err)
	}
	// MAINPID is the one caller that must be able to tell the two apart.
	if err := notifySystemdMainPID(1); err != ErrNoNotifySocket {
		t.Fatalf("notifySystemdMainPID error = %v, want ErrNoNotifySocket", err)
	}
}

func TestCheckExecutableSelfHealsAMissingExecBit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can execute a file without an execute bit")
	}
	path := filepath.Join(t.TempDir(), "urnet-miner")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The updater writes the new build before chmod; the handoff heals the
	// bit rather than aborting and permanently blocking the update.
	if err := checkExecutable(path); err != nil {
		t.Fatalf("checkExecutable: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("execute bit not restored: %v", info.Mode().Perm())
	}
}

func TestCheckExecutableRejectsADirectory(t *testing.T) {
	if err := checkExecutable(t.TempDir()); err == nil {
		t.Fatal("expected an error for a directory")
	}
	if err := checkExecutable(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected an error for a missing path")
	}
}

func TestWaitForControlSocketReleaseReturnsWithoutAListener(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())

	done := make(chan struct{})
	go func() {
		defer close(done)
		waitForControlSocketRelease(time.Minute)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not return when nothing holds the socket path")
	}
}

func TestExecInPlaceUsesTheSuppliedImage(t *testing.T) {
	// execInPlace only ever returns in the failure case; a nonexistent path
	// must come back as an error rather than replacing the test process.
	err := execInPlace(filepath.Join(t.TempDir(), "no-such-binary"), []string{"no-such-binary"}, os.Environ())
	if err == nil {
		t.Fatal("expected syscall.Exec to fail for a missing binary")
	}
}

// TestHotSwapTakeoverCandidateAcksAndTakesOver drives the successor side of a
// completed handoff directly: the IPC is a real socketpair (so the ACK is read
// back from the parent's end), the control socket must be bound by this process
// once the parent's is gone, and the handoff trigger must move to the new
// process.
func TestHotSwapTakeoverCandidateAcksAndTakesOver(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	ResetHotSwapStateForTest()
	t.Cleanup(func() {
		ResetHotSwapStateForTest()
		setHotSwapTrigger(nil)
	})

	fds := mustSocketpair(t)
	parentEnd := os.NewFile(uintptr(fds[0]), "test-hotswap-parent")
	childEnd := os.NewFile(uintptr(fds[1]), "test-hotswap-child")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state, err := loadControlState()
	if err != nil {
		t.Fatalf("loadControlState: %v", err)
	}
	globalControlState = state
	owner := &hotSwapControlSocketOwner{ctx: ctx, state: state}
	defer owner.finish()

	settings := providerRunSettings{hotSwapIPC: childEnd, hotSwapCandidate: true}
	settings.hotSwapTakeoverCandidate(ctx, cancel, owner)

	ackCh := make(chan HotswapMessage, 1)
	go func() {
		msg, err := readHotswapMessage(bufio.NewReader(parentEnd))
		if err == nil {
			ackCh <- msg
		}
	}()
	select {
	case ack := <-ackCh:
		if ack.Type != HotswapMsgAck {
			t.Fatalf("candidate sent %q, want ACK", ack.Type)
		}
		if ack.PID != os.Getpid() {
			t.Fatalf("ACK pid = %d, want %d", ack.PID, os.Getpid())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("candidate never ACKed the parent")
	}

	// The parent quiesced its socket at the commit point, so the successor
	// must now own the path.
	path, err := controlSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("successor did not bind the control socket: %v", err)
	}
	conn.Close()

	if getHotSwapTrigger() == nil {
		t.Fatal("successor did not take over the hotswap trigger")
	}
}
