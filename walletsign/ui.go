package walletsign

// ui.go -- `provider wallet sign`: one local page where a browser
// wallet extension signs the network wallet consent.
//
// The page is served on 127.0.0.1 only, for a bounded time, with a visible
// countdown. It hands the server's statement to the extension and receives the
// signature (a public value). It never sees a seed phrase or a private key; the
// extension signs. The challenge comes from the sdk's network consent call,
// which checks the statement against the requested coldkey, range and network
// before the page shows it. The message that is stored is the one the sdk
// returned, never one the page sends back.

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/sdk"

	"github.com/urfoundation/sn/ss58"
)

//go:embed wallet_sign_page.html
var snSignPageHtml string

// the logo is the URnetwork wordmark from the URnetwork web repository
// (MPL-2.0), inlined so the page needs no network and no font or image
// downloads
//
//go:embed wallet_sign_logo.svg
var snSignLogoSvg string

const (
	// how long the page is served; the statement itself lives five minutes and
	// can be renewed from the page
	snSignPageLifetime = 15 * time.Minute
	// the server keeps a few unused challenges per network; do not flood it
	snSignMaxRefreshes   = 8
	snSignRefreshSpacing = 3 * time.Second
	// time the result stays on screen after a successful store
	snSignLingerAfterDone = 20 * time.Second
	// the page can be given another full lifetime a few times; it is still
	// bounded, and local-only the whole while
	snSignMaxExtensions = 3
	snSignDefaultPort   = 8765
)

// snSignApi is the part of the sdk api the page uses, so tests can stand in a
// fake platform.
type snSignApi interface {
	SnEpochSyncWithContext(context.Context) (*sdk.SnEpochResult, error)
	SnNetworkConsentChallengeSyncWithContext(context.Context, string) (*sdk.SnNetworkConsentChallengeResult, error)
	SnNetworkConsentSubmitSyncWithContext(context.Context, string, string, string) (*sdk.SnSetWalletResult, error)
}

type snSignUi struct {
	ctx      context.Context
	api      snSignApi
	apiUrl   string
	coldkey  string
	token    string
	deadline time.Time
	// preview marks example data (never set by the command)
	preview bool

	mutex        sync.Mutex
	challenge    *sdk.SnNetworkConsentChallengeResult
	currentEpoch int64
	refreshes    int
	extensions   int
	lastRefresh  time.Time
	stored       bool

	done chan struct{}
}

func snSignToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// snPlain strips control characters from text that came from a server or a
// flag before it is printed, so a hostile endpoint cannot inject terminal
// escape sequences.
func snPlain(text string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, text)
}

// snCheckSigningApiUrl refuses to have the coldkey sign a challenge from any
// endpoint but the main network's, unless the operator opts in for a private
// test API.
func snCheckSigningApiUrl(apiUrl string) error {
	if os.Getenv("URNETWORK_WALLET_ALLOW_CUSTOM_API") == "1" {
		return nil
	}
	u, err := url.Parse(apiUrl)
	if err != nil || u.Scheme != "https" || u.Hostname() != defaultAPIHost {
		return fmt.Errorf("refusing to sign a challenge from %q: the coldkey signs only challenges from https://%s (set URNETWORK_WALLET_ALLOW_CUSTOM_API=1 only for a private test API)", snPlain(apiUrl), defaultAPIHost)
	}
	return nil
}

// bindWithLock binds the run to the account the wallet extension offered (or
// the one named on the command line) and gets the first statement. The address
// is checked as a Bittensor ss58 address before anything is requested.
func (self *snSignUi) bindWithLock(address string) error {
	if self.challenge != nil && self.coldkey == address {
		return nil
	}
	if _, err := ss58.DecodeWithPrefix(address, ss58.BittensorPrefix); err != nil {
		return fmt.Errorf("invalid ss58 coldkey %q: %s", snPlain(address), err)
	}
	previous := self.coldkey
	self.coldkey = address
	if err := self.newMessageWithLock(); err != nil {
		self.coldkey = previous
		return err
	}
	return nil
}

// newMessageWithLock asks the server for a fresh statement; the sdk verifies it
// names the requested coldkey, range and network.
func (self *snSignUi) newMessageWithLock() error {
	challenge, err := self.api.SnNetworkConsentChallengeSyncWithContext(self.ctx, self.coldkey)
	if err != nil {
		return err
	}
	if challenge == nil || challenge.Message == "" {
		return errors.New("the platform returned no consent message")
	}
	self.challenge = challenge
	if epoch, err := self.api.SnEpochSyncWithContext(self.ctx); err == nil && epoch != nil {
		self.currentEpoch = epoch.Epoch
	}
	return nil
}

type snSignState struct {
	Message   string `json:"message"`
	Hex       string `json:"hex"`
	ExpiresAt int64  `json:"expiresAt"`
	From      int64  `json:"from"`
	Through   int64  `json:"through"`
	// the epoch the network is in now, so the page can say when the consent starts
	CurrentEpoch int64 `json:"currentEpoch"`
}

func (self *snSignUi) stateWithLock() snSignState {
	if self.challenge == nil {
		return snSignState{}
	}
	return snSignState{
		Message:      self.challenge.Message,
		Hex:          "0x" + hex.EncodeToString([]byte(self.challenge.Message)),
		ExpiresAt:    self.challenge.ExpiresAt,
		From:         self.challenge.FromEpoch,
		Through:      self.challenge.ThroughEpoch,
		CurrentEpoch: self.currentEpoch,
	}
}

func snSignWriteJson(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func snSignError(w http.ResponseWriter, status int, message string) {
	snSignWriteJson(w, status, map[string]string{"error": snPlain(message)})
}

// snSignLocalRequest reports whether the request names this machine: a page
// that a remote site loaded through DNS rebinding carries another Host or
// Origin.
func snSignLocalRequest(r *http.Request) bool {
	hostOk := func(hostport string) bool {
		host, _, err := net.SplitHostPort(hostport)
		if err != nil {
			host = hostport
		}
		return host == "127.0.0.1" || host == "localhost"
	}
	if !hostOk(r.Host) {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
		if !hostOk(trimmed) {
			return false
		}
	}
	return true
}

func (self *snSignUi) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" || r.Method != http.MethodGet || !snSignLocalRequest(r) {
			http.NotFound(w, r)
			return
		}
		self.mutex.Lock()
		state := self.stateWithLock()
		deadline, extensions, coldkey := self.deadline, self.extensions, self.coldkey
		self.mutex.Unlock()
		boot := map[string]any{
			"preview":        self.preview,
			"address":        coldkey,
			"token":          self.token,
			"message":        state.Message,
			"hex":            state.Hex,
			"expiresAt":      state.ExpiresAt,
			"from":           state.From,
			"through":        state.Through,
			"currentEpoch":   state.CurrentEpoch,
			"pageDeadline":   deadline.Unix(),
			"extensionsLeft": snSignMaxExtensions - extensions,
		}
		bootJson, _ := json.Marshal(boot)
		logo := snSignLogoSvg
		if i := strings.Index(logo, "<svg"); i > 0 {
			logo = logo[i:]
		}
		page := strings.Replace(snSignPageHtml, "{{LOGO}}", logo, 1)
		// "</" cannot end the script element
		page = strings.Replace(page, "{{BOOT}}", strings.ReplaceAll(string(bootJson), "</", "<\\/"), 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:")
		_, _ = io.WriteString(w, page)
	})
	post := func(handle func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || !snSignLocalRequest(r) || r.Header.Get("X-Session") != self.token {
				snSignError(w, http.StatusForbidden, "forbidden")
				return
			}
			self.mutex.Lock()
			stopped := time.Now().After(self.deadline)
			self.mutex.Unlock()
			if stopped {
				snSignError(w, http.StatusGone, "this page has stopped; run the command again")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 8192)
			handle(w, r)
		}
	}
	mux.HandleFunc("/select", post(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Address string `json:"address"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			snSignError(w, http.StatusBadRequest, "bad request")
			return
		}
		address := strings.TrimSpace(body.Address)
		self.mutex.Lock()
		defer self.mutex.Unlock()
		if self.stored {
			snSignError(w, http.StatusConflict, "the consent is already stored")
			return
		}
		if self.coldkey != "" && self.coldkey != address {
			snSignError(w, http.StatusConflict, "this run is for "+self.coldkey)
			return
		}
		if err := self.bindWithLock(address); err != nil {
			snSignError(w, http.StatusBadRequest, err.Error())
			return
		}
		snSignWriteJson(w, http.StatusOK, map[string]any{"address": self.coldkey, "state": self.stateWithLock()})
	}))
	mux.HandleFunc("/extend", post(func(w http.ResponseWriter, r *http.Request) {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		if self.stored {
			snSignError(w, http.StatusConflict, "the consent is already stored")
			return
		}
		if self.extensions >= snSignMaxExtensions {
			snSignError(w, http.StatusTooManyRequests, "this page cannot be extended again; run the command again")
			return
		}
		self.extensions += 1
		self.deadline = time.Now().Add(snSignPageLifetime)
		snSignWriteJson(w, http.StatusOK, map[string]any{"pageDeadline": self.deadline.Unix(), "extensionsLeft": snSignMaxExtensions - self.extensions})
	}))
	mux.HandleFunc("/refresh", post(func(w http.ResponseWriter, r *http.Request) {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		if self.challenge == nil {
			snSignError(w, http.StatusConflict, "choose your account first")
			return
		}
		if self.stored {
			snSignError(w, http.StatusConflict, "the consent is already stored")
			return
		}
		if self.refreshes >= snSignMaxRefreshes || time.Since(self.lastRefresh) < snSignRefreshSpacing {
			snSignError(w, http.StatusTooManyRequests, fmt.Sprintf("wait %d seconds between new messages (at most %d per run); the message you have stays valid until its timer ends", int(snSignRefreshSpacing/time.Second), snSignMaxRefreshes))
			return
		}
		self.refreshes += 1
		self.lastRefresh = time.Now()
		if err := self.newMessageWithLock(); err != nil {
			snSignError(w, http.StatusBadGateway, err.Error())
			return
		}
		// a new message is a fresh start: the page lives another full lifetime
		// (bounded by the refresh limit above)
		self.deadline = time.Now().Add(snSignPageLifetime)
		state := self.stateWithLock()
		snSignWriteJson(w, http.StatusOK, map[string]any{
			"message": state.Message, "hex": state.Hex, "expiresAt": state.ExpiresAt,
			"from": state.From, "through": state.Through, "currentEpoch": state.CurrentEpoch,
			"pageDeadline": self.deadline.Unix(),
		})
	}))
	mux.HandleFunc("/signature", post(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Signature string `json:"signature"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			snSignError(w, http.StatusBadRequest, "bad request")
			return
		}
		signature, err := snNormalizeSignatureHex(body.Signature)
		if err != nil {
			snSignError(w, http.StatusBadRequest, err.Error())
			return
		}
		self.mutex.Lock()
		defer self.mutex.Unlock()
		if self.challenge == nil {
			snSignError(w, http.StatusConflict, "choose your account first")
			return
		}
		if self.stored {
			snSignError(w, http.StatusConflict, "the consent is already stored")
			return
		}
		if time.Now().Unix() >= self.challenge.ExpiresAt {
			snSignError(w, http.StatusGone, "the message expired; click New message")
			return
		}
		// the stored message is the one the sdk verified, never one the page sent
		challenge := self.challenge
		result, err := self.api.SnNetworkConsentSubmitSyncWithContext(self.ctx, self.coldkey, challenge.Message, signature)
		if err != nil {
			snSignError(w, http.StatusBadGateway, err.Error())
			return
		}
		if result.Error != nil {
			snSignError(w, http.StatusBadGateway, result.Error.Message)
			return
		}
		self.stored = true
		fmt.Printf("subnet wallet consent for %s stored: epochs %d through %d, generation %d, receipt %s\n",
			self.coldkey, challenge.FromEpoch, challenge.ThroughEpoch, result.MappingGeneration, snPlain(result.MappingHash))
		fmt.Print(snConsentNextSteps(challenge.FromEpoch, challenge.ThroughEpoch, self.currentEpoch))
		snSignWriteJson(w, http.StatusOK, map[string]any{
			"ok": true, "receipt": snPlain(result.MappingHash), "from": challenge.FromEpoch,
			"through": challenge.ThroughEpoch, "generation": result.MappingGeneration,
		})
		go func() {
			time.Sleep(snSignLingerAfterDone)
			close(self.done)
		}()
	}))
	return mux
}

// snConsentNextSteps is what the operator does and does not have to do after a
// consent is stored.
func snConsentNextSteps(from, through, currentEpoch int64) string {
	var b strings.Builder
	b.WriteString("\nWhat happens next:\n")
	b.WriteString("  - You are done. Nothing changes on your provider machines; the consent covers every provider client on your network, including ones you add later.\n")
	if from > currentEpoch {
		fmt.Fprintf(&b, "  - It applies from epoch %d, which starts in %d epoch(s) (the network is in epoch %d now). Earlier epochs are not covered by this consent.\n", from, from-currentEpoch, currentEpoch)
	} else {
		fmt.Fprintf(&b, "  - It applies from epoch %d.\n", from)
	}
	b.WriteString("  - Check it any time with: provider wallet status\n")
	fmt.Fprintf(&b, "  - Sign again only to change the wallet or after epoch %d. A later consent must start at a later epoch than this one.\n", through)
	return b.String()
}

// snOpenBrowser opens the url in the default browser, best effort. Nothing
// depends on it: the url is always printed.
func snOpenBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		if raw, err := os.ReadFile("/proc/version"); err == nil && strings.Contains(strings.ToLower(string(raw)), "microsoft") {
			cmd = exec.Command("cmd.exe", "/c", "start", "", url)
		} else {
			cmd = exec.Command("xdg-open", url)
		}
	}
	_ = cmd.Start()
}

// snServeSignUi serves the page on the loopback interface until it is done,
// times out or the context ends.
func snServeSignUi(ctx context.Context, out io.Writer, ui *snSignUi, port int, openBrowser bool) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil && port != 0 {
		// the chosen port is taken: any free one will do
		listener, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return fmt.Errorf("cannot listen on this computer: %w", err)
	}
	pageUrl := fmt.Sprintf("http://%s/", listener.Addr().String())
	server := &http.Server{Handler: ui.handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer server.Close()

	ui.mutex.Lock()
	challenge, coldkey := ui.challenge, ui.coldkey
	ui.mutex.Unlock()
	if challenge != nil {
		fmt.Fprintf(out, "Sign the wallet consent for %s (epochs %d through %d).\n", snPlain(coldkey), challenge.FromEpoch, challenge.ThroughEpoch)
	} else {
		fmt.Fprintf(out, "Sign the wallet consent. You choose the account in your wallet extension on the page.\n")
	}
	fmt.Fprintf(out, "Open this address in the browser that has your wallet extension:\n\n    %s\n\n", pageUrl)
	fmt.Fprintf(out, "It stops serving in %d minutes (the page can extend it), or when you are done. Ctrl-C stops it now.\n", int(snSignPageLifetime/time.Minute))
	if openBrowser {
		snOpenBrowser(pageUrl)
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ui.done:
			return nil
		case <-ticker.C:
			ui.mutex.Lock()
			stopped, stored := time.Now().After(ui.deadline), ui.stored
			ui.mutex.Unlock()
			if stopped {
				if !stored {
					return errors.New("the page timed out before a signature was stored; run the command again")
				}
				return nil
			}
		case err := <-serveErr:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// snWalletSignUi is `provider wallet sign`.
func snWalletSignUi(ctx context.Context, out io.Writer, api snSignApi, apiUrl string, coldkeySs58 string, port int, openBrowser bool) error {
	if err := snCheckSigningApiUrl(apiUrl); err != nil {
		return err
	}
	token, err := snSignToken()
	if err != nil {
		return err
	}
	ui := &snSignUi{
		ctx: ctx, api: api, apiUrl: apiUrl, token: token,
		deadline: time.Now().Add(snSignPageLifetime), done: make(chan struct{}),
	}
	if coldkeySs58 = strings.TrimSpace(coldkeySs58); coldkeySs58 != "" {
		ui.mutex.Lock()
		err = ui.bindWithLock(coldkeySs58)
		ui.mutex.Unlock()
		if err != nil {
			return err
		}
	}
	return snServeSignUi(ctx, out, ui, port, openBrowser)
}
