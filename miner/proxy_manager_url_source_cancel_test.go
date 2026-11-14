package miner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// lastSourceTransport serves one source whose body cancels the cycle when it
// is read to the end. The response is complete, so the fetch succeeds and the
// cancel is first seen after the fetch, which is the case the last source hits.
type lastSourceTransport struct{ cancel context.CancelCauseFunc }

func (tr *lastSourceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := &cancelAtEOFBody{Reader: strings.NewReader("192.0.2.1:1\n"), cancel: tr.cancel}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, Request: req}, nil
}

type cancelAtEOFBody struct {
	*strings.Reader
	cancel context.CancelCauseFunc
}

func (b *cancelAtEOFBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		b.cancel(context.Canceled)
	}
	return n, err
}

func (b *cancelAtEOFBody) Close() error { return nil }

// A shutdown that lands while the LAST source is read or probed has no later
// iteration to notice it. The cycle must still say it was cancelled, and must
// still not report a source failure.
func TestFetchAndMergeProxyURLs_ShutdownDuringLastSourceIsLogged(t *testing.T) {
	withTempHome(t)
	warnings := captureSourceWarnings(t)

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	prev := proxyURLHTTPClient
	proxyURLHTTPClient = &http.Client{Transport: &lastSourceTransport{cancel: cancel}}
	t.Cleanup(func() { proxyURLHTTPClient = prev })

	out := captureTlog(t, func() {
		fetchAndMergeProxyURLs(ctx, []string{"http://source.invalid/list"}, 0, "", 0)
	})
	if !strings.Contains(out, "fetch cycle cancelled after 1 of 1 sources") {
		t.Errorf("the cancelled cycle was not logged:\n%s", out)
	}
	if len(*warnings) != 0 {
		t.Errorf("shutdown reported as source failures: %q", *warnings)
	}
}
