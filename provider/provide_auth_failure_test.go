package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/connect"
)

// TestProvideHandleAuthFailure_CancelledProxyNotAGiveUp: a URL proxy removed,
// rotated or trimmed mid-auth often surfaces a dial or timeout error rather
// than context.Canceled. Only the error was checked, so that cancellation was
// recorded as a give-up and could eventually evict a healthy proxy.
func TestProvideHandleAuthFailure_CancelledProxyNotAGiveUp(t *testing.T) {
	withTempHome(t)
	settings := &connect.ProxySettings{Network: "tcp", Address: "9.9.9.9:1080"}
	st := &provideState{proxyCancelMap: map[string]context.CancelFunc{}}

	proxyCtx, cancel := context.WithCancel(context.Background())
	cancel()
	provideHandleAuthFailure(st, proxyCtx, settings, false, true, errors.New("dial tcp 9.9.9.9:1080: i/o timeout"))

	if n := globalProxyFailureHistory.GiveUpCount(settings.Key()); n != 0 {
		t.Fatalf("give-up recorded for a cancelled proxy: count=%d", n)
	}

	// A live context with the same error is still a give-up.
	provideHandleAuthFailure(st, context.Background(), settings, false, true, errors.New("dial tcp 9.9.9.9:1080: i/o timeout"))
	if n := globalProxyFailureHistory.GiveUpCount(settings.Key()); n != 1 {
		t.Fatalf("give-up count for a live proxy = %d, want 1", n)
	}
}
