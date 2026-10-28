package miner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The probe dials an address once, with the first credentials listed for it.
// A later line for the same address with other credentials was never probed and
// must not inherit that passing grade or replace what was verified.
func TestFetchAndMergeProxyURLs_OnlyProbedCredentialsAreCached(t *testing.T) {
	withTempHome(t)
	allowLoopbackForTest(t)
	seedAllProbeDNS(t)

	ca := newTestCA(t)
	leaf := issueLeafForHost(t, ca, "127.0.0.1")
	withProbeTLSRoot(t, ca)
	addr, cleanup := listenSocks5ConnectOnceTLS(t, 0x00, &leaf)
	defer cleanup()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(addr + ":good:pw\n" + addr + ":bad:pw\n"))
	}))
	defer srv.Close()
	writeReviewProbeOverride(t, map[string]any{"sample_width": 4, "timeout_ms": 500})

	fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 100, "127.0.0.1", 443)

	state, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := state.Cache[addr]
	if !ok {
		t.Fatalf("probed address %s was not cached", addr)
	}
	if entry.User != "good" {
		t.Fatalf("cached user = %q, want the probed %q", entry.User, "good")
	}
}

// A rotation candidate that grades below the bar is not merged, so the old
// credentials stay in service. Its failing grade must not be written onto them.
func TestFetchAndMergeProxyURLs_FailedRotationKeepsOldEntryGrade(t *testing.T) {
	withTempHome(t)
	allowLoopbackForTest(t)
	seedAllProbeDNS(t)

	ca := newTestCA(t)
	leaf := issueLeafForHost(t, ca, "127.0.0.1")
	withProbeTLSRoot(t, ca)
	addr, cleanup := listenSocks5SmartTLS(t, map[string]bool{"127.0.0.1": true}, &leaf)
	defer cleanup()

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		addr: {User: "old", Password: "pw", ProbeOK: true, Score: 0.95, Graded: true},
	}}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(addr + ":new:pw\n"))
	}))
	defer srv.Close()
	writeReviewProbeOverride(t, map[string]any{"sample_width": 4, "timeout_ms": 500})

	fetchAndMergeProxyURLs(context.Background(), []string{srv.URL}, 100, "127.0.0.1", 443)

	state, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	entry := state.Cache[addr]
	if entry.User != "old" {
		t.Fatalf("a failed rotation replaced the working credentials: %+v", entry)
	}
	if !entry.ProbeOK || entry.Score != 0.95 {
		t.Fatalf("the failed candidate's grade was applied to the old entry: %+v", entry)
	}
}
