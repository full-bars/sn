package bandwidth

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// startEchoServer accepts connections and echoes every byte back.
func startEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// startSocks5Server is a minimal no-auth SOCKS5 CONNECT proxy. It counts the
// connections it carries, which is the proof that a dial went through it.
func startSocks5Server(t *testing.T) (addr string, carried *atomic.Int64) {
	t.Helper()
	carried = &atomic.Int64{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				head := make([]byte, 2)
				if _, err := io.ReadFull(c, head); err != nil || head[0] != 5 {
					return
				}
				io.CopyN(io.Discard, c, int64(head[1]))
				c.Write([]byte{5, 0})
				req := make([]byte, 4)
				if _, err := io.ReadFull(c, req); err != nil || req[1] != 1 {
					return
				}
				var host string
				switch req[3] {
				case 1:
					ip := make([]byte, 4)
					io.ReadFull(c, ip)
					host = net.IP(ip).String()
				case 3:
					l := make([]byte, 1)
					io.ReadFull(c, l)
					name := make([]byte, l[0])
					io.ReadFull(c, name)
					host = string(name)
				default:
					return
				}
				portBytes := make([]byte, 2)
				io.ReadFull(c, portBytes)
				port := binary.BigEndian.Uint16(portBytes)
				up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
				if err != nil {
					c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				carried.Add(1)
				go io.Copy(up, c)
				io.Copy(c, up)
			}()
		}
	}()
	return ln.Addr().String(), carried
}

// The provider's own connections to the platform go through this wrapper. It
// must keep routing them through the proxy, count their bytes into the total
// only, and never register a client session or a billable byte.
func TestWrapConnectSettingsTotalCountsControlPlaneThroughTheProxy(t *testing.T) {
	echo := startEchoServer(t)
	socks, carried := startSocks5Server(t)

	cs := connect.ConnectSettings{
		ProxySettings: &connect.ProxySettings{Network: "tcp", Address: socks},
	}
	bw := &ProxyBandwidth{}
	wrapped := WrapConnectSettingsTotal(cs, bw, "proxy-1")
	if wrapped.DialContextSettings == nil {
		t.Fatal("a proxied identity must get a counting dial")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := wrapped.DialContext(ctx, "tcp", echo)
	if err != nil {
		t.Fatalf("dial through the wrapped settings: %v", err)
	}
	defer conn.Close()

	payload := []byte("control plane bytes")
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo = %q, want %q", got, payload)
	}

	if carried.Load() != 1 {
		t.Fatalf("the proxy carried %d connections, want 1 (the dial must go through it)", carried.Load())
	}
	if bw.TotalTx.Load() != uint64(len(payload)) || bw.TotalRx.Load() != uint64(len(payload)) {
		t.Fatalf("total tx=%d rx=%d, want %d each", bw.TotalTx.Load(), bw.TotalRx.Load(), len(payload))
	}
	if bw.BillableTx.Load() != 0 || bw.BillableRx.Load() != 0 {
		t.Fatalf("control-plane bytes were counted as billable: tx=%d rx=%d", bw.BillableTx.Load(), bw.BillableRx.Load())
	}
	if bw.Clients.Load() != 0 {
		t.Fatalf("the provider's own connection was counted as a client session (%d)", bw.Clients.Load())
	}
}

// The relay wrapper still counts a client session, so the two wrappers differ
// only in that.
func TestWrapConnectSettingsRelayStillCountsAClientSession(t *testing.T) {
	echo := startEchoServer(t)
	socks, _ := startSocks5Server(t)
	cs := connect.ConnectSettings{
		ProxySettings: &connect.ProxySettings{Network: "tcp", Address: socks},
	}
	bw := &ProxyBandwidth{}
	wrapped := WrapConnectSettings(cs, bw, "proxy-1")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := wrapped.DialContext(ctx, "tcp", echo)
	if err != nil {
		t.Fatal(err)
	}
	if bw.Clients.Load() != 1 {
		t.Fatalf("clients = %d, want 1 while the relay connection is open", bw.Clients.Load())
	}
	conn.Close()
	if bw.Clients.Load() != 0 {
		t.Fatalf("clients = %d after close, want 0", bw.Clients.Load())
	}
}

// Wrapping the control plane after the relay copy must not count relay bytes
// twice: the relay copy is built from the unwrapped settings.
func TestRelayThenTotalWrapDoesNotDoubleCountRelayBytes(t *testing.T) {
	echo := startEchoServer(t)
	socks, _ := startSocks5Server(t)
	base := connect.ConnectSettings{
		ProxySettings: &connect.ProxySettings{Network: "tcp", Address: socks},
	}
	bw := &ProxyBandwidth{}
	relay := WrapConnectSettings(base, bw, "proxy-1")
	_ = WrapConnectSettingsTotal(base, bw, "proxy-1")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := relay.DialContext(ctx, "tcp", echo)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("abcd"))
	io.ReadFull(conn, make([]byte, 4))
	if bw.TotalTx.Load() != 4 || bw.TotalRx.Load() != 4 {
		t.Fatalf("relay bytes counted tx=%d rx=%d, want 4 each (once)", bw.TotalTx.Load(), bw.TotalRx.Load())
	}
}

// A direct identity keeps the engine's own dial, which races address families.
func TestWrapConnectSettingsTotalLeavesADirectIdentityAlone(t *testing.T) {
	cs := connect.ConnectSettings{}
	wrapped := WrapConnectSettingsTotal(cs, &ProxyBandwidth{}, "direct")
	if wrapped.DialContextSettings != nil {
		t.Fatal("a direct identity must keep the engine's dial path")
	}
	// nil bandwidth is a no-op too
	proxied := connect.ConnectSettings{ProxySettings: &connect.ProxySettings{Network: "tcp", Address: "127.0.0.1:1"}}
	if got := WrapConnectSettingsTotal(proxied, nil, "p"); got.DialContextSettings != nil {
		t.Fatal("a nil tracker must leave the settings unchanged")
	}
}
