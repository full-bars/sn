package miner

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// withTempHome points the miner's state directory at a fresh temp directory
// (URNETWORK_STATE_DIR) and resets the process-wide proxy state a reload/trim
// test dirties, so a shuffled run cannot inherit another test's entries.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	prevHistory := globalProxyFailureHistory
	globalProxyFailureHistory = &proxyFailureHistory{failures: map[string]int{}}
	t.Cleanup(func() { globalProxyFailureHistory = prevHistory })
	resetTrimCapSeen()
	t.Cleanup(resetTrimCapSeen)
	// The stage-1 probe config resolver holds a parsed override snapshot for
	// probeConfigTTL; a test that wrote an override would otherwise leak it
	// into the next test's resolution (v33 resets it only around its own
	// override writes).
	resetProbeConfigCache()
	// The proxy_grades.json resolver caches its parsed snapshot against the
	// file mtime, so a test that wrote an override would leak it similarly.
	resetProxyGradesConfigCache()
	// The live health registry is process-wide too.
	resetProxyHealthReportStateForTest()
	resetGlobalControlStateForTest()
	t.Cleanup(resetGlobalControlStateForTest)
	lastReloadTriggerTime.Lock()
	lastReloadTriggerTime.ts = time.Time{}
	lastReloadTriggerTime.pending = false
	lastReloadTriggerTime.Unlock()
	prevDebounce := writeReloadTriggerDebounce
	writeReloadTriggerDebounce = 0
	t.Cleanup(func() { writeReloadTriggerDebounce = prevDebounce })
	return dir
}

// captureTlog captures everything the ported proxy pipeline logs while fn runs.
func captureTlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := proxyManagerLogWriter
	proxyManagerLogWriter = &buf
	defer func() { proxyManagerLogWriter = prev }()
	fn()
	return buf.String()
}

// allowLoopbackForTest lets the SSRF guard reach httptest servers, restoring
// the production setting afterwards.
func allowLoopbackForTest(t *testing.T) {
	t.Helper()
	prev := ssrfAllowLoopback.Load()
	ssrfAllowLoopback.Store(true)
	t.Cleanup(func() { ssrfAllowLoopback.Store(prev) })
}

// identityKey is the identity form (address, or address+user) tests use for
// credentialed proxies.
func identityKey(addr, user string) string {
	if user == "" {
		return addr
	}
	return (&connect.ProxySettings{Address: addr, Auth: &proxy.Auth{User: user}}).Key()
}

func authSettings(address, user, password string) *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: address, Auth: &proxy.Auth{User: user, Password: password}}
}

func noAuthSettings(address string) *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: address}
}

func almostEq(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

// allowLoopback is the package-wide seam the v33 suite sets in init; here each
// fetching test calls allowLoopbackForTest instead, because the miner suite has
// other users of the HTTP client.
var ssrfTestMu sync.Mutex

func TestWriteReadProxyURLState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy_url.json")

	s := &ProxyURLState{
		Sources: []string{"https://example.com/list.txt"},
		Cache: map[string]ProxyURLEntry{
			"1.2.3.4:1080": {},
			"5.6.7.8:1080": {User: "u", Password: "p"},
		},
	}

	if err := writeProxyURLStateTo(path, s); err != nil {
		t.Fatal(err)
	}

	got, err := readProxyURLStateFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sources) != 1 || got.Sources[0] != "https://example.com/list.txt" {
		t.Errorf("sources: got %v", got.Sources)
	}
	if len(got.Cache) != 2 {
		t.Errorf("cache: got %d entries, want 2", len(got.Cache))
	}
	if got.Cache["5.6.7.8:1080"].User != "u" {
		t.Errorf("cache entry user: got %q, want %q", got.Cache["5.6.7.8:1080"].User, "u")
	}
}

func TestReadProxyURLState_NotExist(t *testing.T) {
	s, err := readProxyURLStateFrom(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Cache == nil {
		t.Fatal("expected non-nil Cache map")
	}
}

func TestParseProxyURLLine(t *testing.T) {
	tests := []struct {
		name         string
		line         string
		wantAddr     string
		wantUser     string
		wantPassword string
		wantOK       bool
	}{
		{"plain host:port", "1.2.3.4:1080", "1.2.3.4:1080", "", "", true},
		{"host:port:user:pass", "1.2.3.4:1080:myuser:mypass", "1.2.3.4:1080", "myuser", "mypass", true},
		{"socks5 no creds", "socks5://1.2.3.4:1080", "1.2.3.4:1080", "", "", true},
		{"socks5 with creds", "socks5://myuser:mypass@1.2.3.4:1080", "1.2.3.4:1080", "myuser", "mypass", true},
		{"SOCKS5 case-insensitive scheme", "SOCKS5://1.2.3.4:1080", "1.2.3.4:1080", "", "", true},
		{"blank line", "", "", "", "", false},
		{"comment line", "# 1.2.3.4:1080", "", "", "", false},
		{"unsupported scheme", "http://1.2.3.4:1080", "", "", "", false},
		{"whitespace padded", "  1.2.3.4:1080  ", "1.2.3.4:1080", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, user, password, ok := parseProxyURLLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if addr != tt.wantAddr {
				t.Errorf("address: got %q, want %q", addr, tt.wantAddr)
			}
			if user != tt.wantUser {
				t.Errorf("user: got %q, want %q", user, tt.wantUser)
			}
			if password != tt.wantPassword {
				t.Errorf("password: got %q, want %q", password, tt.wantPassword)
			}
		})
	}
}

func TestFetchProxyURLLines_Success(t *testing.T) {
	allowLoopbackForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("1.2.3.4:1080\n5.6.7.8:1080\n"))
	}))
	defer srv.Close()

	lines, err := fetchProxyURLLines(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "1.2.3.4:1080" || lines[1] != "5.6.7.8:1080" {
		t.Fatalf("got %v", lines)
	}
}

func TestFetchProxyURLLines_NonOKStatus(t *testing.T) {
	allowLoopbackForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := fetchProxyURLLines(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestFetchProxyURLLines_ConnectionErrorDoesNotLeakTheURL(t *testing.T) {
	// A *net/url.Error from http.Client.Do embeds the full request URL in its
	// message, credentials included. The log, resolution reason and operator
	// warning all surface the fetch error, so the URL must not survive into it.
	const secret = "supersecretusertoken"
	// A credentialed URL to an unrouteable port: the dial fails, and Go wraps
	// that failure in a *url.Error whose message would name the URL.
	url := "http://user:" + secret + "@127.0.0.1:1/list"
	_, err := fetchProxyURLLines(context.Background(), url)
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "user:") {
		t.Fatalf("fetch error leaked the credential-bearing URL: %q", err.Error())
	}
	if strings.Contains(err.Error(), "127.0.0.1:1/list") {
		t.Fatalf("fetch error leaked the request URL: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "fetch:") {
		t.Fatalf("fetch error lost its context prefix: %q", err.Error())
	}
}

func TestFetchProxyURLLines_EmptyBody(t *testing.T) {
	allowLoopbackForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	_, err := fetchProxyURLLines(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestFetchProxyURLLines_BodyTruncatedAtLimit(t *testing.T) {
	allowLoopbackForTest(t)
	huge := strings.Repeat("a", maxProxyURLFetchBytes+1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(huge))
	}))
	defer srv.Close()

	lines, err := fetchProxyURLLines(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, l := range lines {
		total += len(l)
	}
	if total > maxProxyURLFetchBytes {
		t.Fatalf("body not truncated: got %d bytes, want <= %d", total, maxProxyURLFetchBytes)
	}
}

func TestMergeProxyURLEntries_AddsNewSkipsExisting(t *testing.T) {
	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {},
	}}
	added := mergeProxyURLEntries(state, []string{
		"1.2.3.4:1080", // already present, not re-added
		"5.6.7.8:1080:user:pass",
		"# comment, skipped",
	}, 0, 0, nil, nil)
	if added != 1 {
		t.Fatalf("added: got %d, want 1", added)
	}
	if len(state.Cache) != 2 {
		t.Fatalf("cache size: got %d, want 2", len(state.Cache))
	}
	if state.Cache["5.6.7.8:1080"].User != "user" {
		t.Errorf("expected creds preserved, got %+v", state.Cache["5.6.7.8:1080"])
	}
}

func TestMergeProxyURLEntries_RespectsMaxTotal(t *testing.T) {
	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {},
	}}
	added := mergeProxyURLEntries(state, []string{
		"5.6.7.8:1080",
		"9.9.9.9:1080",
	}, 0, 2, nil, nil)
	if added != 1 {
		t.Fatalf("added: got %d, want 1 (cap of 2 total, 1 already present)", added)
	}
	if len(state.Cache) != 2 {
		t.Fatalf("cache size: got %d, want 2", len(state.Cache))
	}
}

func TestMergeProxyURLEntries_SetsProbeOKForApiVerifiedEntries(t *testing.T) {
	state := &ProxyURLState{Cache: map[string]ProxyURLEntry{}}
	// apiOKCount=1: only the first line (api-verified) should get ProbeOK=true;
	// the rest (socks5-only) must stay ProbeOK=false so the reaper retries them.
	added := mergeProxyURLEntries(state, []string{
		"1.2.3.4:1080",
		"5.6.7.8:1080",
	}, 1, 0, nil, nil)
	if added != 2 {
		t.Fatalf("added: got %d, want 2", added)
	}
	if !state.Cache["1.2.3.4:1080"].ProbeOK {
		t.Error("api-verified entry should have ProbeOK=true")
	}
	if state.Cache["5.6.7.8:1080"].ProbeOK {
		t.Error("socks5-only entry should have ProbeOK=false")
	}
}

func TestMergeProxyURLCache_PrimarySourceWins(t *testing.T) {
	desiredSet := map[string]*connect.ProxySettings{
		"1.2.3.4:1080": {Network: "tcp", Address: "1.2.3.4:1080"},
	}
	sourceOf := map[string]string{"1.2.3.4:1080": "file"}
	urlState := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {User: "should-be-ignored"},
		"5.6.7.8:1080": {User: "u", Password: "p"},
	}}

	mergeProxyURLCache(desiredSet, sourceOf, urlState)

	if sourceOf["1.2.3.4:1080"] != "file" {
		t.Errorf("existing entry's source was overwritten: got %q", sourceOf["1.2.3.4:1080"])
	}
	newKey := (&connect.ProxySettings{Address: "5.6.7.8:1080", Auth: &proxy.Auth{User: "u"}}).Key()
	if sourceOf[newKey] != "url" {
		t.Errorf("new entry not tagged url: got %q", sourceOf[newKey])
	}
	settings, ok := desiredSet[newKey]
	if !ok {
		t.Fatal("expected new address merged into desiredSet")
	}
	if settings.Auth == nil || settings.Auth.User != "u" || settings.Auth.Password != "p" {
		t.Errorf("expected auth u/p, got %+v", settings.Auth)
	}
}

func TestWriteReadProxyURLState_RoundTrip_Blacklist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy_url.json")

	ts := time.Now().UTC().Truncate(time.Second)
	s := &ProxyURLState{
		Cache:     map[string]ProxyURLEntry{},
		Blacklist: map[string]time.Time{"1.2.3.4:1080": ts},
	}
	if err := writeProxyURLStateTo(path, s); err != nil {
		t.Fatal(err)
	}

	got, err := readProxyURLStateFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	gotTs, ok := got.Blacklist["1.2.3.4:1080"]
	if !ok {
		t.Fatal("expected blacklist entry to round-trip")
	}
	if !gotTs.Equal(ts) {
		t.Errorf("blacklist timestamp: got %v, want %v", gotTs, ts)
	}
}

func TestMergeProxyURLEntries_SkipsBlacklisted(t *testing.T) {
	state := &ProxyURLState{
		Cache:     map[string]ProxyURLEntry{},
		Blacklist: map[string]time.Time{"1.2.3.4:1080": time.Now()},
	}
	added := mergeProxyURLEntries(state, []string{
		"1.2.3.4:1080", // blacklisted, must be skipped even though not yet cached
		"5.6.7.8:1080",
	}, 0, 0, nil, nil)
	if added != 1 {
		t.Fatalf("added: got %d, want 1", added)
	}
	if _, ok := state.Cache["1.2.3.4:1080"]; ok {
		t.Error("blacklisted address must not be added to cache")
	}
	if _, ok := state.Cache["5.6.7.8:1080"]; !ok {
		t.Error("non-blacklisted address should still be added")
	}
}

func TestMergeProxyURLCache_NilStateIsNoop(t *testing.T) {
	desiredSet := map[string]*connect.ProxySettings{}
	sourceOf := map[string]string{}
	mergeProxyURLCache(desiredSet, sourceOf, nil)
	if len(desiredSet) != 0 {
		t.Fatalf("expected no-op, got %v", desiredSet)
	}
}

func TestMergeProxyURLEntriesSkipsExcludePatterns(t *testing.T) {
	state := &ProxyURLState{
		Cache:           map[string]ProxyURLEntry{},
		ExcludePatterns: []string{"dc.decodo.com"},
	}
	lines := []string{
		"dc.decodo.com:8001",
		"DC.DECODO.COM:8002:user:pass",
		"gate.smartproxy.com:7000",
	}
	added := mergeProxyURLEntries(state, lines, 0, 0, nil, nil)
	if added != 1 {
		t.Fatalf("added = %d, want 1 (only the non-excluded proxy)", added)
	}
	if _, ok := state.Cache["gate.smartproxy.com:7000"]; !ok {
		t.Errorf("expected gate.smartproxy.com:7000 in cache")
	}
	for addr := range state.Cache {
		if matchProxyHost("dc.decodo.com", addr) {
			t.Errorf("excluded address %q was cached", addr)
		}
	}
}

func TestAddExcludePattern(t *testing.T) {
	state := &ProxyURLState{}
	if !addExcludePattern(state, "dc.decodo.com") {
		t.Fatal("first add should return true")
	}
	if addExcludePattern(state, "DC.Decodo.Com") {
		t.Fatal("case-insensitive duplicate should return false")
	}
	if len(state.ExcludePatterns) != 1 {
		t.Fatalf("ExcludePatterns = %v, want 1 entry", state.ExcludePatterns)
	}
}

// ---- last_graded honesty (v33 proxy_url_lastgraded_test.go) ----

func TestApplyProxyGradeToEntryStampsLastGraded(t *testing.T) {
	gradedAt := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	entry := ProxyURLEntry{}
	g := proxyURLGrade{
		Decidable:  true,
		Socks5Only: false,
		Score:      0.92,
		Failed:     []string{"a.com"},
	}

	applyProxyGradeToEntry(&entry, g, gradedAt)

	if !entry.Graded {
		t.Fatal("expected entry graded after a decidable pass")
	}
	if entry.Score != 0.92 {
		t.Errorf("Score = %f, want 0.92", entry.Score)
	}
	if !entry.LastGraded.Equal(gradedAt) {
		t.Errorf("LastGraded = %v, want %v", entry.LastGraded, gradedAt)
	}
}

func TestApplyProxyGradeToEntrySocks5OnlyDoesNotStamp(t *testing.T) {
	entry := ProxyURLEntry{LastGraded: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	g := proxyURLGrade{Decidable: false, Socks5Only: true}

	applyProxyGradeToEntry(&entry, g, time.Now())

	if entry.Graded {
		t.Fatal("socks5-only probe must not set Graded")
	}
	if !entry.LastGraded.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("LastGraded advanced on a non-grade probe: %v", entry.LastGraded)
	}
}

func TestTrimGradeForURLLastGradedFromFieldNotLastProbe(t *testing.T) {
	gradedAt := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {
			Score: 0.61, Graded: true,
			LastProbe:  time.Now(),
			LastGraded: gradedAt,
		},
	}}

	score, ok := trimGradeFor("1.2.3.4:1080", &ProxyState{}, url)
	if !ok || score != 0.61 {
		t.Fatalf("expected the persisted URL grade 0.61, got %v ok=%v", score, ok)
	}
}

// ---- fetch redaction (v33 proxy_url_redaction_test.go) ----

func TestFetchProxyURLLinesBuildRequestErrorDoesNotEchoTheURL(t *testing.T) {
	_, err := fetchProxyURLLines(context.Background(), "http://127.0.0.1:1/p%zz.txt?token=SECRET")
	if err == nil {
		t.Fatal("expected an error for a malformed URL")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error text leaks the URL's token: %q", err.Error())
	}
}

func TestFetchCycleDoesNotLogTheRawURLOfAMalformedSource(t *testing.T) {
	withTempHome(t)
	out := captureTlog(t, func() {
		fetchAndMergeProxyURLs(context.Background(), []string{"http://127.0.0.1:1/p%zz.txt?token=SECRET"}, 0, "127.0.0.1", 1)
	})
	if strings.Contains(out, "SECRET") {
		t.Fatalf("the cycle's log output leaks the URL's token:\n%s", out)
	}
}

func TestSSRFVerifyURLHostErrorsDoNotEchoTheURL(t *testing.T) {
	ssrfTestMu.Lock()
	defer ssrfTestMu.Unlock()
	for _, raw := range []string{
		"https:?token=SECRET",
		"http://exa mple.com/?token=SECRET",
	} {
		err := ssrfVerifyURLHost(raw)
		if err == nil {
			t.Fatalf("ssrfVerifyURLHost(%q) accepted a URL it should refuse", raw)
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("ssrfVerifyURLHost(%q) leaks the token: %q", raw, err.Error())
		}
	}
}

func TestFetchRedirectToAHostlessURLDoesNotEchoItsToken(t *testing.T) {
	allowLoopbackForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https:?token=REMOTE_ONLY")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	_, err := fetchProxyURLLines(context.Background(), srv.URL+"/p.txt")
	if err == nil {
		t.Fatal("expected the redirect to be refused")
	}
	if strings.Contains(err.Error(), "REMOTE_ONLY") {
		t.Fatalf("the fetch error leaks the redirect's token: %q", err.Error())
	}
}

func TestFetchFailureResolutionReasonUsesTheRedactedLabel(t *testing.T) {
	withTempHome(t)
	captureTlog(t, func() {
		fetchAndMergeProxyURLs(context.Background(), []string{"http://127.0.0.1:1/p%zz.txt?token=SECRET"}, 0, "127.0.0.1", 1)
	})
	reason := getProxyResolutionReason()
	if reason == "" {
		t.Fatal("a failed fetch left no resolution reason")
	}
	if strings.Contains(reason, "SECRET") {
		t.Fatalf("the resolution reason leaks the URL's token: %q", reason)
	}
}
