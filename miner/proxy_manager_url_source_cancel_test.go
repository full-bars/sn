package miner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// captureSourceWarnings swaps the operator warning hook for a recorder and
// resets the resolution status, restoring both when the test ends.
func captureSourceWarnings(t *testing.T) *[]string {
	t.Helper()
	var warnings []string
	prevWarn := warnProxySourceFailure
	warnProxySourceFailure = func(label, reason string) { warnings = append(warnings, label+": "+reason) }
	prevStatus, prevReason := proxyResolutionStatus.Load(), getProxyResolutionReason()
	proxyResolutionStatus.Store(proxyResolutionOK)
	t.Cleanup(func() {
		warnProxySourceFailure = prevWarn
		proxyResolutionStatus.Store(prevStatus)
		proxyResolutionReason.Store(prevReason)
	})
	return &warnings
}

// A process shutdown (SIGTERM cancels the root signal context) that lands
// mid-cycle must stop the cycle. It must not turn every remaining source into
// a "fetch failed ... terminated signal received" failure, flip the resolution
// status to failed, or page the operator with a "retrying" warning.
func TestFetchAndMergeProxyURLs_ShutdownMidCycleIsNotSourceFailure(t *testing.T) {
	withTempHome(t)
	allowLoopbackForTest(t)
	warnings := captureSourceWarnings(t)

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	var laterHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The shutdown arrives while the first source is being served.
		cancel(context.Canceled)
		w.Write([]byte("127.0.0.1:1\n"))
	}))
	defer first.Close()
	later := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		laterHits.Add(1)
		w.Write([]byte("127.0.0.1:2\n"))
	}))
	defer later.Close()

	urls := []string{first.URL, later.URL + "/a", later.URL + "/b", later.URL + "/c"}
	fetchAndMergeProxyURLs(ctx, urls, 0, "", 0)

	if n := laterHits.Load(); n != 0 {
		t.Errorf("sources after the shutdown were still fetched: %d requests", n)
	}
	if len(*warnings) != 0 {
		t.Errorf("shutdown reported as source failures: %q", *warnings)
	}
	if got := proxyResolutionStatus.Load(); got == proxyResolutionFailed {
		t.Errorf("resolution status flipped to failed by a shutdown: %q", getProxyResolutionReason())
	}
}

// Healthy sources, however many, are all fetched in one cycle with no failure
// reported; one source's error never cancels its siblings.
func TestFetchAndMergeProxyURLs_ManySourcesAllFetched(t *testing.T) {
	withTempHome(t)
	allowLoopbackForTest(t)
	warnings := captureSourceWarnings(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/broken" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte("127.0.0.1:1\n"))
	}))
	defer srv.Close()

	urls := []string{srv.URL + "/1", srv.URL + "/broken", srv.URL + "/2", srv.URL + "/3",
		srv.URL + "/4", srv.URL + "/5", srv.URL + "/6", srv.URL + "/7"}
	fetchAndMergeProxyURLs(context.Background(), urls, 0, "", 0)

	if got := hits.Load(); got != int32(len(urls)) {
		t.Errorf("fetched %d of %d sources", got, len(urls))
	}
	for _, w := range *warnings {
		if !containsAll(w, "unexpected status 500") && !containsAll(w, "all unparseable or dead") {
			t.Errorf("unexpected warning: %s", w)
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
