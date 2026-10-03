package bandwidth

import (
	"sync"
	"testing"
)

// The observer turns the engine's CUMULATIVE relay counts into billable deltas,
// mapping ingress (received from the tunnel) to BillableTx and egress (returned
// into the tunnel) to BillableRx, the same two quantities the 3.23-fix line
// counted inside its user NAT provider.
func TestRelayStatsObserverCountsDeltasNotCumulativeTotals(t *testing.T) {
	bw := &ProxyBandwidth{}
	observe := bw.RelayStatsObserver()

	observe(1000, 4000)
	observe(1500, 4000)
	observe(1500, 9000)
	// the same cumulative value reported twice must not double count
	observe(1500, 9000)

	if got := bw.BillableTx.Load(); got != 1500 {
		t.Fatalf("BillableTx = %d, want 1500 (the cumulative ingress, counted once)", got)
	}
	if got := bw.BillableRx.Load(); got != 9000 {
		t.Fatalf("BillableRx = %d, want 9000 (the cumulative egress, counted once)", got)
	}
}

// A rebuilt provider starts its cumulative counts from zero; the observer for
// the new provider is a fresh one and must count from its own zero without
// disturbing what the previous one already added.
func TestRelayStatsObserverRebuiltProviderAddsToTheSameProxyTotals(t *testing.T) {
	bw := &ProxyBandwidth{}
	first := bw.RelayStatsObserver()
	first(2000, 3000)

	second := bw.RelayStatsObserver()
	second(500, 700)
	second(900, 700)

	if got := bw.BillableTx.Load(); got != 2900 {
		t.Fatalf("BillableTx = %d, want 2000 + 900", got)
	}
	if got := bw.BillableRx.Load(); got != 3700 {
		t.Fatalf("BillableRx = %d, want 3000 + 700", got)
	}
}

// Within one observer the engine's counts only go up, so a value below the
// high-water mark is an out-of-order delivery (for example a final read racing
// an epoch). It adds nothing and must not re-add history or lower the mark.
func TestRelayStatsObserverOutOfOrderValueAddsNothing(t *testing.T) {
	bw := &ProxyBandwidth{}
	observe := bw.RelayStatsObserver()
	observe(5000, 7000)
	observe(100, 200) // late, older snapshot
	observe(5000, 7000)
	observe(5600, 7300)

	if got := bw.BillableTx.Load(); got != 5600 {
		t.Fatalf("BillableTx = %d, want 5600 (history counted once)", got)
	}
	if got := bw.BillableRx.Load(); got != 7300 {
		t.Fatalf("BillableRx = %d, want 7300 (history counted once)", got)
	}
}

// Reporting zero, or nothing new, changes nothing.
func TestRelayStatsObserverZeroAndUnchangedAreNoops(t *testing.T) {
	bw := &ProxyBandwidth{}
	observe := bw.RelayStatsObserver()
	observe(0, 0)
	observe(0, 0)
	if bw.BillableTx.Load() != 0 || bw.BillableRx.Load() != 0 {
		t.Fatalf("zero reports moved the counters: tx=%d rx=%d", bw.BillableTx.Load(), bw.BillableRx.Load())
	}
}

// The engine fires the callback from a worker while a final read can race it;
// concurrent, interleaved reports of one ascending cumulative series must count
// each byte exactly once.
func TestRelayStatsObserverConcurrentReportsCountEachByteOnce(t *testing.T) {
	bw := &ProxyBandwidth{}
	observe := bw.RelayStatsObserver()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for v := uint64(1); v <= 1000; v++ {
				observe(v, v*2)
			}
		}()
	}
	wg.Wait()

	if got := bw.BillableTx.Load(); got != 1000 {
		t.Fatalf("BillableTx = %d, want exactly 1000", got)
	}
	if got := bw.BillableRx.Load(); got != 2000 {
		t.Fatalf("BillableRx = %d, want exactly 2000", got)
	}
}

// A final read taken after the last epoch adds exactly the bytes that epoch
// had not yet delivered, and a repeat of it adds nothing.
func TestRelayStatsObserverFinalFlushAddsOnlyTheRemainder(t *testing.T) {
	bw := &ProxyBandwidth{}
	observe := bw.RelayStatsObserver()
	observe(1000, 2000) // last epoch
	observe(1450, 2900) // the shutdown read
	observe(1450, 2900) // and again
	if bw.BillableTx.Load() != 1450 || bw.BillableRx.Load() != 2900 {
		t.Fatalf("tx=%d rx=%d, want 1450 and 2900", bw.BillableTx.Load(), bw.BillableRx.Load())
	}
}

// The wrapper no longer owns billable: it keeps counting total only.
func TestConnWriteCountsTotalNotBillable(t *testing.T) {
	bw := &ProxyBandwidth{}
	conn := NewConn(newMockConn(), bw, "p")
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 5)); err != nil && bw.TotalRx.Load() == 0 {
		// the mock may have nothing to read; the write alone proves the point
		_ = err
	}
	if bw.TotalTx.Load() != 5 {
		t.Fatalf("TotalTx = %d, want 5", bw.TotalTx.Load())
	}
	if bw.BillableTx.Load() != 0 || bw.BillableRx.Load() != 0 {
		t.Fatalf("the wrapper counted billable: tx=%d rx=%d", bw.BillableTx.Load(), bw.BillableRx.Load())
	}
}
