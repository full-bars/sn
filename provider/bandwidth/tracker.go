// Package bandwidth provides connection wrappers that count bytes for billing.
// It wraps both TCP (net.Conn) and UDP (net.PacketConn) at the provider's
// DialContextSettings boundary, feeding the same ProxyBandwidth counters
// regardless of transport protocol (H1/TCP or H3/QUIC/UDP).
//
// DESIGN ADAPTATION (v2026 migration):
// The fork tracked bandwidth by wrapping net.Conn inside connect's package
// via trackedConn in net.go. This worked for H1/TCP but made H3/UDP traffic
// invisible to billing (zero bytes reported). v2026 exposes DialContextSettings
// with two seams: DialContext (TCP streams) and PacketConnFactory (UDP sockets).
// By wrapping both seams at the provider boundary, we get byte counting for
// both H1 and H3 without forking connect itself. The tradeoff is slightly less
// granularity (we can't see errors deep inside connect internals) but we stay
// on upstream's public API surface.
package bandwidth

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ProxyBandwidth holds per-proxy atomic byte counters and session tracking.
// Safe for concurrent use from multiple goroutines and transport paths.
type ProxyBandwidth struct {
	TotalRx, TotalTx, BillableRx, BillableTx atomic.Uint64
	Clients                                  atomic.Int64
	LatencyNs                                atomic.Int64
	SocksLatencyNs                           atomic.Int64

	mu            sync.Mutex
	sessions      map[any]time.Time
	presenceSince time.Time
	lastActivity  time.Time
}

const clientPresenceGrace = 10 * time.Second

func (self *ProxyBandwidth) AddSession(key any, start time.Time) {
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.sessions == nil {
		self.sessions = make(map[any]time.Time)
	}
	if len(self.sessions) == 0 {
		gap := time.Since(self.lastActivity)
		if self.presenceSince.IsZero() || gap >= clientPresenceGrace {
			self.presenceSince = start
		}
	}
	self.sessions[key] = start
	self.lastActivity = time.Now()
}

func (self *ProxyBandwidth) RemoveSession(key any) {
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.sessions != nil {
		delete(self.sessions, key)
		if len(self.sessions) == 0 {
			self.lastActivity = time.Now()
		}
	}
}

func (self *ProxyBandwidth) MaxAge() time.Duration {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.ageLocked()
}

func (self *ProxyBandwidth) ageLocked() time.Duration {
	if self.presenceSince.IsZero() {
		return 0
	}
	if len(self.sessions) == 0 && time.Since(self.lastActivity) >= clientPresenceGrace {
		return 0
	}
	return time.Since(self.presenceSince)
}

// Snapshot returns a detached copy of bw that carries the same
// latency, byte, and client counters, plus enough internal timing state for
// MaxAge() to return the correct value. It avoids the fake-AddSession pattern
// that silently zeroed LatencyNs/SocksLatencyNs on every copy.
func (bw *ProxyBandwidth) Snapshot() *ProxyBandwidth {
	if bw == nil {
		return nil
	}
	out := &ProxyBandwidth{}
	out.TotalRx.Store(bw.TotalRx.Load())
	out.TotalTx.Store(bw.TotalTx.Load())
	out.BillableRx.Store(bw.BillableRx.Load())
	out.BillableTx.Store(bw.BillableTx.Load())
	out.Clients.Store(bw.Clients.Load())
	out.LatencyNs.Store(bw.LatencyNs.Load())
	out.SocksLatencyNs.Store(bw.SocksLatencyNs.Load())

	bw.mu.Lock()
	out.presenceSince = bw.presenceSince
	out.lastActivity = bw.lastActivity
	if bw.sessions != nil && len(bw.sessions) > 0 {
		out.sessions = make(map[any]time.Time, 1)
		out.sessions["snapshot"] = time.Now()
	}
	bw.mu.Unlock()

	return out
}

// ProxyRegistry holds per-index bandwidth counters.
type ProxyRegistry struct {
	mu       sync.RWMutex
	counters map[int]*ProxyBandwidth
}

// NewRegistry creates an empty registry.
func NewRegistry() *ProxyRegistry {
	return &ProxyRegistry{
		counters: make(map[int]*ProxyBandwidth),
	}
}

// Register returns the bandwidth counter for the given proxy index,
// creating one if it doesn't exist.
func (r *ProxyRegistry) Register(index int) *ProxyBandwidth {
	r.mu.RLock()
	bw, ok := r.counters[index]
	r.mu.RUnlock()
	if ok {
		return bw
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check after acquiring write lock.
	if bw, ok = r.counters[index]; ok {
		return bw
	}
	bw = &ProxyBandwidth{}
	r.counters[index] = bw
	return bw
}

// Snapshot returns all counters for reporting/metrics.
func (r *ProxyRegistry) Snapshot() map[int]*ProxyBandwidth {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[int]*ProxyBandwidth, len(r.counters))
	for k, v := range r.counters {
		out[k] = v
	}
	return out
}

// Conn wraps a net.Conn and counts every Read/Write byte into
// the associated ProxyBandwidth counters.
type Conn struct {
	net.Conn
	bw        *ProxyBandwidth
	proxyAddr string
}

// NewConn wraps conn with bandwidth tracking for the given proxy.
func NewConn(conn net.Conn, bw *ProxyBandwidth, proxyAddr string) *Conn {
	return &Conn{Conn: conn, bw: bw, proxyAddr: proxyAddr}
}

func (c *Conn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.bw.TotalRx.Add(uint64(n))
	}
	return n, err
}

func (c *Conn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.bw.TotalTx.Add(uint64(n))
	}
	return n, err
}

func (c *Conn) ProxyAddress() string { return c.proxyAddr }

// PacketConn wraps a net.PacketConn and counts every ReadFrom/WriteTo byte
// into the associated ProxyBandwidth counters. This is the H3/QUIC path —
// without this wrapper, QUIC traffic reports zero bytes to billing.
type PacketConn struct {
	net.PacketConn
	bw        *ProxyBandwidth
	proxyAddr string
}

// NewPacketConn wraps pc with bandwidth tracking for the given proxy.
func NewPacketConn(pc net.PacketConn, bw *ProxyBandwidth, proxyAddr string) *PacketConn {
	return &PacketConn{PacketConn: pc, bw: bw, proxyAddr: proxyAddr}
}

func (pc *PacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := pc.PacketConn.ReadFrom(p)
	if n > 0 {
		pc.bw.TotalRx.Add(uint64(n))
	}
	return n, addr, err
}

func (pc *PacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	n, err := pc.PacketConn.WriteTo(p, addr)
	if n > 0 {
		pc.bw.TotalTx.Add(uint64(n))
	}
	return n, err
}

func (pc *PacketConn) ProxyAddress() string { return pc.proxyAddr }
