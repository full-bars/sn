package miner

import (
	"math/bits"
	"sort"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// Proxy grid cell state codes. They are the wire values the provider sends for
// each proxy and the client decodes; keep them in step with proxyCellStateName.
const (
	ProxyCellUp uint8 = iota
	ProxyCellDegraded
	ProxyCellConnecting
	ProxyCellDead
	ProxyCellParked
)

// ProxyCellDetail is the full per-proxy record from the provider's
// "proxy_cell_detail" command, used for the hover/click popup.
type ProxyCellDetail struct {
	Index         int    `json:"index"`
	Addr          string `json:"addr"`
	State         uint8  `json:"state"`
	Clients       int64  `json:"clients"`
	IntervalBytes uint64 `json:"interval_bytes"`
	AgeSeconds    int64  `json:"age_seconds"`
}

type proxyCellRaw struct {
	Index   int
	Key     string
	Addr    string
	State   uint8
	Total   uint64
	Clients int64
	Age     time.Duration
}

// proxyCellStore serves compact per-proxy grid cells from the provider control
// socket. It holds the computed wire bytes for one second so repeated polls
// (top draws every frame) reuse them instead of re-scanning the proxy registry
// and recomputing throughput buckets on every tick.
//
// Packed wire format, 4 bytes per proxy:
//
//	[indexLo, indexHi, state<<5|bucket, clients]
//
// state is one of ProxyCellUp..ProxyCellParked; bucket is 0..7; clients
// is capped at 255. The whole slice is a base64 string on the JSON wire.
type proxyCellStore struct {
	mu      sync.Mutex
	at      time.Time
	raw     []proxyCellRaw
	deltas  map[int]uint64
	cells   []byte
	prev    map[int]uint64
	prevKey map[int]string
	prevAt  time.Time
}

// proxyCellCacheTTL is how long the computed grid cells stay valid. A second
// matches top's snapshot cadence and keeps throughput buckets meaningful.
const proxyCellCacheTTL = time.Second

// Allow three times top's slowest built-in polling step (10s), while still
// discarding traffic accumulated during a long-closed top session.
const proxyCellMaxBaselineAge = 30 * time.Second

var (
	proxyCellStoreMu sync.Mutex
	proxyCellStore_  *proxyCellStore
)

func getProxyCellStore() *proxyCellStore {
	proxyCellStoreMu.Lock()
	defer proxyCellStoreMu.Unlock()
	if proxyCellStore_ == nil {
		proxyCellStore_ = &proxyCellStore{prev: map[int]uint64{}}
	}
	return proxyCellStore_
}

func resetProxyCellStoreForTest() {
	proxyCellStoreMu.Lock()
	defer proxyCellStoreMu.Unlock()
	proxyCellStore_ = &proxyCellStore{prev: map[int]uint64{}}
}

func proxyEntryCellState(health, downSince string, now time.Time) uint8 {
	switch health {
	case "parked":
		return ProxyCellParked
	case "up":
		return ProxyCellUp
	case "connecting":
		return ProxyCellConnecting
	case "recently_offline", "offline", "long_offline":
		if downSince != "" {
			if t, err := time.Parse(time.RFC3339, downSince); err == nil && !t.IsZero() && now.Sub(t) >= 7*24*time.Hour {
				return ProxyCellDead
			}
		}
		return ProxyCellDegraded
	case "dead", "inactive":
		return ProxyCellDead
	default:
		if health == "" {
			return ProxyCellConnecting
		}
		return ProxyCellDead
	}
}

func sampleProxyCellsRaw(now time.Time) []proxyCellRaw {
	state, err := readProxyState()
	if err != nil || state == nil || len(state.Proxies) == 0 {
		return nil
	}
	overlayLiveProxyHealth(state.Proxies)
	liveBw := proxyEarnBandwidthSnapshot()

	out := make([]proxyCellRaw, 0, len(state.Proxies))
	for key, entry := range state.Proxies {
		c := proxyCellRaw{
			Index: entry.ID,
			Key:   key,
			State: proxyEntryCellState(entry.Health, entry.DownSince, now),
		}
		addr, _ := connect.SplitProxyKey(key)
		if addr == "" {
			addr = key
		}
		c.Addr = addr

		if liveBw != nil {
			if bw := liveBw[key]; bw != nil {
				c.Total = bw.TotalRx.Load() + bw.TotalTx.Load()
				c.Clients = bw.Clients.Load()
				c.Age = bw.MaxAge()
			}
		}
		out = append(out, c)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Index != out[j].Index {
			return out[i].Index < out[j].Index
		}
		return out[i].Key < out[j].Key
	})
	return out
}

var readProxyCellsRawFunc = sampleProxyCellsRaw

// sample refreshes the cache if expired and returns the raw pool plus the
// per-index interval-byte deltas. Callers hold c.mu.
func (c *proxyCellStore) sample(now time.Time) ([]proxyCellRaw, map[int]uint64) {
	if len(c.raw) > 0 && !now.Before(c.at) && now.Sub(c.at) < proxyCellCacheTTL {
		return c.raw, c.deltas
	}
	raw := readProxyCellsRawFunc(now)
	deltas := make(map[int]uint64, len(raw))
	nextPrev := make(map[int]uint64, len(raw))
	nextKey := make(map[int]string, len(raw))
	buf := make([]byte, 0, 4*len(raw))
	elapsed := now.Sub(c.at)
	validBaseline := !c.at.IsZero() && elapsed > 0 && elapsed <= proxyCellMaxBaselineAge
	for _, p := range raw {
		delta := uint64(0)
		if last, ok := c.prev[p.Index]; validBaseline && ok && c.prevKey[p.Index] == p.Key && p.Total >= last {
			delta = p.Total - last
		}
		deltas[p.Index] = delta
		bucket := proxyThroughputBucket(proxyBytesPerSecond(delta, elapsed))
		state := p.State
		if state > 7 {
			state = 7 // guard: state codes fit in 3 bits
		}
		clients := p.Clients
		if clients < 0 {
			clients = 0
		} else if clients > 255 {
			clients = 255
		}
		index := p.Index
		if index < 0 {
			index = 0
		} else if index > 0xFFFF {
			index = 0xFFFF
		}
		buf = append(buf,
			byte(index), byte(index>>8),
			state<<5|bucket,
			byte(clients),
		)
		nextPrev[p.Index] = p.Total
		nextKey[p.Index] = p.Key
	}
	c.prev, c.prevKey = nextPrev, nextKey
	c.prevAt = c.at
	c.at = now
	c.raw = raw
	c.deltas = deltas
	c.cells = buf
	return raw, deltas
}

// Cells returns the packed grid-cell wire bytes for now, recomputing at most
// once per proxyCellCacheTTL.
func (c *proxyCellStore) Cells(now time.Time) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sample(now)
	return c.cells
}

// Detail returns the full per-proxy records for the hover/click popup. It
// shares the cached sample with Cells so the popup describes the same pool the
// grid shows. Not polled; called only when the popup opens.
func (c *proxyCellStore) Detail(now time.Time) []ProxyCellDetail {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, deltas := c.sample(now)
	out := make([]ProxyCellDetail, 0, len(raw))
	for _, p := range raw {
		clients := p.Clients
		if clients < 0 {
			clients = 0
		}
		out = append(out, ProxyCellDetail{
			Index:         p.Index,
			Addr:          p.Addr,
			State:         p.State,
			Clients:       clients,
			IntervalBytes: deltas[p.Index],
			AgeSeconds:    int64(p.Age / time.Second),
		})
	}
	return out
}

// proxyBytesPerSecond scales without overflowing even for very large counters.
func proxyBytesPerSecond(delta uint64, elapsed time.Duration) uint64 {
	if elapsed <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(delta, uint64(time.Second))
	denom := uint64(elapsed)
	if hi >= denom {
		return ^uint64(0)
	}
	rate, _ := bits.Div64(hi, lo, denom)
	return rate
}

// proxyThroughputBucket buckets bytes per second into
// eight logarithmic levels so the heatmap can shade cells by activity:
//
//	0 idle, 1 <1KiB, 2 <8KiB, 3 <64KiB, 4 <512KiB,
//	5 <4MiB, 6 <32MiB, 7 >=32MiB
func proxyThroughputBucket(delta uint64) uint8 {
	switch {
	case delta == 0:
		return 0
	case delta < 1<<10:
		return 1
	case delta < 8<<10:
		return 2
	case delta < 64<<10:
		return 3
	case delta < 512<<10:
		return 4
	case delta < 4<<20:
		return 5
	case delta < 32<<20:
		return 6
	default:
		return 7
	}
}
