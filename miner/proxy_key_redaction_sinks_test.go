package miner

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

// Every operator-facing sink that receives a proxy identity key is driven for
// real and its output checked for the key separator, the user and the password.

const (
	sinkTestUser     = "sppmr4vcnj"
	sinkTestPassword = "hunter2hunter2"
	sinkTestAddress  = "dc.decodo.com:10072"
)

func sinkTestSettings() *connect.ProxySettings {
	return reloadTestSettings(sinkTestAddress, sinkTestUser, sinkTestPassword)
}

func assertNoKeyLeak(t *testing.T, sink, text string, extra ...string) {
	t.Helper()
	for _, leak := range append([]string{"\x1f", sinkTestUser, sinkTestPassword}, extra...) {
		if strings.Contains(text, leak) {
			t.Errorf("%s leaks %q: %q", sink, leak, text)
		}
	}
}

// Sink: the duplicate-address refusal. The address is an operator-written
// field the legacy parser may keep malformed, userinfo included.
func TestAddressRefusalDoesNotPrintMalformedAddressOrOwner(t *testing.T) {
	for _, address := range []string{
		"socks5://alice:secret@host:1080/token",
		"alice:secret@host:1080",
		"host:1080;password=secret",
		sinkTestAddress,
	} {
		r := newMinerProxyReloader(nil, nil)
		first := reloadTestSettings(address, sinkTestUser, sinkTestPassword)
		second := reloadTestSettings(address, "othername77", "otherpass")
		if err := r.reserveAddress(first); err != nil {
			t.Fatal(err)
		}
		err := r.reserveAddress(second)
		if err == nil {
			t.Fatalf("%q: a second identity reserved the address", address)
		}
		assertNoKeyLeak(t, "reserveAddress refusal for "+address, err.Error(), "alice", "secret", "othername", "otherpass", "token")
	}
}

// Sink: the error reload() returns for a failed start, a failed rotation, and
// the watcher/retry lines that print it.
func TestReloadErrorsDoNotCarryTheRawKey(t *testing.T) {
	h := &reloadTestHarness{}
	h.setDesired(sinkTestSettings())
	r := h.reloader()
	h.spawnErr = errors.New("dial failed")

	_, err := r.reload()
	if err == nil {
		t.Fatal("a failed start returned no error")
	}
	assertNoKeyLeak(t, "reload() start error", err.Error())

	// Rotation: a running slot whose credentials changed, replacement fails.
	h2 := &reloadTestHarness{}
	h2.setDesired(sinkTestSettings())
	r2 := h2.reloader()
	if _, err := r2.reload(); err != nil {
		t.Fatal(err)
	}
	h2.setDesired(reloadTestSettings(sinkTestAddress, sinkTestUser, "rotated-"+sinkTestPassword))
	h2.spawnErr = errors.New("dial failed")
	_, err = r2.reload()
	if err == nil {
		t.Fatal("a failed rotation returned no error")
	}
	assertNoKeyLeak(t, "reload() rotation error", err.Error(), "rotated-")
	if !strings.Contains(err.Error(), "no longer served") {
		t.Fatalf("rotation error lost its meaning: %q", err)
	}

	// The watcher and the retry path print exactly this joined error.
	log := captureHeartbeatLog(t)
	tlog("[proxy] reload failed: %v (will retry)\n", err)
	tlog("[proxy][retry] reload for %s failed: %v\n", proxyKeyDisplay(sinkTestSettings().Key()), err)
	assertNoKeyLeak(t, "watcher/retry log line", log.String())
}

// Sink: the h3 relaunch log line, which printed the detached key directly.
func TestRelaunchIdentityLogDoesNotPrintTheRawKey(t *testing.T) {
	h := &reloadTestHarness{}
	settings := sinkTestSettings()
	h.setDesired(settings)
	r := h.reloader()
	if _, err := r.reload(); err != nil {
		t.Fatal(err)
	}
	h.derr = errors.New("desired set unreadable")
	log := captureHeartbeatLog(t)

	if !r.relaunchIdentity(settings.Key()) {
		t.Fatal("the running identity was not detached")
	}
	if !strings.Contains(log.String(), "reload after detaching") {
		t.Fatalf("the relaunch failure was not logged: %q", log.String())
	}
	assertNoKeyLeak(t, "relaunchIdentity log", log.String())
}

// Sink: `provider claim --store-client`, whose errors go straight to stderr.
func TestStoreClientErrorsDoNotEchoKeyOrUser(t *testing.T) {
	typed := sinkTestAddress + "\x1f" + sinkTestUser
	writeStoreTestStore(t, map[string]clientJWTStoreEntry{
		typed:                             {ByClientJWT: storeTestNetworkJwt(t)},
		"gw.example:1080\x1fexpired-user": {ByClientJWT: storeTestClientJwt(t, "00000000-0000-0000-0000-000000000201", "x", time.Now().Add(-time.Hour))},
	})

	cases := map[string]string{
		"invalid stored credential": typed,
		"expired stored credential": "gw.example:1080\x1fexpired-user",
		"missing client":            "gone.example:1080\x1f" + sinkTestUser,
	}
	for sink, key := range cases {
		_, err := snClaimCredentialFromOpts(docopt.Opts{"--store-client": key})
		if err == nil {
			t.Fatalf("%s: selected an identity", sink)
		}
		assertNoKeyLeak(t, "store-client "+sink, err.Error(), "expired-user")
	}
}

// Sink: the slot-stop line, at the function both reload spawn paths call.
func TestReloadedSlotStopLineUsesTheDisplayKey(t *testing.T) {
	log := captureHeartbeatLog(t)
	logReloadedSlotStopped(sinkTestSettings(), 7, errors.New("session ended"))
	if !strings.Contains(log.String(), "(provider 7) stopped: session ended") {
		t.Fatalf("slot-stop line changed shape: %q", log.String())
	}
	assertNoKeyLeak(t, "slot-stop log line", log.String())
}

// The real call sites cannot be driven without a full provide run, so pin them
// by source: neither spawn path may log the stop itself, and no operator-facing
// print may format a ProxySettings key without proxyKeyDisplay.
func TestNoPrintSinkFormatsARawProxyKey(t *testing.T) {
	run, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(run), "logReloadedSlotStopped(settings, index, err)"); got != 2 {
		t.Fatalf("run.go calls logReloadedSlotStopped %d times, want one per reload spawn path (2)", got)
	}

	// A key or raw address reaches a print through a local as often as it does
	// literally, so the scan follows one hop: a name assigned from a key or
	// address source (within the same function) is as raw as the source.
	sink := regexp.MustCompile(`\b(tlog|Printf|Fprintf|Sprintf|Errorf|Println|panic|Wrap|Infof|Warningf)\(|"proxy:"\s*\+`)
	source := regexp.MustCompile(`\.Key\(\)|\bidentityKey\b|\.Ids\(\)|(?i:settings)\.Address\b|\bproxyAddr\b`)
	redacted := regexp.MustCompile(`proxyKeyDisplay\(|redactedProxyAddress\(|providerSlotDisplay\(|obfuscateUser\(`)
	assign := regexp.MustCompile(`^\s*(?:for\s+)?([A-Za-z_]\w*(?:\s*,\s*[A-Za-z_]\w*)*)\s*(?::=|=)\s*(?:range\s+)?(.*)$`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		tainted := map[string]bool{}
		for number, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(line, "func ") {
				tainted = map[string]bool{}
			}
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if m := assign.FindStringSubmatch(line); m != nil && source.MatchString(topLevelOnly(m[2])) && !redacted.MatchString(m[2]) && !sink.MatchString(m[2]) {
				for _, name := range strings.Split(m[1], ",") {
					if name = strings.TrimSpace(name); name != "_" {
						tainted[name] = true
					}
				}
			}
			if !sink.MatchString(line) || redacted.MatchString(line) {
				continue
			}
			raw := source.MatchString(line)
			for name := range tainted {
				if name != "err" && regexp.MustCompile(`\b`+name+`\b`).MatchString(line) {
					raw = true
				}
			}
			if raw {
				t.Errorf("%s:%d formats a raw proxy key or address: %s", file, number+1, trimmed)
			}
		}
	}
}

// topLevelOnly drops index expressions and the arguments of calls that only
// consume a value (a lookup key, a dial address), so the assignment is tainted
// by what it returns from its source and not by what it was handed. Calls that
// build text from their arguments keep them.
func topLevelOnly(rhs string) string {
	index := regexp.MustCompile(`\[[^\[\]]*\]`)
	call := regexp.MustCompile(`(\w+)\(([^()]+)\)`)
	passThrough := map[string]bool{"Sprintf": true, "Errorf": true, "Join": true, "string": true, "Wrap": true}
	for {
		next := index.ReplaceAllString(rhs, "")
		next = call.ReplaceAllStringFunc(next, func(m string) string {
			sub := call.FindStringSubmatch(m)
			if passThrough[sub[1]] {
				return sub[2]
			}
			return sub[1] + "()"
		})
		if next == rhs {
			return rhs
		}
		rhs = next
	}
}
