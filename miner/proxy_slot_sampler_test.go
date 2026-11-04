package miner

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urfoundation/sn/miner/bandwidth"
	"github.com/urnetwork/sdk"
)

func countProxySlotSamplerGoroutines() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "proxySlotSamplerLoop")
}

// TestProxySlotSampler_GoroutinesPerSlotCount proves the goroutine count:
// With N slots registered (each having both transport health and billing),
// exactly N ticker goroutines run across the pool (1 per slot instead of 2).
func TestProxySlotSampler_GoroutinesPerSlotCount(t *testing.T) {
	const nSlots = 10
	stops := make([]func(), nSlots)
	ctx := context.Background()

	for i := 0; i < nSlots; i++ {
		key := fmt.Sprintf("slot-key-%d", i)
		transport := &fakeProviderTransport{connected: true}
		bw := &bandwidth.ProxyBandwidth{}
		src := &fakeBillingSource{}
		stops[i] = watchProxySlotSampler(ctx, key, transport, bw, src, time.Hour)
	}

	deadline := time.Now().Add(2 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		got = countProxySlotSamplerGoroutines()
		if got == nSlots {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got != nSlots {
		t.Fatalf("expected exactly %d ticker goroutines for %d slots (1 per slot), got %d", nSlots, nSlots, got)
	}

	for _, stop := range stops {
		stop()
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got = countProxySlotSamplerGoroutines()
		if got == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got != 0 {
		t.Fatalf("expected 0 ticker goroutines after stops, got %d", got)
	}
}

type blockingTransportState struct {
	mu      sync.Mutex
	unblock chan struct{}
	called  atomic.Int64
}

func (b *blockingTransportState) GetProviderConnected() bool {
	b.called.Add(1)
	if b.unblock != nil {
		<-b.unblock
	}
	return true
}

// TestProxySlotSampler_SlowJobDoesNotBlockOtherSlots proves that a slow or
// blocked job for one slot does not stop other slots from being serviced.
func TestProxySlotSampler_SlowJobDoesNotBlockOtherSlots(t *testing.T) {
	unblock := make(chan struct{})
	slowTransport := &blockingTransportState{unblock: unblock}

	fastSrc := &fakeBillingSource{}
	fastBw := &bandwidth.ProxyBandwidth{}
	fastSrc.set(100, 200, 50, 50)

	ctx := context.Background()

	// Slot 1: slow/blocked job
	stop1 := watchProxySlotSampler(ctx, "slow-slot", slowTransport, nil, nil, 5*time.Millisecond)
	defer func() {
		close(unblock)
		stop1()
	}()

	// Wait until slot 1 has actually entered its blocking call
	deadline := time.Now().Add(2 * time.Second)
	for slowTransport.called.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("slow slot never called GetProviderConnected")
		}
		time.Sleep(time.Millisecond)
	}

	// Slot 2: normal slot on the same cadence
	stop2 := watchProxySlotSampler(ctx, "", nil, fastBw, fastSrc, 5*time.Millisecond)
	defer stop2()

	// While slot 1 is STILL blocked on <-unblock, slot 2 must be serviced repeatedly.
	deadline = time.Now().Add(2 * time.Second)
	for fastBw.BillableTx.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("slot 2 was not serviced while slot 1 was blocked: head-of-line blocking occurred")
		}
		time.Sleep(time.Millisecond)
	}

	// Mutate slot 2's readings and verify it continues to tick while slot 1 remains blocked
	for i := 1; i <= 5; i++ {
		fastSrc.set(100*int64(i+1), 200*int64(i+1), 50, 50)
		targetTx := uint64(100 * (i + 1))
		tickDeadline := time.Now().Add(2 * time.Second)
		for fastBw.BillableTx.Load() < targetTx {
			if time.Now().After(tickDeadline) {
				t.Fatalf("slot 2 stopped being serviced at iteration %d while slot 1 blocked", i)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// TestProxySlotSampler_CleanTeardownAndCancellation proves that cancellation
// and shutdown leave no goroutine running and allow no further ticks.
func TestProxySlotSampler_CleanTeardownAndCancellation(t *testing.T) {
	bw := &bandwidth.ProxyBandwidth{}
	src := &fakeBillingSource{}
	id := sdk.NewId()
	src.setContracts(contractDetails([]*sdk.Id{id}, false), nil)
	src.set(1000, 2000, 100, 100)

	transport := &fakeProviderTransport{connected: true}
	key := "teardown-slot-key"

	stop := watchProxySlotSampler(context.Background(), key, transport, bw, src, 5*time.Millisecond)

	// Wait until clients are counted and bytes are sampled
	deadline := time.Now().Add(2 * time.Second)
	for bw.Clients.Load() == 0 || bw.BillableTx.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("slot sampler never sampled initial clients/bytes")
		}
		time.Sleep(time.Millisecond)
	}

	// Stop the sampler
	stop()

	// Clients must be immediately released
	if got := bw.Clients.Load(); got != 0 {
		t.Fatalf("clients not released on stop: got %d, want 0", got)
	}

	// Ticker goroutine must be gone
	deadline = time.Now().Add(2 * time.Second)
	var count int
	for time.Now().Before(deadline) {
		count = countProxySlotSamplerGoroutines()
		if count == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if count != 0 {
		t.Fatalf("ticker goroutine still running after stop: %d", count)
	}

	// Device closes now (simulated: change readings and disconnect transport)
	transport.set(false)
	src.set(5000, 10000, 100, 100)
	lastTx := bw.BillableTx.Load()

	time.Sleep(50 * time.Millisecond)
	if got := bw.BillableTx.Load(); got != lastTx {
		t.Fatalf("slot sampler sampled after stop: %d -> %d", lastTx, got)
	}
}

// TestProxySlotSampler_DirectSlotBillingOnly tests direct slots without transport.
func TestProxySlotSampler_DirectSlotBillingOnly(t *testing.T) {
	bw := &bandwidth.ProxyBandwidth{}
	src := &fakeBillingSource{}
	src.set(500, 1500, 50, 50)

	stop := watchProxySlotSampler(context.Background(), "", nil, bw, src, 5*time.Millisecond)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for bw.BillableTx.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("direct slot never sampled billing bytes")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestProxySlotSampler_NilInputsNoop tests that nil inputs start zero goroutines.
func TestProxySlotSampler_NilInputsNoop(t *testing.T) {
	stop := watchProxySlotSampler(context.Background(), "", nil, nil, nil, time.Millisecond)
	if stop == nil {
		t.Fatal("expected non-nil stop func")
	}
	stop()
	if count := countProxySlotSamplerGoroutines(); count != 0 {
		t.Fatalf("expected 0 goroutines for nil inputs, got %d", count)
	}
}
