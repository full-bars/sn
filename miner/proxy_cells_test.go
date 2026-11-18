package miner

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/urfoundation/sn/miner/bandwidth"
)

func TestProxyCellStoreDetail(t *testing.T) {
	resetProxyCellStoreForTest()
	now := time.Unix(1000, 0)

	bwMap := map[string]*bandwidth.ProxyBandwidth{
		"10.0.0.1:1080": {},
		"10.0.0.2:1080": {},
	}

	rawPool := []proxyCellRaw{
		{Index: 0, Key: "10.0.0.1:1080", Addr: "10.0.0.1:1080", State: ProxyCellUp},
		{Index: 1, Key: "10.0.0.2:1080", Addr: "10.0.0.2:1080", State: ProxyCellDegraded},
	}

	prevFunc := readProxyCellsRawFunc
	readProxyCellsRawFunc = func(t time.Time) []proxyCellRaw {
		for i := range rawPool {
			if bw := bwMap[rawPool[i].Key]; bw != nil {
				rawPool[i].Total = bw.TotalRx.Load() + bw.TotalTx.Load()
				rawPool[i].Clients = bw.Clients.Load()
			}
		}
		return rawPool
	}
	defer func() { readProxyCellsRawFunc = prevFunc }()

	store := &proxyCellStore{prev: map[int]uint64{}}

	// first call establishes the baseline (interval bytes 0)
	cells := store.Cells(now)
	if len(cells) != 8 {
		t.Fatalf("want 8 packed bytes for 2 proxies, got %d", len(cells))
	}
	detail := store.Detail(now)
	if len(detail) != 2 {
		t.Fatalf("want 2 detail records, got %d", len(detail))
	}
	byIndex := map[int]ProxyCellDetail{}
	for _, d := range detail {
		byIndex[d.Index] = d
	}
	if byIndex[0].Addr != "10.0.0.1:1080" || byIndex[1].Addr != "10.0.0.2:1080" {
		t.Fatalf("detail addrs wrong: %+v", detail)
	}
	for _, d := range detail {
		if d.IntervalBytes != 0 {
			t.Fatalf("first sample should report zero interval bytes, got %d for proxy %d", d.IntervalBytes, d.Index)
		}
	}

	// advance counters and re-sample: interval = 512KiB rx + 256KiB tx
	bw := bwMap["10.0.0.1:1080"]
	bw.TotalRx.Store(512 << 10)
	bw.TotalTx.Store(256 << 10)
	now2 := now.Add(time.Second)
	cells2 := store.Cells(now2)
	detail2 := store.Detail(now2)
	byIndex = map[int]ProxyCellDetail{}
	for _, d := range detail2 {
		byIndex[d.Index] = d
	}
	wantBytes := uint64(512<<10) + uint64(256<<10)
	if byIndex[0].IntervalBytes != wantBytes {
		t.Fatalf("proxy 0 interval bytes = %d, want %d", byIndex[0].IntervalBytes, wantBytes)
	}
	// the bucket for ~768 KiB in the interval must be >= 4 (512KiB..4MiB band)
	bucket := cells2[2] & 0x07
	if bucket < 4 {
		t.Fatalf("proxy 0 bucket = %d, want >= 4 for a 768 KiB interval", bucket)
	}
}

func TestProxyCellStoreCacheValidOneSecond(t *testing.T) {
	prevFunc := readProxyCellsRawFunc
	readProxyCellsRawFunc = func(t time.Time) []proxyCellRaw {
		return []proxyCellRaw{{Index: 0, Key: "10.0.0.1:1080", Addr: "10.0.0.1:1080"}}
	}
	defer func() { readProxyCellsRawFunc = prevFunc }()

	store := &proxyCellStore{prev: map[int]uint64{}}
	now := time.Now()
	first := store.Cells(now)
	// same-second call must return the cached slice, not rescan
	second := store.Cells(now.Add(500 * time.Millisecond))
	if &first[0] != &second[0] {
		t.Fatal("cached cells should be returned within the TTL")
	}
	// after the TTL it recomputes
	third := store.Cells(now.Add(time.Second + time.Millisecond))
	if len(third) != len(first) {
		t.Fatalf("recomputed cells length %d != %d", len(third), len(first))
	}
}

func TestProxyCellDetailJSONShape(t *testing.T) {
	detail := ProxyCellDetail{Index: 3, Addr: "10.0.0.1:1080", State: 1, Clients: 2, IntervalBytes: 1234, AgeSeconds: 42}
	b, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var got ProxyCellDetail
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Index != 3 || got.Addr != "10.0.0.1:1080" || got.IntervalBytes != 1234 || got.AgeSeconds != 42 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestProxyCellStoreConcurrentCellsAndDetail(t *testing.T) {
	raw := make([]proxyCellRaw, 100)
	for i := 0; i < 100; i++ {
		raw[i] = proxyCellRaw{Index: i, Key: "10.0.0.1:1080", Addr: "10.0.0.1:1080"}
	}
	prevFunc := readProxyCellsRawFunc
	readProxyCellsRawFunc = func(t time.Time) []proxyCellRaw {
		return raw
	}
	defer func() { readProxyCellsRawFunc = prevFunc }()

	store := &proxyCellStore{prev: map[int]uint64{}}
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = store.Cells(now)
		}()
		go func() {
			defer wg.Done()
			_ = store.Detail(now)
		}()
	}
	wg.Wait()
}

func TestProxyCellRatesByElapsedTime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		delta   uint64
		bucket  uint8
		detail  uint64
	}{
		{"one second", time.Second, 4096, 2, 4096},
		{"two seconds", 2 * time.Second, 8192, 2, 8192},
		{"three seconds", 3 * time.Second, 12288, 2, 12288},
		{"ten seconds", 10 * time.Second, 40960, 2, 40960},
		{"long gap", 10 * time.Minute, 900000, 0, 0},
		{"zero elapsed", 0, 2048, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bw := &bandwidth.ProxyBandwidth{}
			prevFunc := readProxyCellsRawFunc
			readProxyCellsRawFunc = func(t time.Time) []proxyCellRaw {
				return []proxyCellRaw{
					{
						Index: 0,
						Key:   "address",
						Addr:  "address",
						Total: bw.TotalRx.Load() + bw.TotalTx.Load(),
					},
				}
			}
			defer func() { readProxyCellsRawFunc = prevFunc }()

			now := time.Unix(1000, 0)
			store := &proxyCellStore{}
			store.Cells(now)
			bw.TotalRx.Store(tc.delta)
			if tc.elapsed == 0 {
				store.raw = nil
			} // force a resample at the same timestamp
			cells := store.Cells(now.Add(tc.elapsed))
			detail := store.Detail(now.Add(tc.elapsed))
			if got := cells[2] & 7; got != tc.bucket {
				t.Errorf("bucket=%d want %d", got, tc.bucket)
			}
			if got := detail[0].IntervalBytes; got != tc.detail {
				t.Errorf("interval_bytes=%d want %d", got, tc.detail)
			}
		})
	}
}

func TestProxyCellStoreRegistrationRebase(t *testing.T) {
	bw := &bandwidth.ProxyBandwidth{}
	curKey := "first"
	prevFunc := readProxyCellsRawFunc
	readProxyCellsRawFunc = func(t time.Time) []proxyCellRaw {
		return []proxyCellRaw{
			{
				Index: 1,
				Key:   curKey,
				Addr:  "same",
				Total: bw.TotalRx.Load() + bw.TotalTx.Load(),
			},
		}
	}
	defer func() { readProxyCellsRawFunc = prevFunc }()

	now := time.Unix(1000, 0)
	store := &proxyCellStore{prev: map[int]uint64{}}
	bw.TotalRx.Store(100)
	store.Detail(now)

	curKey = "second"
	bw.TotalRx.Store(200)
	if got := store.Detail(now.Add(time.Second))[0].IntervalBytes; got != 0 {
		t.Fatalf("new registration inherited interval %d", got)
	}

	bw.TotalRx.Store(250)
	if got := store.Detail(now.Add(2 * time.Second))[0].IntervalBytes; got != 50 {
		t.Fatalf("same registration interval %d, want 50", got)
	}

	curKey = "third"
	bw.TotalRx.Store(300)
	if got := store.Detail(now.Add(3 * time.Second))[0].IntervalBytes; got != 0 {
		t.Fatalf("unobserved round-trip registration inherited interval %d", got)
	}
}

func TestProxyCellControlSocketResponse(t *testing.T) {
	resetProxyCellStoreForTest()
	bw := &bandwidth.ProxyBandwidth{}
	bw.TotalRx.Store(1024)
	bw.Clients.Store(3)

	prevFunc := readProxyCellsRawFunc
	readProxyCellsRawFunc = func(t time.Time) []proxyCellRaw {
		return []proxyCellRaw{
			{
				Index:   42,
				Key:     "10.0.0.1:1080",
				Addr:    "10.0.0.1:1080",
				State:   ProxyCellUp,
				Total:   bw.TotalRx.Load(),
				Clients: bw.Clients.Load(),
			},
		}
	}
	defer func() { readProxyCellsRawFunc = prevFunc }()

	resp := handleControlRequest(nil, controlRequest{Cmd: "proxy_cells"})
	if !resp.OK {
		t.Fatalf("proxy_cells request failed: %s", resp.Error)
	}
	if len(resp.ProxyCells) != 4 {
		t.Fatalf("want 4 bytes for 1 proxy cell, got %d", len(resp.ProxyCells))
	}
	idx := int(resp.ProxyCells[0]) | int(resp.ProxyCells[1])<<8
	if idx != 42 {
		t.Fatalf("want index 42, got %d", idx)
	}
	if clients := resp.ProxyCells[3]; clients != 3 {
		t.Fatalf("want clients 3, got %d", clients)
	}

	respDetail := handleControlRequest(nil, controlRequest{Cmd: "proxy_cell_detail"})
	if !respDetail.OK {
		t.Fatalf("proxy_cell_detail request failed: %s", respDetail.Error)
	}
	if len(respDetail.ProxyCellDetail) != 1 {
		t.Fatalf("want 1 detail record, got %d", len(respDetail.ProxyCellDetail))
	}
	d := respDetail.ProxyCellDetail[0]
	if d.Index != 42 || d.Addr != "10.0.0.1:1080" || d.State != ProxyCellUp || d.Clients != 3 {
		t.Fatalf("unexpected detail: %+v", d)
	}
}
