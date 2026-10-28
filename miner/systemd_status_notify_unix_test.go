//go:build unix

package miner

// The line is only useful if it reaches the wire: these tests stand up a real
// NOTIFY_SOCKET and assert the sd_notify frame the publisher sends, rather than
// a stub of the send path.

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func notifyListener(t *testing.T) *net.UnixConn {
	t.Helper()
	sockPath := filepath.Join(shortStateDir(t), "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unixgram: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener
}

func readNotifyDatagram(t *testing.T, listener *net.UnixConn) string {
	t.Helper()
	if err := listener.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 256)
	n, err := listener.Read(buf)
	if err != nil {
		t.Fatalf("read notify socket: %v", err)
	}
	return string(buf[:n])
}

// TestReportProxyStatusToSystemdDeliversTheLine is the delivery half: the
// fabricated pool state must arrive as one STATUS= datagram carrying the
// relay's sentence.
func TestReportProxyStatusToSystemdDeliversTheLine(t *testing.T) {
	resetSystemdStatusState(t)
	listener := notifyListener(t)

	// Build the state with no notify socket, so the only datagram on the wire
	// is the one this test sends.
	t.Setenv("NOTIFY_SOCKET", "")
	setProxyStatusPool(fabricatedPool("a:1", "b:2", "c:3"))
	markProxyStatusLive("a:1")
	markProxyStatusLive("b:2")
	setProxyAuditSystemdState(1, false)

	t.Setenv("NOTIFY_SOCKET", listener.LocalAddr().String())
	reportProxyStatusToSystemd()

	// With one of the three parked, the two live proxies are the whole
	// effective set (2/2 = 100% of what should be live), so the word is active
	// while the rendered percentage is over the configured three. Parks are
	// deliberate, exactly as the relay treats them.
	want := "STATUS=active: 2/3 proxies authenticated (67%), 1 parked by proxy audit\n"
	if got := readNotifyDatagram(t, listener); got != want {
		t.Errorf("STATUS frame = %q, want %q", got, want)
	}
}

// TestRefreshProxySystemdStatusDeliversThePoolLine proves the periodic caller's
// entry point delivers on its own: it derives both counters from the published
// pool and the auth marks, then sends.
func TestRefreshProxySystemdStatusDeliversThePoolLine(t *testing.T) {
	resetSystemdStatusState(t)
	listener := notifyListener(t)

	t.Setenv("NOTIFY_SOCKET", "")
	setProxyStatusPool(fabricatedPool("a:1", "b:2", "c:3", "d:4", "e:5", "f:6", "g:7", "h:8"))
	for _, addr := range []string{"a:1", "b:2", "c:3", "d:4", "e:5", "f:6", "g:7", "h:8"} {
		markProxyStatusLive(addr)
	}

	t.Setenv("NOTIFY_SOCKET", listener.LocalAddr().String())
	refreshProxySystemdStatus()

	want := "STATUS=active: 8/8 proxies authenticated (100%)\n"
	if got := readNotifyDatagram(t, listener); got != want {
		t.Errorf("STATUS frame = %q, want %q", got, want)
	}
}

// TestReportProxyStatusToSystemdWithoutASocketIsSilent covers the fleet's
// Type=simple units and every non-systemd invocation: a missing socket is not
// an error and must not panic.
func TestReportProxyStatusToSystemdWithoutASocketIsSilent(t *testing.T) {
	resetSystemdStatusState(t)
	t.Setenv("NOTIFY_SOCKET", "")
	setProxyStatusPool(fabricatedPool("a:1"))
	reportProxyStatusToSystemd()
}
