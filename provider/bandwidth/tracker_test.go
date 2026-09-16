package bandwidth

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"
)

// mockConn is a minimal net.Conn for testing.
type mockConn struct {
	net.Conn
	readBuf  *bytes.Buffer
	writeBuf *bytes.Buffer
}

func newMockConn() *mockConn {
	return &mockConn{
		readBuf:  &bytes.Buffer{},
		writeBuf: &bytes.Buffer{},
	}
}

func (m *mockConn) Read(b []byte) (int, error) {
	return m.readBuf.Read(b)
}

func (m *mockConn) Write(b []byte) (int, error) {
	return m.writeBuf.Write(b)
}

func (m *mockConn) Close() error                        { return nil }
func (m *mockConn) LocalAddr() net.Addr                 { return &net.TCPAddr{} }
func (m *mockConn) RemoteAddr() net.Addr                { return &net.TCPAddr{} }
func (m *mockConn) SetDeadline(_ time.Time) error       { return nil }
func (m *mockConn) SetReadDeadline(_ time.Time) error   { return nil }
func (m *mockConn) SetWriteDeadline(_ time.Time) error  { return nil }

// mockPacketConn is a minimal net.PacketConn for testing.
type mockPacketConn struct {
	net.PacketConn
	readBuf  *bytes.Buffer
	writeBuf *bytes.Buffer
}

func newMockPacketConn() *mockPacketConn {
	return &mockPacketConn{
		readBuf:  &bytes.Buffer{},
		writeBuf: &bytes.Buffer{},
	}
}

func (m *mockPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := m.readBuf.Read(p)
	return n, &net.UDPAddr{}, err
}

func (m *mockPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	return m.writeBuf.Write(p)
}

func (m *mockPacketConn) Close() error                       { return nil }
func (m *mockPacketConn) LocalAddr() net.Addr                { return &net.UDPAddr{} }
func (m *mockPacketConn) SetDeadline(_ time.Time) error      { return nil }
func (m *mockPacketConn) SetReadDeadline(_ time.Time) error  { return nil }
func (m *mockPacketConn) SetWriteDeadline(_ time.Time) error { return nil }

func TestConnReadCountsBytes(t *testing.T) {
	mock := newMockConn()
	mock.readBuf.WriteString("hello world")

	bw := &ProxyBandwidth{}
	conn := NewConn(mock, bw, "proxy-1")

	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != 11 {
		t.Fatalf("expected 11 bytes read, got %d", n)
	}
	if bw.TotalRx.Load() != 11 {
		t.Fatalf("TotalRx: expected 11, got %d", bw.TotalRx.Load())
	}
}

func TestConnWriteCountsBytes(t *testing.T) {
	mock := newMockConn()
	bw := &ProxyBandwidth{}
	conn := NewConn(mock, bw, "proxy-1")

	data := []byte("test data here")
	n, err := conn.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected %d bytes written, got %d", len(data), n)
	}
	if bw.TotalTx.Load() != uint64(len(data)) {
		t.Fatalf("TotalTx: expected %d, got %d", len(data), bw.TotalTx.Load())
	}
}

func TestPacketConnReadFromCountsBytes(t *testing.T) {
	mock := newMockPacketConn()
	mock.readBuf.Write([]byte("quic payload"))

	bw := &ProxyBandwidth{}
	pc := NewPacketConn(mock, bw, "proxy-2")

	buf := make([]byte, 1024)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if n != 12 {
		t.Fatalf("expected 12 bytes, got %d", n)
	}
	if bw.TotalRx.Load() != 12 {
		t.Fatalf("TotalRx: expected 12, got %d", bw.TotalRx.Load())
	}
}

func TestPacketConnWriteToCountsBytes(t *testing.T) {
	mock := newMockPacketConn()
	bw := &ProxyBandwidth{}
	pc := NewPacketConn(mock, bw, "proxy-2")

	data := []byte("quic outbound")
	n, err := pc.WriteTo(data, &net.UDPAddr{})
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected %d, got %d", len(data), n)
	}
	if bw.TotalTx.Load() != uint64(len(data)) {
		t.Fatalf("TotalTx: expected %d, got %d", len(data), bw.TotalTx.Load())
	}
}

func TestRegistryRegisterCreatesOnce(t *testing.T) {
	reg := NewRegistry()
	bw1 := reg.Register(0)
	bw2 := reg.Register(0)
	if bw1 != bw2 {
		t.Fatal("expected same pointer for same index")
	}
	bw3 := reg.Register(1)
	if bw1 == bw3 {
		t.Fatal("expected different pointer for different index")
	}
}

func TestRegistryConcurrentRegister(t *testing.T) {
	reg := NewRegistry()
	var wg sync.WaitGroup
	results := make([]*ProxyBandwidth, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = reg.Register(idx % 5)
		}(i)
	}
	wg.Wait()
	// All goroutines using the same index should get the same pointer.
	for i := 0; i < 5; i++ {
		count := 0
		for j := 0; j < 100; j++ {
			if results[j] == reg.Register(i) {
				count++
			}
		}
		if count != 20 {
			t.Fatalf("index %d: expected 20 matches, got %d", i, count)
		}
	}
}

func TestProxyBandwidthSessionTracking(t *testing.T) {
	bw := &ProxyBandwidth{}
	start := time.Now()
	bw.AddSession("session-1", start)
	bw.AddSession("session-2", start)

	age := bw.MaxAge()
	if age < 0 || age > time.Second {
		t.Fatalf("unexpected age: %v", age)
	}

	bw.RemoveSession("session-1")
	bw.RemoveSession("session-2")

	// After removing all sessions, age should still reflect presenceSince.
	age = bw.MaxAge()
	if age < 0 {
		t.Fatalf("unexpected negative age: %v", age)
	}
}

func TestRegistrySnapshot(t *testing.T) {
	reg := NewRegistry()
	bw0 := reg.Register(0)
	bw0.TotalRx.Add(100)
	bw1 := reg.Register(1)
	bw1.TotalTx.Add(200)

	snap := reg.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(snap))
	}
	if snap[0].TotalRx.Load() != 100 {
		t.Fatalf("snapshot[0].TotalRx: expected 100, got %d", snap[0].TotalRx.Load())
	}
	if snap[1].TotalTx.Load() != 200 {
		t.Fatalf("snapshot[1].TotalTx: expected 200, got %d", snap[1].TotalTx.Load())
	}
}
