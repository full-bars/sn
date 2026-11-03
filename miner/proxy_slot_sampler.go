package miner

import (
	"context"
	"sync"
	"time"

	"github.com/urfoundation/sn/miner/bandwidth"
)

// proxyTransportTracker maintains connection state for one slot's platform transport.
type proxyTransportTracker struct {
	key               string
	transport         proxyTransportState
	connected         bool
	disconnectedSince time.Time
}

func (t *proxyTransportTracker) sample(ctx context.Context) {
	if t == nil || t.key == "" || t.transport == nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	now := t.transport.GetProviderConnected()
	switch {
	case now:
		t.disconnectedSince = time.Time{}
		if !t.connected {
			t.connected = true
			noteLiveProxyTransportUp(t.key)
		}
	case t.connected:
		if t.disconnectedSince.IsZero() {
			t.disconnectedSince = time.Now()
		}
		if time.Since(t.disconnectedSince) >= proxyTransportDropConfirm {
			if ctx.Err() != nil {
				return
			}
			t.connected = false
			t.disconnectedSince = time.Time{}
			noteLiveProxyHealthDrop(t.key)
		}
	}
}

// proxySlotSamplerLoop drives periodic sampling of platform transport connection
// health and billing accounting for one slot. It samples immediately on launch,
// then ticks on interval until ctx is cancelled.
func proxySlotSamplerLoop(
	ctx context.Context,
	tracker *proxyTransportTracker,
	sampler *proxyBillingSampler,
	src slotBillingSource,
	interval time.Duration,
	done chan<- struct{},
) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if ctx.Err() != nil {
			return
		}
		if tracker != nil {
			tracker.sample(ctx)
		}
		if sampler != nil && src != nil {
			sampler.sample(src)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// watchProxySlotSampler drives transport health and billing sampling for one
// slot from a single ticker goroutine. On a node running hundreds of slots,
// this cuts the per-slot ticker goroutine count in half (from 2 to 1 per slot)
// while preserving independent execution across slots (avoiding head-of-line
// blocking between different slots).
//
// A proxied slot supplies both:
//   - key != "" and transport != nil: platform transport connection state is sampled
//   - bw != nil and src != nil: billing bytes and open contracts are sampled
//
// A direct slot supplies transport == nil and key == "", so only billing is sampled.
//
// It returns a stop func that cancels the watch and waits for the goroutine
// to exit before releasing client sessions. Defers in the slot must call stop
// before device close so teardown is never read as a drop or client leak.
func watchProxySlotSampler(
	ctx context.Context,
	key string,
	transport proxyTransportState,
	bw *bandwidth.ProxyBandwidth,
	src slotBillingSource,
	interval time.Duration,
) (stop func()) {
	var tracker *proxyTransportTracker
	if key != "" && transport != nil {
		tracker = &proxyTransportTracker{
			key:       key,
			transport: transport,
		}
	}
	var sampler *proxyBillingSampler
	if bw != nil && src != nil {
		sampler = &proxyBillingSampler{bw: bw}
	}
	if tracker == nil && sampler == nil {
		return func() {}
	}
	if interval <= 0 {
		interval = providerBillingSampleInterval
	}

	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go proxySlotSamplerLoop(watchCtx, tracker, sampler, src, interval, done)

	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			cancel()
			<-done
			if sampler != nil {
				sampler.releaseClients()
			}
		})
	}
}
