package miner

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnnounceStartupCompleteSendsReady(t *testing.T) {
	sockPath := filepath.Join(shortStateDir(t), "n.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Skipf("unixgram sockets unavailable: %v", err)
	}
	defer listener.Close()
	t.Setenv("NOTIFY_SOCKET", sockPath)

	announceStartupComplete()

	buf := make([]byte, 64)
	n, err := listener.Read(buf)
	if err != nil {
		t.Fatalf("read notify socket: %v", err)
	}
	if got := string(buf[:n]); got != "READY=1\n" {
		t.Fatalf("payload = %q, want READY=1", got)
	}
}

func TestAnnounceStartupCompleteWithoutSocketDoesNotPanic(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	announceStartupComplete()
}

// The regression this guards is a sender with no caller: notifySystemdReady
// existed and was tested while nothing in the run ever called it, so a
// Type=notify unit sat in "activating" and systemd killed the miner every
// TimeoutStartSec. run is too large to execute in a test, so assert the
// structure instead: the barrier is sent for a non-candidate before the
// watchdog starts, and a candidate signals after its takeover.
func TestRunSignalsReadyBeforeTheProxyLaunch(t *testing.T) {
	src, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	call := strings.Index(text, "announceStartupComplete()")
	watchdog := strings.Index(text, "startLivenessWatchdog(ctx)")
	launch := strings.Index(text, "providerAuthenticationCallbacks{")
	if call < 0 || watchdog < 0 || launch < 0 {
		t.Fatalf("run.go lost an anchor: ready=%d watchdog=%d launch=%d", call, watchdog, launch)
	}
	if !(call < watchdog && watchdog < launch) {
		t.Fatalf("READY must precede the watchdog and the proxy launch: ready=%d watchdog=%d launch=%d", call, watchdog, launch)
	}
	if !strings.Contains(text[max(0, call-120):call], "!self.hotSwapCandidate") {
		t.Fatal("a HotSwap candidate must not signal READY before it owns the unit")
	}
	hs, err := os.ReadFile("hotswap.go")
	if err != nil {
		t.Fatal(err)
	}
	takeover := strings.Index(string(hs), "func (self providerRunSettings) hotSwapTakeoverCandidate")
	if takeover < 0 || !strings.Contains(string(hs)[takeover:], "announceStartupComplete()") {
		t.Fatal("the takeover continuation must signal READY")
	}
}
