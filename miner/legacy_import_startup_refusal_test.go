//go:build linux || darwin

// A 3.23-fix state refused at startup must not disappear. The automatic
// legacy import is reached, a refused startup reports the documented
// startup_recovery_required JSON even when the primary diagnostic stream is a
// redirected regular file the exporter refuses, and the process exits nonzero
// so a supervisor retries instead of reading a clean completion.
package miner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	"github.com/urnetwork/connect"
)

const legacyRefusalChildEnv = "SN_TEST_LEGACY_STARTUP_REFUSAL_CHILD"

// legacyStartupRefusalState is the operator-side state a 3.23-fix provider
// leaves on disk: the legacy identity store, the retained seed without the sn
// identity marker, a proxy.state with one configured proxy and the network
// sign-in. None of it is sn custody, so the automatic import is the only
// writer with a choice about these files.
type legacyStartupRefusalState struct {
	dir      string
	home     string
	seed     []byte
	storeRaw []byte
	proxyKey string
}

// A valid 32-byte seed still needs the sn marker to adopt; the NY5 backup's
// zero seed is the unusable-key variant of the same refused startup.
func writeLegacyStartupRefusalState(t *testing.T, seed []byte) *legacyStartupRefusalState {
	t.Helper()
	dir := providerRegistrationPrivateDir(t)
	home := providerRegistrationPrivateDir(t)
	networkID := connect.NewId().String()
	clientID := "00000000-0000-0000-0000-000000000101"
	member := &connect.ProxySettings{
		Network: "tcp",
		Address: "192.0.2.40:1080",
		Auth:    &proxy.Auth{User: "synthetic-one", Password: "synthetic-password"},
	}
	proxyKey := member.Key()
	entry := legacyClientJwtEntry{
		ByClientJWT: legacyTestClientToken(networkID, clientID, "00000000-0000-0000-0000-000000000202"),
		ClientID:    clientID,
		NetworkID:   networkID,
		MintedAt:    time.Now().UTC(),
	}
	storeRaw, err := json.Marshal(map[string]legacyClientJwtEntry{proxyKey: entry, legacyImportDirectKey: entry})
	if err != nil {
		t.Fatal(err)
	}
	proxyState, err := json.Marshal(map[string]any{
		"version":    2,
		"source":     "",
		"started_at": time.Now().UTC().Format(time.RFC3339),
		"next_id":    2,
		"proxies": map[string]any{
			proxyKey: map[string]any{"id": 1, "health": "up", "source": "internal"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	proxyConfig, err := json.Marshal(map[string]any{
		"auths":   map[string]any{},
		"servers": map[string]string{"192.0.2.40:1080:synthetic-one:synthetic-password": ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, raw []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("jwt", []byte(legacyTestNetworkToken(networkID)))
	write(legacyProviderKeyName, seed)
	write(legacyClientJwtStoreName, storeRaw)
	write("proxy.state", proxyState)
	write("proxy", proxyConfig)
	return &legacyStartupRefusalState{dir: dir, home: home, seed: seed, storeRaw: storeRaw, proxyKey: proxyKey}
}

// runLegacyRefusalChild re-enters the real public command with the state in
// the environment, exactly as the operator ran it, with stdout redirected to
// a regular file (the exporter refuses that sink) and stderr captured.
func runLegacyRefusalChild(t *testing.T, state *legacyStartupRefusalState, apiUrl string) (*bytes.Buffer, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProvideLegacyStartupRefusal", "-test.timeout=160s")
	command.Env = append(os.Environ(),
		legacyRefusalChildEnv+"=1",
		"HOME="+state.home,
		"URNETWORK_STATE_DIR="+state.dir,
		"WARP_VERSION=synthetic-test",
		"SN_TEST_LEGACY_API="+apiUrl,
		"SN_TEST_LEGACY_CONNECT=ws"+strings.TrimPrefix(apiUrl, "http"),
	)
	stdoutPath := filepath.Join(t.TempDir(), "provide.stdout")
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	var stderr bytes.Buffer
	command.Stdout, command.Stderr = stdout, &stderr
	return &stderr, command.Run()
}

// The refused startup leaves the 3.23-fix files byte-identical, no sn
// identity marker and no credential, and reports the import decision.
func assertLegacyStartupRefusedLoudly(t *testing.T, state *legacyStartupRefusalState, stderr *bytes.Buffer, runErr error, skipDetail string) {
	t.Helper()
	var exited *exec.ExitError
	if !errors.As(runErr, &exited) || exited.ExitCode() == 0 {
		t.Fatalf("a refused 3.23-fix startup read as a clean exit: err=%v stderr=%s", runErr, stderr.String())
	}
	text := stderr.String()
	if !strings.Contains(text, `"event":"startup_recovery_required"`) {
		t.Fatalf("refused startup did not report the documented recovery JSON: stderr=%s", text)
	}
	if !strings.Contains(text, `"event":"legacy_import_skipped"`) || !strings.Contains(text, skipDetail) {
		t.Fatalf("refused startup did not report the legacy import outcome (%s): stderr=%s", skipDetail, text)
	}
	if !strings.Contains(text, "provider startup refused") {
		t.Fatalf("refused startup did not name the refusal on stderr: stderr=%s", text)
	}
	if _, err := os.Stat(filepath.Join(state.dir, legacyProviderMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused import wrote the sn identity marker: %v", err)
	}
	for _, pattern := range []string{".provider.jwt", ".provider-*.jwt"} {
		names, err := filepath.Glob(filepath.Join(state.dir, pattern))
		if err != nil || len(names) != 0 {
			t.Fatalf("refused import wrote sn credentials (%s): %v %v", pattern, names, err)
		}
	}
	for name, want := range map[string][]byte{legacyProviderKeyName: state.seed, legacyClientJwtStoreName: state.storeRaw} {
		got, err := os.ReadFile(filepath.Join(state.dir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("refused import changed the 3.23-fix file %s", name)
		}
	}
}

// The zero-seed state is the NY5 backup shape: the import runs and refuses
// the unusable key at once, and the key gate refuses the unmarked key.
func TestProvideLegacyStartupRefusalZeroSeedStaysLoud(t *testing.T) {
	if os.Getenv(legacyRefusalChildEnv) == "1" {
		Run([]string{"provide", "--port=0", "--api_url=" + os.Getenv("SN_TEST_LEGACY_API"), "--connect_url=" + os.Getenv("SN_TEST_LEGACY_CONNECT")})
		return
	}
	state := writeLegacyStartupRefusalState(t, make([]byte, 32))
	stderr, err := runLegacyRefusalChild(t, state, "http://127.0.0.1:1")
	assertLegacyStartupRefusedLoudly(t, state, stderr, err, "provider_key_invalid")
}

// The valid-seed state reaches the proof phase against a local API that only
// answers transient failures: the slot is deferred without a decision, no
// marker is written, and the refused startup is still loud and nonzero.
func TestProvideLegacyStartupRefusalDeferredProofStaysLoud(t *testing.T) {
	if os.Getenv(legacyRefusalChildEnv) == "1" {
		Run([]string{"provide", "--port=0", "--api_url=" + os.Getenv("SN_TEST_LEGACY_API"), "--connect_url=" + os.Getenv("SN_TEST_LEGACY_CONNECT")})
		return
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	seed := bytes.Repeat([]byte{0x1f}, 32)
	state := writeLegacyStartupRefusalState(t, seed)
	stderr, err := runLegacyRefusalChild(t, state, server.URL)
	assertLegacyStartupRefusedLoudly(t, state, stderr, err, "refresh_unavailable")

	// A deferral is bookkeeping, never a decision: the journal may not carry
	// an imported or fallback verdict for the deferred slot.
	raw, readErr := os.ReadFile(filepath.Join(state.dir, legacyImportJournalName))
	if readErr != nil {
		t.Fatalf("deferred import did not retain its bookkeeping: %v", readErr)
	}
	var journal legacyImportJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		t.Fatalf("deferred import bookkeeping is unreadable: %v", err)
	}
	if len(journal.Slots) == 0 {
		t.Fatal("deferred import journal is empty")
	}
	for key, slot := range journal.Slots {
		if slot.State != "defer" {
			t.Fatalf("deferred import froze a decision for %s: state=%s reason=%s", key, slot.State, slot.Reason)
		}
	}
}
