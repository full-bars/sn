package miner

// Tests for the ported v33 one-shot commands `provider direct`,
// `provider logs` and `provider print-network-id`.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gojwt "github.com/golang-jwt/jwt/v5"
)

// The new command grammar must parse at the public docopt boundary with v33's
// argument shapes.
func TestMainUsageDirectLogsPrintNetworkId(t *testing.T) {
	opts := parseArgsForTest(t, []string{"direct", "on"})
	if selected, err := opts.Bool("direct"); err != nil || !selected {
		t.Fatalf("direct command was lost: %v, %v", selected, err)
	}
	if state, err := opts.String("<state>"); err != nil || state != "on" {
		t.Fatalf("direct lost its state: %q, %v", state, err)
	}
	if state, _ := parseArgsForTest(t, []string{"direct"}).String("<state>"); state != "" {
		t.Fatal("a bare direct must report the current state")
	}

	opts = parseArgsForTest(t, []string{"logs", "-n", "7"})
	if selected, _ := opts.Bool("logs"); !selected {
		t.Fatal("logs command was lost")
	}
	if n, err := opts.Int("-n"); err != nil || n != 7 {
		t.Fatalf("logs lost its line count: %d, %v", n, err)
	}
	if n, err := parseArgsForTest(t, []string{"logs"}).Int("-n"); err != nil || n != 0 {
		t.Fatalf("logs -n default is not 0: %d, %v", n, err)
	}

	opts = parseArgsForTest(t, []string{"print-network-id", "/synthetic/jwt"})
	if selected, _ := opts.Bool("print-network-id"); !selected {
		t.Fatal("print-network-id command was lost")
	}
	if file, err := opts.String("<file>"); err != nil || file != "/synthetic/jwt" {
		t.Fatalf("print-network-id lost its file: %q, %v", file, err)
	}
}

// `provider direct` reports, enables and disables the persisted toggle with
// v33's exact output lines.
func TestDirectCommandWritesReportsAndClears(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)
	t.Setenv("DISABLE_DIRECT_IP", "")

	out := captureStdout(t, func() { cmdDirect(parseArgsForTest(t, []string{"direct"})) })
	if out != "direct: on (default — no override file)\n" {
		t.Fatalf("default status changed: %q", out)
	}

	out = captureStdout(t, func() { cmdDirect(parseArgsForTest(t, []string{"direct", "on"})) })
	if out != "direct: providing on direct/local IP is now ENABLED\n" {
		t.Fatalf("direct on output changed: %q", out)
	}
	path, err := directControlPath()
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "on\n" {
		t.Fatalf("direct on did not write the toggle: %q, %v", b, err)
	}
	out = captureStdout(t, func() { cmdDirect(parseArgsForTest(t, []string{"direct", "status"})) })
	if out != "direct: on (direct/local IP providing enabled)\n" {
		t.Fatalf("direct status after on changed: %q", out)
	}

	out = captureStdout(t, func() { cmdDirect(parseArgsForTest(t, []string{"direct", "off"})) })
	if out != "direct: providing on direct/local IP is now DISABLED (proxies only)\n" {
		t.Fatalf("direct off output changed: %q", out)
	}
	if enabled, exists := readDirectOverride(); !exists || enabled {
		t.Fatal("direct off did not persist the disabled state")
	}
	out = captureStdout(t, func() { cmdDirect(parseArgsForTest(t, []string{"direct"})) })
	if out != "direct: off (direct/local IP providing disabled)\n" {
		t.Fatalf("direct status after off changed: %q", out)
	}

	if err := clearDirectToggle(); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() { cmdDirect(parseArgsForTest(t, []string{"direct"})) })
	if out != "direct: on (default — no override file)\n" {
		t.Fatalf("cleared toggle did not restore the default: %q", out)
	}

	// the environment is the fallback only while no file exists
	t.Setenv("DISABLE_DIRECT_IP", "1")
	out = captureStdout(t, func() { cmdDirect(parseArgsForTest(t, []string{"direct"})) })
	if out != "direct: off (disabled by DISABLE_DIRECT_IP env var)\n" {
		t.Fatalf("env fallback changed: %q", out)
	}
	// a toggle file wins over the environment, the v33 precedence
	if err := writeDirectEnabled(true); err != nil {
		t.Fatal(err)
	}
	if !isDirectEnabled() {
		t.Fatal("the toggle file did not override DISABLE_DIRECT_IP")
	}
}

// readSHMLog returns the tail of the RAM log exactly as v33 does.
func TestReadSHMLogTailsTheRamLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "urnetwork.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := readSHMLog(path, 2); err != nil || out != "two\nthree\n" {
		t.Fatalf("tail changed: %q, %v", out, err)
	}
	if out, err := readSHMLog(path, 0); err != nil || out != "one\ntwo\nthree\n" {
		t.Fatalf("whole-file read changed: %q, %v", out, err)
	}
	if out, err := readSHMLog(path, 99); err != nil || out != "one\ntwo\nthree\n" {
		t.Fatalf("over-long tail changed: %q, %v", out, err)
	}
	if _, err := readSHMLog(filepath.Join(t.TempDir(), "absent.log"), 1); err == nil {
		t.Fatal("a missing ram log did not fail")
	}
}

// printNetworkIdCmd prints the network_id claim of a JWT file.
func TestPrintNetworkIdExtractsTheClaim(t *testing.T) {
	token, err := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims{
		"network_id": "00000000-0000-0000-0000-000000000301",
		"user_id":    "00000000-0000-0000-0000-000000000401",
	}).SignedString(gojwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		printNetworkIdCmd(parseArgsForTest(t, []string{"print-network-id", path}))
	})
	if strings.TrimSpace(out) != "00000000-0000-0000-0000-000000000301" {
		t.Fatalf("print-network-id output changed: %q", out)
	}
}
