//go:build linux

// The supervisor tests' shell children under a pinned supervisor stop. The
// barrier that holds a shell stopped is a waitid stop report, which is Linux
// only.
package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A supervisor stop lands while a shell child waits on its poll's sleep, and
// the shell reaps that sleep before it exits. In run 5 the burst cooldown
// child's shell died of the stop's TERM first, and the release gate's
// subreaper adopted its sleep. The sleep on the child's PATH here holds at a
// FIFO until the test has seen it start, and the child's group is stopped
// before the TERM, so neither process acts on it until both have it. The
// caller's private owner adopts anything the shell leaves behind.
func assertSupervisorStopJoinsPollSleep(t *testing.T, spec ProcessSpec) {
	t.Helper()
	dir := t.TempDir()
	barrierPath := filepath.Join(dir, "sleep.fifo")
	if err := syscall.Mkfifo(barrierPath, 0o600); err != nil {
		t.Fatal(err)
	}
	// The poll's sleep opens the barrier, then blocks reading it.
	sleepDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(sleepDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sleepDir, "sleep"), []byte("#!/bin/sh\nexec /bin/cat \"$SIM_TEST_SLEEP_BARRIER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for key, value := range spec.Env {
		env[key] = value
	}
	env["PATH"] = sleepDir + ":" + os.Getenv("PATH")
	env["SIM_TEST_SLEEP_BARRIER"] = barrierPath
	spec.Env = env
	command, exited, err := startSpecWithExit(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	joined := false
	t.Cleanup(func() {
		if !joined {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			<-exited
		}
	})
	identity, err := observeStartedSupervisedProcessIdentity(context.Background(), command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	// Opening the barrier to write returns once the sleep has opened it to
	// read, so from here the shell waits on a live sleep.
	type openedBarrier struct {
		file *os.File
		err  error
	}
	opened := make(chan openedBarrier, 1)
	go func() {
		file, err := os.OpenFile(barrierPath, os.O_WRONLY, 0)
		opened <- openedBarrier{file: file, err: err}
	}()
	select {
	case barrier := <-opened:
		if barrier.err != nil {
			t.Fatal(barrier.err)
		}
		defer barrier.file.Close()
	case err := <-exited:
		t.Fatalf("%s exited before its poll's sleep started: %v", spec.ID, err)
	}
	// Stop the group, and wait until the shell's stop is reported, so the
	// supervisor's TERM is pending in both processes before either can run.
	if err := syscall.Kill(-identity.ProcessGroupID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	var stopped unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, command.Process.Pid, &stopped, unix.WSTOPPED|unix.WNOWAIT, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		break
	}
	if !signalSupervisedCommandWithObserver(supervisedCommand{spec: spec, cmd: command, identity: identity}, syscall.SIGTERM, observeSupervisedProcessIdentity, syscall.Kill) {
		t.Fatalf("the supervisor's stop did not signal the group of %s", spec.ID)
	}
	if err := syscall.Kill(-identity.ProcessGroupID, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	// The trap exits 143 once the shell has reaped the sleep. A shell that
	// dies of the TERM instead leaves the sleep to the private owner.
	err = <-exited
	joined = true
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		t.Fatalf("stopped %s exit = %v, want 143 from its TERM trap", spec.ID, err)
	}
}

// The burst cooldown test stops its third attempt while it polls.
func TestSupervisorStoppedCooldownChildJoinsItsPollHelper(t *testing.T) {
	runSupervisorDescendantOwnerTest(t, 0, 25*time.Second, func(t *testing.T) {
		dir := t.TempDir()
		counterPath := filepath.Join(dir, "child.counter")
		// Two earlier attempts make this the third.
		if err := os.WriteFile(counterPath, []byte("2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// No state is published, so the attempt polls until it is stopped.
		assertSupervisorStopJoinsPollSleep(t, supervisorCooldownTestChild(dir, counterPath, filepath.Join(dir, "supervisor.state.json")))
	})
}

// The failed cooldown test's child polls the same way on every attempt.
func TestSupervisorStoppedFailedCooldownChildJoinsItsPollHelper(t *testing.T) {
	runSupervisorDescendantOwnerTest(t, 0, 25*time.Second, func(t *testing.T) {
		dir := t.TempDir()
		assertSupervisorStopJoinsPollSleep(t, supervisorFailedCooldownTestChild(t, dir, filepath.Join(dir, "child.counter"), filepath.Join(dir, "supervisor.state.json")))
	})
}

// The restart test's first attempt polls until its state is published.
func TestSupervisorStoppedRestartChildJoinsItsPollHelper(t *testing.T) {
	runSupervisorDescendantOwnerTest(t, 0, 25*time.Second, func(t *testing.T) {
		dir := t.TempDir()
		assertSupervisorStopJoinsPollSleep(t, supervisorRestartTestChild(dir, filepath.Join(dir, "child.counter"), filepath.Join(dir, "child.ready"), filepath.Join(dir, "supervisor.state.json"), ""))
	})
}
