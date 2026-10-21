package miner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/sdk"
)

const testSignMessage = "Approve URnetwork network wallet mapping\n{\"schema\":\"synthetic\"}"

// fakeSignApi stands in for the platform: it issues challenges and records
// what is submitted.
type fakeSignApi struct {
	mutex        sync.Mutex
	challenges   int
	submissions  []fakeSubmission
	submitError  *sdk.SnSetWalletError
	challengeErr error
	epoch        int64
}

type fakeSubmission struct{ Coldkey, Message, Signature string }

func (self *fakeSignApi) SnEpochSyncWithContext(context.Context) (*sdk.SnEpochResult, error) {
	return &sdk.SnEpochResult{Epoch: self.epoch}, nil
}

func (self *fakeSignApi) SnNetworkConsentChallengeSyncWithContext(_ context.Context, coldkey string) (*sdk.SnNetworkConsentChallengeResult, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.challengeErr != nil {
		return nil, self.challengeErr
	}
	self.challenges += 1
	return &sdk.SnNetworkConsentChallengeResult{
		Message: testSignMessage, ColdkeySs58: coldkey,
		FromEpoch: self.epoch + 1, ThroughEpoch: self.epoch + 1 + sdk.SnNetworkConsentEpochSpan,
		ExpiresAt: time.Now().Add(5*time.Minute).Unix() + int64(self.challenges),
	}, nil
}

func (self *fakeSignApi) SnNetworkConsentSubmitSyncWithContext(_ context.Context, coldkey, message, signature string) (*sdk.SnSetWalletResult, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.submissions = append(self.submissions, fakeSubmission{coldkey, message, signature})
	if self.submitError != nil {
		return &sdk.SnSetWalletResult{Error: self.submitError}, nil
	}
	return &sdk.SnSetWalletResult{MappingHash: "0x" + strings.Repeat("ab", 32), MappingGeneration: 1}, nil
}

func newSignUiForTest(t *testing.T, bind bool) (*snSignUi, *fakeSignApi, *httptest.Server) {
	t.Helper()
	fake := &fakeSignApi{epoch: 10}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ui := &snSignUi{
		ctx: ctx, api: fake, apiUrl: "https://" + defaultAPIHost, token: "test-token",
		deadline: time.Now().Add(snSignPageLifetime), done: make(chan struct{}),
	}
	if bind {
		ui.mutex.Lock()
		err := ui.bindWithLock(testAliceAddress)
		ui.mutex.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	page := httptest.NewServer(ui.handler())
	t.Cleanup(page.Close)
	return ui, fake, page
}

func signUiPost(t *testing.T, base, path, token, host string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	request, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("X-Session", token)
	}
	if host != "" {
		request.Host = host
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(response.Body).Decode(&out)
	return response.StatusCode, out
}

func validSignatureHex() string { return "0x" + strings.Repeat("ab", 64) }

func TestWalletSignPageServesTheMessageAndRefusesRebinding(t *testing.T) {
	_, _, page := newSignUiForTest(t, true)
	response, err := http.Get(page.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	text := string(body)
	for _, want := range []string{testAliceAddress, "Approve URnetwork network wallet mapping", "<svg", "This message expires in", "This page closes in"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the page lacks %q", want)
		}
	}
	if strings.Contains(text, "{{") {
		t.Fatal("an unfilled placeholder is left in the page")
	}
	if csp := response.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("no content security policy: %q", csp)
	}
	if other, _ := http.Get(page.URL + "/state"); other.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown path answered %d", other.StatusCode)
	}
	request, _ := http.NewRequest(http.MethodGet, page.URL+"/", nil)
	request.Host = "evil.example"
	rebound, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.StatusCode != http.StatusNotFound {
		t.Fatalf("a rebound host was served the page (%d)", rebound.StatusCode)
	}
}

func TestWalletSignLocalRequestChecksHostAndOrigin(t *testing.T) {
	cases := []struct {
		host, origin string
		want         bool
	}{
		{"127.0.0.1:8765", "", true},
		{"localhost:8765", "http://localhost:8765", true},
		{"127.0.0.1:8765", "https://evil.example", false},
		{"evil.example", "", false},
		{"127.0.0.1.evil.example:80", "", false},
	}
	for _, c := range cases {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Host = c.host
		if c.origin != "" {
			request.Header.Set("Origin", c.origin)
		}
		if got := snSignLocalRequest(request); got != c.want {
			t.Fatalf("host %q origin %q: got %v", c.host, c.origin, got)
		}
	}
}

func TestWalletSignPageSubmitsOnlyTheVerifiedMessage(t *testing.T) {
	_, fake, page := newSignUiForTest(t, true)
	sig := validSignatureHex()
	if status, _ := signUiPost(t, page.URL, "/signature", "wrong", "", map[string]string{"signature": sig}); status != http.StatusForbidden {
		t.Fatalf("a wrong session token answered %d", status)
	}
	if status, _ := signUiPost(t, page.URL, "/signature", "test-token", "evil.example", map[string]string{"signature": sig}); status != http.StatusForbidden {
		t.Fatalf("a foreign host answered %d", status)
	}
	if status, _ := signUiPost(t, page.URL, "/signature", "test-token", "", map[string]string{"signature": "0x12"}); status != http.StatusBadRequest {
		t.Fatalf("a short signature answered %d", status)
	}
	if len(fake.submissions) != 0 {
		t.Fatal("a refused request reached the platform")
	}
	status, out := signUiPost(t, page.URL, "/signature", "test-token", "", map[string]any{"signature": sig, "message": "Sign in to URnetwork"})
	if status != http.StatusOK || out["ok"] != true {
		t.Fatalf("the signature was not stored: %d %v", status, out)
	}
	if len(fake.submissions) != 1 || fake.submissions[0].Message != testSignMessage || fake.submissions[0].Coldkey != testAliceAddress {
		t.Fatalf("the platform did not receive the verified message: %+v", fake.submissions)
	}
	if status, _ := signUiPost(t, page.URL, "/signature", "test-token", "", map[string]string{"signature": sig}); status != http.StatusConflict {
		t.Fatalf("a second signature answered %d", status)
	}
}

func TestWalletSignPageReportsAServerRefusal(t *testing.T) {
	ui, fake, page := newSignUiForTest(t, true)
	fake.submitError = &sdk.SnSetWalletError{Code: sdk.SnErrorCodeSignatureMismatch, Message: "signature_mismatch: wrong account"}
	status, out := signUiPost(t, page.URL, "/signature", "test-token", "", map[string]string{"signature": validSignatureHex()})
	if status != http.StatusBadGateway || !strings.Contains(out["error"].(string), "signature_mismatch") {
		t.Fatalf("a refusal answered %d %v", status, out)
	}
	if ui.stored {
		t.Fatal("a refused consent was marked stored")
	}
}

func TestWalletSignPageRefreshesWithinLimitsAndStops(t *testing.T) {
	ui, fake, page := newSignUiForTest(t, true)
	before := fake.challenges
	ui.mutex.Lock()
	ui.lastRefresh = time.Now().Add(-time.Minute)
	ui.mutex.Unlock()
	if status, _ := signUiPost(t, page.URL, "/refresh", "test-token", "", nil); status != http.StatusOK {
		t.Fatalf("a refresh answered %d", status)
	}
	if fake.challenges != before+1 {
		t.Fatal("a refresh did not ask the platform for a new message")
	}
	if status, _ := signUiPost(t, page.URL, "/refresh", "test-token", "", nil); status != http.StatusTooManyRequests {
		t.Fatalf("an immediate second refresh answered %d", status)
	}
	ui.mutex.Lock()
	ui.deadline = time.Now().Add(-time.Second)
	ui.mutex.Unlock()
	if status, _ := signUiPost(t, page.URL, "/refresh", "test-token", "", nil); status != http.StatusGone {
		t.Fatalf("a stopped page answered %d", status)
	}
}

func TestWalletSignPageRefusesAnExpiredMessage(t *testing.T) {
	ui, fake, page := newSignUiForTest(t, true)
	ui.mutex.Lock()
	ui.challenge.ExpiresAt = time.Now().Add(-time.Second).Unix()
	ui.mutex.Unlock()
	if status, _ := signUiPost(t, page.URL, "/signature", "test-token", "", map[string]string{"signature": validSignatureHex()}); status != http.StatusGone {
		t.Fatalf("an expired message answered %d", status)
	}
	if len(fake.submissions) != 0 {
		t.Fatal("an expired message reached the platform")
	}
}

func TestWalletSignPageExtendIsBounded(t *testing.T) {
	ui, _, page := newSignUiForTest(t, true)
	ui.mutex.Lock()
	ui.deadline = time.Now().Add(30 * time.Second)
	ui.mutex.Unlock()
	for i := 0; i < snSignMaxExtensions; i++ {
		status, out := signUiPost(t, page.URL, "/extend", "test-token", "", nil)
		if status != http.StatusOK {
			t.Fatalf("extension %d answered %d", i+1, status)
		}
		if left := time.Until(time.Unix(int64(out["pageDeadline"].(float64)), 0)); left < snSignPageLifetime-5*time.Second {
			t.Fatalf("the page timer was reset to only %s", left)
		}
	}
	if status, _ := signUiPost(t, page.URL, "/extend", "test-token", "", nil); status != http.StatusTooManyRequests {
		t.Fatalf("an extension beyond the limit answered %d", status)
	}
}

func TestWalletSignPageChoosesTheAccountOnThePage(t *testing.T) {
	ui, fake, page := newSignUiForTest(t, false)
	if status, _ := signUiPost(t, page.URL, "/refresh", "test-token", "", nil); status != http.StatusConflict {
		t.Fatalf("a refresh before an account was chosen answered %d", status)
	}
	if status, _ := signUiPost(t, page.URL, "/signature", "test-token", "", map[string]string{"signature": validSignatureHex()}); status != http.StatusConflict {
		t.Fatalf("a signature before an account was chosen answered %d", status)
	}
	if status, _ := signUiPost(t, page.URL, "/select", "test-token", "", map[string]string{"address": "not-an-address"}); status != http.StatusBadRequest {
		t.Fatalf("a malformed address answered %d", status)
	}
	if fake.challenges != 0 || ui.coldkey != "" {
		t.Fatal("a statement was requested or an account kept before a valid account was chosen")
	}
	status, out := signUiPost(t, page.URL, "/select", "test-token", "", map[string]string{"address": testAliceAddress})
	if status != http.StatusOK || out["address"] != testAliceAddress {
		t.Fatalf("the account was not accepted: %d %v", status, out)
	}
	if fake.challenges != 1 {
		t.Fatal("choosing the account did not request its statement")
	}
	if status, _ := signUiPost(t, page.URL, "/select", "test-token", "", map[string]string{"address": "5FHneW46xGXgs5mUiveU4sbTyGBzmstUspZC92UhjJM694ty"}); status != http.StatusConflict {
		t.Fatalf("a second account answered %d", status)
	}
	if status, out := signUiPost(t, page.URL, "/signature", "test-token", "", map[string]string{"signature": validSignatureHex()}); status != http.StatusOK || out["ok"] != true {
		t.Fatalf("the chosen account could not sign: %d %v", status, out)
	}
}

func TestWalletSignBindFailureKeepsNoAccount(t *testing.T) {
	ui, fake, _ := newSignUiForTest(t, false)
	fake.challengeErr = errors.New("network_owner_only: refused")
	ui.mutex.Lock()
	defer ui.mutex.Unlock()
	if err := ui.bindWithLock(testAliceAddress); err == nil {
		t.Fatal("a refused challenge was accepted")
	}
	if ui.coldkey != "" || ui.challenge != nil {
		t.Fatal("a failed bind left an account behind")
	}
}

func TestWalletSignRefusesAForeignApiAndListensOnLoopbackOnly(t *testing.T) {
	t.Setenv("URNETWORK_WALLET_ALLOW_CUSTOM_API", "")
	for _, apiUrl := range []string{"http://api.bringyour.com", "https://evil.example", "https://api.bringyour.com.evil.example"} {
		if err := snCheckSigningApiUrl(apiUrl); err == nil {
			t.Fatalf("%q was accepted", apiUrl)
		}
	}
	if err := snCheckSigningApiUrl("https://" + defaultAPIHost); err != nil {
		t.Fatal(err)
	}
	err := snWalletSignUi(context.Background(), io.Discard, &fakeSignApi{}, "https://evil.example", testAliceAddress, 0, false)
	if err == nil || !strings.Contains(err.Error(), "refusing to sign") {
		t.Fatalf("a foreign api was not refused: %v", err)
	}
}

func TestWalletSignServesOnLoopbackAndStopsAtItsDeadline(t *testing.T) {
	fake := &fakeSignApi{epoch: 10}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ui := &snSignUi{ctx: ctx, api: fake, token: "t", deadline: time.Now().Add(1500 * time.Millisecond), done: make(chan struct{})}
	var out bytes.Buffer
	err := snServeSignUi(ctx, &out, ui, 0, false)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("the page did not stop at its deadline: %v", err)
	}
	if !strings.Contains(out.String(), "http://127.0.0.1:") {
		t.Fatalf("the page was not announced on loopback: %q", out.String())
	}
}

func TestWalletSignAndStatusUsageParses(t *testing.T) {
	opts, err := docopt.ParseArgs(mainUsage(), []string{"wallet", "sign", testAliceAddress, "--port=9000", "--no-open"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if sign, _ := opts.Bool("sign"); !sign {
		t.Fatal("sign not selected")
	}
	if noOpen, _ := opts.Bool("--no-open"); !noOpen {
		t.Fatal("--no-open lost")
	}
	if port, _ := opts.String("--port"); port != "9000" {
		t.Fatalf("port %q", port)
	}
	if cold, _ := opts.String("<coldkey_ss58>"); cold != testAliceAddress {
		t.Fatalf("coldkey %q", cold)
	}
	opts, err = docopt.ParseArgs(mainUsage(), []string{"wallet", "status"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := opts.Bool("status"); !status {
		t.Fatal("status not selected")
	}
	if hotkey, _ := opts.Bool("hotkey"); hotkey {
		t.Fatal("wallet status selected hotkey")
	}
	// wallet hotkey status stays the hotkey command
	opts, err = docopt.ParseArgs(mainUsage(), []string{"wallet", "hotkey", "status"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if hotkey, _ := opts.Bool("hotkey"); !hotkey {
		t.Fatal("wallet hotkey status lost its hotkey")
	}
}
