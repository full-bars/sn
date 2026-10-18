package miner

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// providerBringUpLogBuffer captures the bring-up lines for one test.
type providerBringUpLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (self *providerBringUpLogBuffer) Write(raw []byte) (int, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.b.Write(raw)
}

func (self *providerBringUpLogBuffer) String() string {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.b.String()
}

func captureProviderBringUpLog(t *testing.T) *providerBringUpLogBuffer {
	t.Helper()
	old := proxyManagerLogWriter
	buffer := &providerBringUpLogBuffer{}
	proxyManagerLogWriter = buffer
	t.Cleanup(func() { proxyManagerLogWriter = old })
	return buffer
}

func TestProviderBringUpNarratesAuthOutcomes(t *testing.T) {
	buffer := captureProviderBringUpLog(t)
	tracker := newProviderBringUpTracker(3)
	tracker.start()
	defer tracker.stop()

	directID := connect.NewId()
	proxyID := connect.NewId()
	tracker.authReused(0, directID, "direct")
	tracker.authNew(1, proxyID, "proxy:10.0.0.1:1080")
	tracker.authFailed(2, "proxy:10.0.0.2:1080", errors.New("synthetic refused"), "in 15m")
	// A worker that fails after its auth line must not repeat the reason.
	tracker.workerFailed(2)

	text := buffer.String()
	for _, want := range []string{
		"providers: 0 running, 3 pending, 0 failed",
		"[reuse] client_id: " + directID.String() + " (provider 0, slot direct)",
		"[new] client_id: " + proxyID.String() + " (provider 1, slot proxy:10.0.0.1:1080)",
		"[auth] provider 2 failed: synthetic refused (slot proxy:10.0.0.2:1080, retry in 15m)",
		"providers: 2 running, 0 pending, 1 failed",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("bring-up lines miss %q:\n%s", want, text)
		}
	}
	if count := strings.Count(text, "[auth] provider 2 failed"); count != 1 {
		t.Fatalf("the failed outcome was narrated %d times:\n%s", count, text)
	}
}

func TestProviderBringUpHeartbeatCadenceAndFinalTally(t *testing.T) {
	oldInterval := providerBringUpHeartbeatInterval
	providerBringUpHeartbeatInterval = 20 * time.Millisecond
	defer func() { providerBringUpHeartbeatInterval = oldInterval }()

	buffer := captureProviderBringUpLog(t)
	tracker := newProviderBringUpTracker(2)
	tracker.start()
	defer tracker.stop()

	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(buffer.String(), "providers:") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if count := strings.Count(buffer.String(), "providers:"); count < 2 {
		t.Fatalf("heartbeat did not repeat: %d lines\n%s", count, buffer.String())
	}

	tracker.authReused(0, connect.NewId(), "direct")
	tracker.authFailed(1, "proxy:10.0.0.9:1080", errors.New("synthetic refused"), "on the next start")
	text := buffer.String()
	if !strings.Contains(text, "providers: 1 running, 0 pending, 1 failed") {
		t.Fatalf("final tally missing:\n%s", text)
	}
	// The heartbeat stops once nothing is pending: no further lines after the
	// final tally.
	resolved := strings.Count(text, "providers:")
	time.Sleep(80 * time.Millisecond)
	if count := strings.Count(buffer.String(), "providers:"); count != resolved {
		t.Fatalf("heartbeat continued after resolution: %d -> %d\n%s", resolved, count, buffer.String())
	}
}

func TestProviderBringUpStartStopIdempotent(t *testing.T) {
	captureProviderBringUpLog(t)
	tracker := newProviderBringUpTracker(0)
	tracker.start()
	tracker.start()
	tracker.stop()
	tracker.stop()
	tracker.workerFailed(0)
	tracker.authReused(0, connect.NewId(), "direct")
	tracker.stop()
}

func TestProviderAuthRetryToken(t *testing.T) {
	cases := []struct {
		name string
		info providerAuthGiveUpInfo
		want string
	}{
		{"below the ceiling", providerAuthGiveUpInfo{}, "on the next start"},
		{"backoff recorded", providerAuthGiveUpInfo{giveUp: 1, retryIn: 15 * time.Minute}, "in 15m"},
		{"evicted", providerAuthGiveUpInfo{giveUp: 4, evicted: true}, "never (evicted from the URL cache)"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := providerAuthRetryToken(testCase.info); got != testCase.want {
				t.Fatalf("retry token = %q, want %q", got, testCase.want)
			}
		})
	}
}
