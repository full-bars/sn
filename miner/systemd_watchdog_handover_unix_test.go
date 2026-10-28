//go:build !windows

package miner

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// listenNotify stands up the unixgram socket systemd would own, so a ping is
// observed at the wire rather than at a counter.
func listenNotify(t *testing.T) *net.UnixConn {
	t.Helper()
	sock := filepath.Join(shortStateDir(t), "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	t.Setenv("NOTIFY_SOCKET", sock)
	return listener
}

func readPing(t *testing.T, listener *net.UnixConn, within time.Duration) bool {
	t.Helper()
	buf := make([]byte, 64)
	_ = listener.SetReadDeadline(time.Now().Add(within))
	if _, _, err := listener.ReadFromUnix(buf); err != nil {
		return false
	}
	return true
}

// TestSystemdWatchdogHandoverStopsTheStartupPinger: the ungated startup pinger
// must stop once the gated loop takes over. If it keeps running, the gate can
// never withhold a ping and a stalled provider is never restarted by systemd.
func TestSystemdWatchdogHandoverStopsTheStartupPinger(t *testing.T) {
	listener := listenNotify(t)
	t.Setenv("WATCHDOG_USEC", "300000") // interval = 100ms
	t.Cleanup(stopSystemdWatchdog)

	startSystemdWatchdog()
	if !sdWatchdogUngated.Load() {
		t.Fatal("startup pinger did not start")
	}
	if !readPing(t, listener, 2*time.Second) {
		t.Fatal("startup pinger sent no ping")
	}

	stopSystemdWatchdog()
	if sdWatchdogUngated.Load() {
		t.Fatal("startup pinger still marked running after stopSystemdWatchdog")
	}

	// Drain anything already in flight, then require silence for several
	// intervals. Without the stop, this is where the old ticker would show up.
	time.Sleep(250 * time.Millisecond)
	for readPing(t, listener, 20*time.Millisecond) {
	}
	if readPing(t, listener, 350*time.Millisecond) {
		t.Fatal("a ping arrived after the startup pinger was stopped")
	}

	// The socket and the notifier themselves are still fine: the gated loop
	// owns the pings now and must be able to send them.
	if err := notifySystemdWatchdog(); err != nil {
		t.Fatalf("gated ping failed after handover: %v", err)
	}
	if !readPing(t, listener, time.Second) {
		t.Fatal("gated ping never arrived")
	}
}

// TestRunSdWatchdogTakesOverFromTheStartupPinger: runSdWatchdog performs the
// handover itself, so callers do not have to remember to.
func TestRunSdWatchdogTakesOverFromTheStartupPinger(t *testing.T) {
	listener := listenNotify(t)
	t.Setenv("WATCHDOG_USEC", "300000")
	t.Cleanup(stopSystemdWatchdog)

	startSystemdWatchdog()
	if !readPing(t, listener, 2*time.Second) {
		t.Fatal("startup pinger sent no ping")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runSdWatchdog(ctx, func(bool) (bool, string) { return true, "test" })

	deadline := time.Now().Add(2 * time.Second)
	for sdWatchdogUngated.Load() {
		if time.Now().After(deadline) {
			t.Fatal("runSdWatchdog never took the watchdog over from the startup pinger")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
