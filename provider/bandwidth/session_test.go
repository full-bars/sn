package bandwidth

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
)

// egressSettings is a relay-egress ConnectSettings whose base dial returns
// whatever newConn makes, wrapped the way provideWithProxy wraps it.
func egressSettings(bw *ProxyBandwidth, newConn func() net.Conn) connect.ConnectSettings {
	cs := connect.ConnectSettings{
		DialContextSettings: &connect.DialContextSettings{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return newConn(), nil
			},
		},
	}
	return WrapConnectSettings(cs, bw, "proxy-s")
}

func dialEgress(t *testing.T, cs connect.ConnectSettings) net.Conn {
	t.Helper()
	conn, err := cs.DialContextSettings.DialContext(context.Background(), "tcp", "1.2.3.4:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

// The relay-egress dial is where client traffic leaves the provider; each
// live connection must count as a client session, and closing it (any number
// of times) must end it exactly once.
func TestWrapConnectSettings_CountsLiveClientSessions(t *testing.T) {
	bw := &ProxyBandwidth{}
	cs := egressSettings(bw, func() net.Conn { return newMockConn() })

	a := dialEgress(t, cs)
	b := dialEgress(t, cs)
	if got := bw.Clients.Load(); got != 2 {
		t.Fatalf("Clients with two live egress connections = %d, want 2", got)
	}
	bw.mu.Lock()
	live := len(bw.sessions)
	bw.mu.Unlock()
	if live != 2 {
		t.Fatalf("tracked sessions = %d, want 2", live)
	}

	a.Close()
	a.Close()
	if got := bw.Clients.Load(); got != 1 {
		t.Fatalf("Clients after closing one connection twice = %d, want 1", got)
	}
	b.Close()
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("Clients after closing both = %d, want 0", got)
	}
	bw.mu.Lock()
	n := len(bw.sessions)
	bw.mu.Unlock()
	if n != 0 {
		t.Fatalf("sessions left after close = %d, want 0", n)
	}
}

func TestNewSessionConn_WrappedTwiceCountsOnce(t *testing.T) {
	bw := &ProxyBandwidth{}

	c := NewSessionConn(NewSessionConn(newMockConn(), bw, "p"), bw, "p")
	if got := bw.Clients.Load(); got != 1 {
		t.Fatalf("session wrapped twice: Clients = %d, want 1", got)
	}
	c.Close()
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("after close: Clients = %d, want 0", got)
	}

	// A byte-only wrapper underneath, as a pre-wrapped dialer would produce.
	d := NewSessionConn(NewConn(newMockConn(), bw, "p"), bw, "p")
	d.Write([]byte("abcd"))
	if got := bw.Clients.Load(); got != 1 {
		t.Fatalf("session over byte wrapper: Clients = %d, want 1", got)
	}
	if got := bw.TotalTx.Load(); got != 4 {
		t.Fatalf("bytes counted twice through a double wrap: TotalTx = %d, want 4", got)
	}
	d.Close()
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("after close: Clients = %d, want 0", got)
	}
}

// The provider's own control-plane connections use NewConn and are not
// client sessions.
func TestNewConn_NotAClientSession(t *testing.T) {
	bw := &ProxyBandwidth{}
	c := NewConn(newMockConn(), bw, "p")
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("control-plane conn counted as a client: %d", got)
	}
	c.Close()
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("control-plane close went negative: %d", got)
	}
}

type errCloseConn struct {
	*mockConn
	panics bool
}

func (c *errCloseConn) Close() error {
	if c.panics {
		panic("close panicked")
	}
	return errors.New("close failed")
}

// The session ends even when the underlying Close errors or panics.
func TestNewSessionConn_ReleasedOnCloseErrorAndPanic(t *testing.T) {
	bw := &ProxyBandwidth{}

	e := NewSessionConn(&errCloseConn{mockConn: newMockConn()}, bw, "p")
	if err := e.Close(); err == nil {
		t.Fatal("expected the inner Close error to be returned")
	}
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("after erroring close: Clients = %d, want 0", got)
	}

	p := NewSessionConn(&errCloseConn{mockConn: newMockConn(), panics: true}, bw, "p")
	func() {
		defer func() { recover() }()
		p.Close()
	}()
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("after panicking close: Clients = %d, want 0", got)
	}
}

// Run with -race: concurrent dials and racing double closes must leave the
// counter at exactly zero.
func TestNewSessionConn_ConcurrentDialClose(t *testing.T) {
	bw := &ProxyBandwidth{}
	cs := egressSettings(bw, func() net.Conn { return newMockConn() })

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := cs.DialContextSettings.DialContext(context.Background(), "tcp", "1.2.3.4:443")
			if err != nil {
				t.Error(err)
				return
			}
			var cwg sync.WaitGroup
			for j := 0; j < 3; j++ {
				cwg.Add(1)
				go func() {
					defer cwg.Done()
					conn.Close()
				}()
			}
			cwg.Wait()
		}()
	}
	wg.Wait()
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("Clients after concurrent dial/close = %d, want 0", got)
	}
}
