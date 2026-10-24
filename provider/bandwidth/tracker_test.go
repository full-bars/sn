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

func (m *mockConn) Close() error                       { return nil }
func (m *mockConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (m *mockConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (m *mockConn) SetDeadline(_ time.Time) error      { return nil }
func (m *mockConn) SetReadDeadline(_ time.Time) error  { return nil }
func (m *mockConn) SetWriteDeadline(_ time.Time) error { return nil }

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

// sizedPacketConn is a net.PacketConn that also exposes the socket buffer
// setters, the way the conns this wrapper is put around do: *net.UDPConn, and
// the proxy's relay (provider/socks5_udp.go, socks5PacketConn).
type sizedPacketConn struct {
	net.PacketConn
	readBuffer  int
	writeBuffer int
}

func (c *sizedPacketConn) SetReadBuffer(bytes int) error {
	c.readBuffer = bytes
	return nil
}

func (c *sizedPacketConn) SetWriteBuffer(bytes int) error {
	c.writeBuffer = bytes
	return nil
}

// The counting wrapper must not hide the socket buffer sizing. quic-go asks for
// larger buffers by type-asserting these methods and silently skips a conn that
// does not expose them, and connect's own wrappers forward them for the same
// reason (net_extender_memory.go, platform_packet_conn.go). Embedding
// net.PacketConn hides them, so a proxied slot's H3 endpoint — the proxy's
// relay, wrapped here — would keep the OS default buffers while the direct
// path, which is not wrapped, gets the size the caller asked for.
func TestPacketConnForwardsSocketBuffers(t *testing.T) {
	sized := &sizedPacketConn{PacketConn: newMockPacketConn()}
	bw := &ProxyBandwidth{}
	pc := NewPacketConn(sized, bw, "proxy-2")

	var _ interface{ SetReadBuffer(int) error } = pc
	var _ interface{ SetWriteBuffer(int) error } = pc

	if err := pc.SetReadBuffer(4 << 20); err != nil {
		t.Fatalf("SetReadBuffer: %v", err)
	}
	if err := pc.SetWriteBuffer(2 << 20); err != nil {
		t.Fatalf("SetWriteBuffer: %v", err)
	}
	if sized.readBuffer != 4<<20 {
		t.Fatalf("read buffer = %d, want %d: the request did not reach the socket", sized.readBuffer, 4<<20)
	}
	if sized.writeBuffer != 2<<20 {
		t.Fatalf("write buffer = %d, want %d: the request did not reach the socket", sized.writeBuffer, 2<<20)
	}

	// Sizing the socket must not disturb the accounting.
	if _, err := pc.WriteTo([]byte("after sizing"), &net.UDPAddr{}); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if got := bw.TotalTx.Load(); got != uint64(len("after sizing")) {
		t.Fatalf("TotalTx = %d, want %d", got, len("after sizing"))
	}
}

// A conn with no socket of its own to size is not an error: the caller asked
// for larger buffers and QUIC works with the defaults. The alternative — an
// error — would make quic-go treat a working endpoint as unusable.
func TestPacketConnBufferForwardingWithoutSupportIsANoOp(t *testing.T) {
	bw := &ProxyBandwidth{}
	pc := NewPacketConn(newMockPacketConn(), bw, "proxy-2")

	if err := pc.SetReadBuffer(4 << 20); err != nil {
		t.Fatalf("SetReadBuffer on an unsizable conn: %v", err)
	}
	if err := pc.SetWriteBuffer(4 << 20); err != nil {
		t.Fatalf("SetWriteBuffer on an unsizable conn: %v", err)
	}
	if _, err := pc.WriteTo([]byte("still counted"), &net.UDPAddr{}); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if got := bw.TotalTx.Load(); got != uint64(len("still counted")) {
		t.Fatalf("TotalTx = %d, want %d", got, len("still counted"))
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

// SetSeed preloads a newly created entry with the totals a previous process
// recorded, and leaves entries that already existed untouched: the miner arms
// it before registration begins, and a live entry must never be rewritten.
func TestProxyRegistrySetSeedAppliesToNewEntries(t *testing.T) {
	registry := NewRegistry()
	existing := registry.Register(1)
	existing.TotalRx.Add(5)

	registry.SetSeed(func(index int) (uint64, uint64, uint64, uint64) {
		if index != 2 {
			t.Fatalf("seed called for index %d, want 2", index)
		}
		return 10, 20, 30, 40
	})

	if got := existing.TotalRx.Load(); got != 5 {
		t.Fatalf("SetSeed changed an existing entry: total_rx = %d, want 5", got)
	}
	seeded := registry.Register(2)
	if got := seeded.TotalRx.Load(); got != 10 {
		t.Fatalf("seeded total_rx = %d, want 10", got)
	}
	if got := seeded.TotalTx.Load(); got != 20 {
		t.Fatalf("seeded total_tx = %d, want 20", got)
	}
	if got := seeded.BillableRx.Load(); got != 30 {
		t.Fatalf("seeded billable_rx = %d, want 30", got)
	}
	if got := seeded.BillableTx.Load(); got != 40 {
		t.Fatalf("seeded billable_tx = %d, want 40", got)
	}
}
