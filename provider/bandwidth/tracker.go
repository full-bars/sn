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

	"github.com/urnetwork/connect"
)

// ProxyBandwidth holds per-proxy atomic byte counters and session tracking.
// Safe for concurrent use from multiple goroutines and transport paths.
type ProxyBandwidth struct {
	TotalRx, TotalTx, BillableRx, BillableTx atomic.Uint64
	// ContractUsedIngress and ContractUsedEgress are the bytes the platform
	// contracts have used, summed from the engine's per-contract accounting
	// (ContractStatsObserver). Ingress is the receive-side contracts (traffic
	// from the clients), egress the send-side ones. This is the number the
	// platform settles on, kept as a cross-check against billable, which is
	// counted from the relay packets.
	ContractUsedIngress, ContractUsedEgress atomic.Uint64
	Clients                                 atomic.Int64
	LatencyNs                               atomic.Int64
	SocksLatencyNs                          atomic.Int64

	mu            sync.Mutex
	sessions      map[any]time.Time
	presenceSince time.Time
	lastActivity  time.Time
}

const clientPresenceGrace = 10 * time.Second

// RelayStatsObserver returns the function a proxy's remote provider feeds with
// its CUMULATIVE relay byte counts. The counts are the engine's own accounting
// of the IP packets it relayed for remote clients: ingress is traffic received
// from the tunnel (the clients' egress) and egress is the return traffic sent
// back into the tunnel. They are what BillableTx and BillableRx mean, and the
// same quantities the 3.23-fix line counted inside its user NAT provider.
//
// Billable used to be incremented in the connection wrapper next to the total,
// by the same byte count, so the two were always equal and billable was only
// the bytes carried on the relay-egress socket. The engine does not take a
// bandwidth object any more, so the wrapper cannot see which bytes belong to a
// client contract; the provider's own packet counters can.
//
// Each observer keeps its own high-water mark, so a provider that is rebuilt (a
// restart of this proxy) starts from zero without double counting. Within one
// observer the engine's counts only ever go up, so a value below the mark can
// only be an out-of-order delivery (a late final read racing an epoch): it adds
// nothing and the mark stays, instead of re-adding history.
// The function is safe to call from any goroutine.
func (self *ProxyBandwidth) RelayStatsObserver() func(ingress uint64, egress uint64) {
	var mu sync.Mutex
	var highIngress, highEgress uint64
	delta := func(current uint64, high *uint64) uint64 {
		if current <= *high {
			return 0
		}
		d := current - *high
		*high = current
		return d
	}
	return func(ingress uint64, egress uint64) {
		mu.Lock()
		in, out := delta(ingress, &highIngress), delta(egress, &highEgress)
		mu.Unlock()
		if 0 < in {
			self.BillableTx.Add(in)
		}
		if 0 < out {
			self.BillableRx.Add(out)
		}
	}
}

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
	out.ContractUsedIngress.Store(bw.ContractUsedIngress.Load())
	out.ContractUsedEgress.Store(bw.ContractUsedEgress.Load())
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
// the associated ProxyBandwidth counters. A Conn made by NewSessionConn also
// counts as one live client session on the ProxyBandwidth until it is closed.
type Conn struct {
	net.Conn
	bw        *ProxyBandwidth
	proxyAddr string

	sessionMu sync.Mutex
	tracked   bool // counted in bw.Clients and bw.sessions
	closed    bool
}

// NewConn wraps conn with bandwidth tracking for the given proxy. A conn that
// is already a *Conn for the same bw is returned as is, so wrapping twice
// never counts its bytes twice.
func NewConn(conn net.Conn, bw *ProxyBandwidth, proxyAddr string) *Conn {
	if c, ok := conn.(*Conn); ok && c.bw == bw {
		return c
	}
	return &Conn{Conn: conn, bw: bw, proxyAddr: proxyAddr}
}

// NewSessionConn wraps conn like NewConn and also counts it as one live
// client session on bw (Clients and the session presence window) until the
// first Close. Used on the relay-egress dial, where each connection carries a
// client's traffic; the provider's own control-plane connections use NewConn.
// Wrapping the same connection twice counts it once.
func NewSessionConn(conn net.Conn, bw *ProxyBandwidth, proxyAddr string) *Conn {
	c := NewConn(conn, bw, proxyAddr)
	c.trackSession()
	return c
}

func (c *Conn) trackSession() {
	if c.bw == nil {
		return
	}
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	if c.tracked || c.closed {
		return
	}
	c.tracked = true
	c.bw.Clients.Add(1)
	c.bw.AddSession(c, time.Now())
}

// releaseSession ends the session exactly once, whichever Close comes first.
func (c *Conn) releaseSession() {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.tracked {
		c.bw.Clients.Add(-1)
		c.bw.RemoveSession(c)
	}
}

// Close ends the client session (if any) before closing the underlying
// connection, so an error or panic from the inner Close cannot leak it.
func (c *Conn) Close() error {
	c.releaseSession()
	return c.Conn.Close()
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
// without this wrapper, QUIC traffic would report zero bytes to the TOTAL.
// Billable is not counted here: it comes from the provider's own relay
// accounting (RelayStatsObserver). This wrapper sits on the platform tunnel
// socket when H3 wins, so Total mixes relay-egress bytes with H3 tunnel bytes.
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

// ContractStatsObserver returns the function a proxy's contract manager feeds
// with per-contract usage events. Each event carries the bytes a contract used
// since its previous event; the observer sums them into ContractUsedIngress
// (receive contracts) and ContractUsedEgress (send contracts).
//
// Events can be delivered out of order or repeated, and a stale snapshot of an
// open contract can arrive after the contract closed, so each contract
// direction keeps the last sequence it applied and ignores anything at or below
// it. A closed contract is forgotten, so the map stays bounded by the number of
// open contracts. The function is safe to call from any goroutine.
func (self *ProxyBandwidth) ContractStatsObserver() func(events []*connect.ContractStatsEvent) {
	type contractDirection struct {
		id      connect.Id
		receive bool
	}
	var mu sync.Mutex
	lastSequence := map[contractDirection]uint64{}
	return func(events []*connect.ContractStatsEvent) {
		var ingress, egress uint64
		mu.Lock()
		for _, event := range events {
			if event == nil {
				continue
			}
			key := contractDirection{id: event.ContractId, receive: event.Receive}
			if last, seen := lastSequence[key]; seen && event.Sequence <= last {
				continue
			}
			if event.Open {
				lastSequence[key] = event.Sequence
			} else {
				// the final event for this contract: apply it, then forget it
				delete(lastSequence, key)
			}
			if 0 < event.UsedByteCountDelta {
				if event.Receive {
					ingress += uint64(event.UsedByteCountDelta)
				} else {
					egress += uint64(event.UsedByteCountDelta)
				}
			}
		}
		mu.Unlock()
		if 0 < ingress {
			self.ContractUsedIngress.Add(ingress)
		}
		if 0 < egress {
			self.ContractUsedEgress.Add(egress)
		}
	}
}
