package miner

import (
	"errors"
	"strings"
	"testing"

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
