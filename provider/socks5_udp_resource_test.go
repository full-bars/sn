package provider

import (
	"context"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func openDescriptorCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd on this platform")
	}
	return len(entries)
}

func settleGoroutines(target int) int {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= target {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}

// Every H3 attempt through a proxy opens a control connection, a UDP socket
// and a goroutine. The platform transport closes what it is given, so closing
// must give all of it back: a fleet of proxies retrying for days cannot leak a
// descriptor or a goroutine per attempt.
func TestSocks5UdpCloseGivesBackEveryDescriptorAndGoroutine(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	echo, _ := startEchoUdp(t)

	// one warm-up round so lazily created runtime state is not counted
	warm, err := dialSocks5UDP(context.Background(), "tcp", server.address(), nil)
	if err != nil {
		t.Fatal(err)
	}
	warm.WriteTo([]byte("x"), echo.LocalAddr())
	warm.Close()
	time.Sleep(200 * time.Millisecond)

	baseFds := openDescriptorCount(t)
	baseGoroutines := runtime.NumGoroutine()
	const rounds = 60
	for i := 0; i < rounds; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pc, err := dialSocks5UDP(ctx, "tcp", server.address(), nil)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		pc.WriteTo([]byte("x"), echo.LocalAddr())
		pc.Close()
	}
	time.Sleep(500 * time.Millisecond)

	if goroutines := settleGoroutines(baseGoroutines + 3); goroutines > baseGoroutines+3 {
		t.Fatalf("%d goroutines after %d open/close rounds, was %d: goroutines leak", goroutines, rounds, baseGoroutines)
	}
	if fds := openDescriptorCount(t); fds > baseFds+6 {
		t.Fatalf("%d descriptors after %d open/close rounds, was %d: descriptors leak", fds, rounds, baseFds)
	}
}

// minAllocsPerRun measures f several times and returns the smallest result.
// perAttempt runs before each measurement.
func minAllocsPerRun(attempts, runs int, f func(), perAttempt func()) float64 {
	best := -1.0
	for a := 0; a < attempts; a++ {
		perAttempt()
		if v := testing.AllocsPerRun(runs, f); best < 0 || v < best {
			best = v
		}
	}
	return best
}

// The packet conn sits on the QUIC hot path, so it must not allocate per
// packet what a pool can hand out. Writing needs nothing new at all; reading
// needs only the source address the interface returns.
func TestSocks5UdpPacketPathBarelyAllocates(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	echo, _ := startEchoUdp(t)
	pc := dialForTest(t, server, nil)
	server.waitRelayPort(t)

	payload := make([]byte, 1200)
	buf := make([]byte, 1500)
	destination := echo.LocalAddr()
	// warm the pools and the echo path
	pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	for i := 0; i < 20; i++ {
		pc.WriteTo(payload, destination)
		pc.ReadFrom(buf)
	}

	writeAllocs := minAllocsPerRun(5, 50, func() {
		pc.WriteTo(payload, destination)
	}, func() {})
	// leave the echoes of those writes to drain, then measure reads alone
	// against a bounded backlog (UDP drops what overflows a socket buffer)
	time.Sleep(300 * time.Millisecond)
	pc.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		if _, _, err := pc.ReadFrom(buf); err != nil {
			break
		}
	}
	for i := 0; i < 60; i++ {
		pc.WriteTo(payload, destination)
	}
	time.Sleep(300 * time.Millisecond)
	pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	readAllocs := testing.AllocsPerRun(40, func() {
		pc.ReadFrom(buf)
	})

	t.Logf("per packet: WriteTo %.1f allocations, ReadFrom %.1f allocations", writeAllocs, readAllocs)
	if writeAllocs > 0 {
		t.Fatalf("WriteTo allocates %.1f times per packet, want 0", writeAllocs)
	}
	if readAllocs > 2 {
		t.Fatalf("ReadFrom allocates %.1f times per packet, want at most 2 (the address it returns)", readAllocs)
	}
}

// A pool of a thousand proxies must not open a thousand checks at once: each
// check holds a control connection and a UDP socket, and a burst of them at
// startup is how a box runs out of descriptors.
func TestUdpCheckStoreLimitsHowManyChecksRunAtOnce(t *testing.T) {
	var running, peak atomic.Int32
	store := newProxyUDPCheckStore(func(ctx context.Context, settings *connect.ProxySettings) proxyUDPResult {
		now := running.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		running.Add(-1)
		return proxyUDPResult{Status: proxyUDPOK}
	})

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			settings := &connect.ProxySettings{Network: "tcp", Address: net.JoinHostPort("192.0.2.1", itoa(1000+i))}
			store.Check(context.Background(), settings, time.Now())
		}(i)
	}
	wg.Wait()

	if got := int(peak.Load()); got > udpCheckMaxConcurrent {
		t.Fatalf("%d checks ran at once, the limit is %d", got, udpCheckMaxConcurrent)
	}
	if peak.Load() < 2 {
		t.Fatalf("checks never overlapped (peak %d): the limit is serializing everything", peak.Load())
	}
}

// The store holds one small entry per identity ever checked. Identities come
// and go with the proxy lists, so entries nobody has asked about for a long
// time must not accumulate for the life of the process.
func TestUdpCheckStoreForgetsIdentitiesNobodyAsksAbout(t *testing.T) {
	store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{Status: proxyUDPOK}
	})
	start := time.Now()
	for i := 0; i < 200; i++ {
		store.Check(context.Background(), &connect.ProxySettings{Network: "tcp", Address: net.JoinHostPort("192.0.2.2", itoa(2000+i))}, start)
	}
	if got := store.size(); got != 200 {
		t.Fatalf("store holds %d entries, want 200", got)
	}
	later := start.Add(3 * proxyUDPResultTTL)
	store.Check(context.Background(), &connect.ProxySettings{Network: "tcp", Address: "192.0.2.3:1080"}, later)
	if got := store.size(); got > 5 {
		t.Fatalf("store still holds %d entries long after they were last asked about, want the stale ones dropped", got)
	}
}

func itoa(n int) string {
	digits := []byte{}
	if n == 0 {
		return "0"
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// testing.AllocsPerRun counts every allocation in the process, not just the
// function's, and divides as integers. A goroutine an earlier test leaked (the
// suite runs shuffled) allocating in the background while the measurement runs
// therefore reads as a per-call allocation on a path that allocates nothing. A
// real regression allocates on every attempt; noise does not, so the smallest
// of several attempts is the measurement.
func TestMinAllocsPerRunIgnoresBackgroundNoiseInSomeAttempts(t *testing.T) {
	var sink [][]byte
	attempt := 0
	got := minAllocsPerRun(5, 20, func() {
		// noisy for the first two attempts only, clean afterwards
		if attempt <= 2 {
			sink = append(sink, make([]byte, 64))
		}
	}, func() { attempt++ })
	if got != 0 {
		t.Fatalf("noise in two of five attempts must not raise the measurement, got %.1f", got)
	}
	_ = sink

	// an allocation on every attempt is a real regression and must show
	always := minAllocsPerRun(3, 20, func() {
		sink = append(sink, make([]byte, 64))
	}, func() {})
	if always < 1 {
		t.Fatalf("an allocation on every call must be reported, got %.1f", always)
	}
}
