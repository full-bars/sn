package miner

// Tests for the ported `provider sn-status [--json]` command: the v33 banner
// and the JSON document are checked against a synthetic platform.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newSnStatusFakeApi(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/network/ranking":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"network_ranking": map[string]any{
					"leaderboard_rank":   42,
					"net_mib_count":      1234.5,
					"leaderboard_public": true,
				},
			})
		case "/sn/epoch":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"epoch":            10,
				"start_block":      100,
				"finalize_block":   200,
				"contract_address": "0x00000000000000000000000000000000000000aa",
				"chain_id":         964,
			})
		case "/sn/pool/claim":
			if r.URL.Query().Get("epoch") != "9" {
				http.Error(w, "wrong epoch", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"epoch":     9,
				"share_bps": 250,
				"coldkey":   base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// `sn-status --json` emits the v33 document with the ranking, epoch and claim
// fields filled from the platform.
func TestSnStatusCommandJSON(t *testing.T) {
	server := newSnStatusFakeApi(t)
	setTestNetworkJwt(t, "synthetic-network-token")

	out := captureStdout(t, func() {
		snStatusCmd(parseArgsForTest(t, []string{"sn-status", "--json", "--api_url=" + server.URL}))
	})
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("sn-status --json is not JSON: %v\n%s", err, out)
	}
	for field, want := range map[string]any{
		"leaderboard_rank":   float64(42),
		"leaderboard_public": true,
		"net_mib_count":      1234.5,
		"current_epoch":      float64(10),
		"start_block":        float64(100),
		"finalize_block":     float64(200),
		"chain_id":           float64(964),
		"payout_share_bps":   float64(250),
		"claim_epoch":        float64(9),
		"top200_eligible":    true,
	} {
		if got := result[field]; got != want {
			t.Fatalf("sn-status --json %s = %#v; want %#v (%s)", field, got, want, out)
		}
	}
	if result["tier_description"] != "Tier 2 High-Volume (Rank #42 Globally)" {
		t.Fatalf("tier description changed: %#v", result["tier_description"])
	}
	coldkey, _ := result["coldkey_ss58"].(string)
	if coldkey == "" {
		t.Fatalf("the claim coldkey was not encoded: %s", out)
	}
	if _, present := result["error"]; present {
		t.Fatalf("a healthy synthetic platform produced an error field: %s", out)
	}
}

// The default output keeps the v33 banner lines.
func TestSnStatusCommandBanner(t *testing.T) {
	server := newSnStatusFakeApi(t)
	setTestNetworkJwt(t, "synthetic-network-token")

	out := captureStdout(t, func() {
		snStatusCmd(parseArgsForTest(t, []string{"sn-status", "--api_url=" + server.URL}))
	})
	for _, want := range []string{
		"URNetwork Subnet 25 — Node & Miner Status",
		"#42",
		"Tier 2 High-Volume (Rank #42 Globally)",
		"Net Bandwidth:",
		"Coldkey (SS58):",
		"Subnet Epoch:",
		"#10 (Start: #100, Finalize: #200)",
		"Payout Share:",
		"2.50%",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("sn-status banner lacks %q:\n%s", want, out)
		}
	}
}

// A failing platform is reported in the warning line, not as a crash.
func TestSnStatusCommandWarnsOnPlatformFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	setTestNetworkJwt(t, "synthetic-network-token")

	out := captureStdout(t, func() {
		snStatusCmd(parseArgsForTest(t, []string{"sn-status", "--json", "--api_url=" + server.URL}))
	})
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("sn-status --json is not JSON: %v\n%s", err, out)
	}
	if message, _ := result["error"].(string); message == "" {
		t.Fatalf("a failing platform produced no warning: %s", out)
	}
}

// The --json flag must parse on the sn-status grammar (v33 CLI-41c).
func TestMainUsageSnStatusJSON(t *testing.T) {
	opts := parseArgsForTest(t, []string{"sn-status", "--json"})
	if selected, _ := opts.Bool("sn-status"); !selected {
		t.Fatal("sn-status command was lost")
	}
	if jsonMode, err := opts.Bool("--json"); err != nil || !jsonMode {
		t.Fatalf("sn-status lost --json: %v, %v", jsonMode, err)
	}
}
