package provider

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// TestRunURLProxyReaperOnce_LiveCredentialedProxyNotFailed: the reaper's
// isLive escape looked the bare cache address up in the identity-keyed health
// map, so a credentialed proxy that was up and serving read as not live, and
// a failed probe counted toward its blacklist.
func TestRunURLProxyReaperOnce_LiveCredentialedProxyNotFailed(t *testing.T) {
	withTempHome(t)
	ResetProxyHealthForTesting()
	t.Cleanup(ResetProxyHealthForTesting)

	// A closed port: the probe fails fast as dead.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		addr: {User: "alice", Password: "pw", ProbeOK: false, ProbeFails: 1, LastProbe: time.Now().Add(-24 * time.Hour)},
	}}); err != nil {
		t.Fatal(err)
	}

	key := (&connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: "alice", Password: "pw"}}).Key()
	RegisterProxy(3, addr, key)
	markProxyUp(3)
	if h := ProxyHealthByKey()[key]; h.Health != "up" {
		t.Fatalf("setup: health for %q = %q, want up", key, h.Health)
	}

	runURLProxyReaperOnce(context.Background(), "1.2.3.4", 443)

	got, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := got.Cache[addr]
	if !ok {
		t.Fatal("live credentialed proxy was removed from the cache")
	}
	if !entry.ProbeOK || entry.ProbeFails != 0 {
		t.Fatalf("live credentialed proxy counted as failed: ProbeOK=%v ProbeFails=%d", entry.ProbeOK, entry.ProbeFails)
	}
}
