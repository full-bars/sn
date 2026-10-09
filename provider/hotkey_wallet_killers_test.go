package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urfoundation/sn/crv4"
	"github.com/urfoundation/sn/hotkeywallet"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
)

func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	oldStdout := os.Stdout
	oldStderr := os.Stderr

	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	os.Stdout = wOut
	os.Stderr = wErr

	var outBuf, errBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&outBuf, rOut)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&errBuf, rErr)
	}()

	fn()

	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout = oldStdout
	os.Stderr = oldStderr
	wg.Wait()
	_ = rOut.Close()
	_ = rErr.Close()

	return outBuf.String() + "\n" + errBuf.String()
}

// 1. SECRET HYGIENE: captures stdout/stderr/tlog output while loading seeds
// and running hotkey commands against mock operator, asserting no hex/0x-hex/base64 seed leaks.
func TestHotkeySecretHygiene_NoSeedLeakedInOutput(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	op := newFakeOperator(t)
	if err := os.WriteFile(filepath.Join(stateDir, "jwt"), []byte(op.byJwt), 0600); err != nil {
		t.Fatal(err)
	}

	hotkeySeedRaw := bytes.Repeat([]byte{0x7a}, 32)
	hotkeySeedHex := hex.EncodeToString(hotkeySeedRaw)
	hotkeySeedPath := filepath.Join(stateDir, "hotkey.seed")
	if err := os.WriteFile(hotkeySeedPath, []byte(hotkeySeedHex), 0600); err != nil {
		t.Fatal(err)
	}

	coldkeySeedRaw := bytes.Repeat([]byte{0x8b}, 32)
	coldkeySeedHex := hex.EncodeToString(coldkeySeedRaw)
	coldkeySeedPath := filepath.Join(stateDir, "coldkey.seed")
	if err := os.WriteFile(coldkeySeedPath, []byte(coldkeySeedHex), 0600); err != nil {
		t.Fatal(err)
	}

	coldkeyPair, err := crv4.LoadSeedFile(coldkeySeedPath)
	if err != nil {
		t.Fatal(err)
	}
	coldkeyKp, err := crv4.KeypairFromSeed(coldkeyPair)
	if err != nil {
		t.Fatal(err)
	}
	coldkeySs58 := coldkeyKp.Address()
	coldkeyPubkey := coldkeyKp.PublicKey()

	var cmdOut bytes.Buffer
	ctx := context.Background()

	captured := captureOutput(t, func() {
		// 1. loadOperatorHotkey
		_, _ = loadOperatorHotkey(hotkeySeedPath)

		// 2. hotkeyWalletCommandHotkey
		_, _, _ = hotkeyWalletCommandHotkey(docopt.Opts{
			"--hotkey_seed_file": hotkeySeedPath,
		})

		// 3. startHotkeyWalletUpkeep
		upkeepCtx, upkeepCancel := context.WithCancel(ctx)
		_, _ = startHotkeyWalletUpkeep(upkeepCtx, hotkeySeedPath, op.server.URL, func(context.Context, string, *crv4.Keypair) {})
		upkeepCancel()

		// 4. snLoadColdkey
		_, _ = snLoadColdkey(coldkeySeedPath, coldkeySs58, coldkeyPubkey)

		// 5. hotkeyWalletChallenge
		_ = hotkeyWalletChallenge(ctx, docopt.Opts{
			"hotkey":             true,
			"challenge":          true,
			"<coldkey_ss58>":     coldkeySs58,
			"--hotkey_seed_file": hotkeySeedPath,
			"--api_url":          op.server.URL,
		}, &cmdOut)

		// 6. hotkeyWalletSet
		_ = hotkeyWalletSet(ctx, docopt.Opts{
			"hotkey":              true,
			"set":                 true,
			"<coldkey_ss58>":      coldkeySs58,
			"--hotkey_seed_file":  hotkeySeedPath,
			"--coldkey_seed_file": coldkeySeedPath,
			"--api_url":           op.server.URL,
		}, &cmdOut)

		// 7. hotkeyWalletStatus
		_ = hotkeyWalletStatus(ctx, docopt.Opts{
			"hotkey":             true,
			"status":             true,
			"--hotkey_seed_file": hotkeySeedPath,
			"--api_url":          op.server.URL,
		}, &cmdOut)
	})

	allOutput := captured + "\n" + cmdOut.String()

	secrets := [][]byte{hotkeySeedRaw, coldkeySeedRaw}
	for _, sec := range secrets {
		patterns := []string{
			hex.EncodeToString(sec),
			strings.ToUpper(hex.EncodeToString(sec)),
			"0x" + hex.EncodeToString(sec),
			"0X" + hex.EncodeToString(sec),
			base64.StdEncoding.EncodeToString(sec),
			base64.RawStdEncoding.EncodeToString(sec),
		}
		for _, p := range patterns {
			if strings.Contains(allOutput, p) {
				t.Fatalf("secret leaked in output: found %q", p)
			}
		}
	}
}

// 2. CONSENT VERIFICATION in hotkeyWalletSet (~574): wrong key signature or corrupted signature
// must be refused with error, nothing appended to store, and nothing submitted to operator.
func TestHotkeyWalletSet_InvalidOrWrongKeySignatureRefused(t *testing.T) {
	tests := []struct {
		name      string
		corrupt   bool
		wrongKey  bool
	}{
		{name: "wrong_key_signature", wrongKey: true},
		{name: "corrupted_signature", corrupt: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			t.Setenv("URNETWORK_STATE_DIR", stateDir)

			op := newFakeOperator(t)
			if err := os.WriteFile(filepath.Join(stateDir, "jwt"), []byte(op.byJwt), 0600); err != nil {
				t.Fatal(err)
			}

			hotkeySeed := filepath.Join(stateDir, "hotkey.seed")
			if err := os.WriteFile(hotkeySeed, []byte(strings.Repeat("12", 32)), 0600); err != nil {
				t.Fatal(err)
			}
			coldkeySeed := filepath.Join(stateDir, "coldkey.seed")
			if err := os.WriteFile(coldkeySeed, []byte(strings.Repeat("34", 32)), 0600); err != nil {
				t.Fatal(err)
			}
			wrongSeed := filepath.Join(stateDir, "wrong.seed")
			if err := os.WriteFile(wrongSeed, []byte(strings.Repeat("56", 32)), 0600); err != nil {
				t.Fatal(err)
			}

			coldkeyRaw, _ := crv4.LoadSeedFile(coldkeySeed)
			coldkeyKp, _ := crv4.KeypairFromSeed(coldkeyRaw)
			wrongRaw, _ := crv4.LoadSeedFile(wrongSeed)
			wrongKp, _ := crv4.KeypairFromSeed(wrongRaw)

			ctx := context.Background()
			var challengeOut bytes.Buffer
			challengeOpts := docopt.Opts{
				"hotkey":             true,
				"challenge":          true,
				"<coldkey_ss58>":     coldkeyKp.Address(),
				"--hotkey_seed_file": hotkeySeed,
				"--api_url":          op.server.URL,
			}
			if err := hotkeyWalletChallenge(ctx, challengeOpts, &challengeOut); err != nil {
				t.Fatalf("challenge failed: %v", err)
			}

			store := hotkeyWalletStore(stateDir)
			pending, err := store.Pending()
			if err != nil || pending == nil {
				t.Fatalf("expected pending statement, got %v", err)
			}
			msg, err := pending.Message()
			if err != nil {
				t.Fatal(err)
			}

			var sig [64]byte
			if tc.wrongKey {
				sig, err = hotkeywallet.Sign(wrongKp, msg)
				if err != nil {
					t.Fatal(err)
				}
			} else if tc.corrupt {
				sig, err = hotkeywallet.Sign(coldkeyKp, msg)
				if err != nil {
					t.Fatal(err)
				}
				sig[0] ^= 0xff
			}

			beforeConsentCalls := op.callCount("POST /sn/wallet/hotkey-consent")

			var setOut bytes.Buffer
			setOpts := docopt.Opts{
				"hotkey":             true,
				"set":                true,
				"<coldkey_ss58>":     coldkeyKp.Address(),
				"--hotkey_seed_file": hotkeySeed,
				"--message":          msg,
				"--signature":        "0x" + hex.EncodeToString(sig[:]),
				"--api_url":          op.server.URL,
			}

			err = hotkeyWalletSet(ctx, setOpts, &setOut)
			if err == nil {
				t.Fatal("expected hotkeyWalletSet to fail on invalid signature, got nil")
			}
			if !strings.Contains(err.Error(), "the coldkey's signature does not verify") {
				t.Fatalf("expected error mentioning verification failure, got: %v", err)
			}

			// Must not append to store
			chain, err := store.Chain()
			if err != nil {
				t.Fatal(err)
			}
			if len(chain) != 0 {
				t.Fatalf("expected 0 entries in chain store, got %d", len(chain))
			}

			// Must not submit to operator
			afterConsentCalls := op.callCount("POST /sn/wallet/hotkey-consent")
			if afterConsentCalls != beforeConsentCalls {
				t.Fatalf("operator was called: before=%d, after=%d", beforeConsentCalls, afterConsentCalls)
			}
		})
	}
}

// 3. PENDING STATEMENT checks in hotkeyWalletSet (~558-565): message mismatch, coldkey mismatch,
// hotkey mismatch, and epoch mismatch must each be refused.
func TestHotkeyWalletSet_PendingStatementMismatchesRefused(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	op := newFakeOperator(t)
	if err := os.WriteFile(filepath.Join(stateDir, "jwt"), []byte(op.byJwt), 0600); err != nil {
		t.Fatal(err)
	}

	hotkey1Seed := filepath.Join(stateDir, "hotkey1.seed")
	_ = os.WriteFile(hotkey1Seed, []byte(strings.Repeat("12", 32)), 0600)
	hotkey2Seed := filepath.Join(stateDir, "hotkey2.seed")
	_ = os.WriteFile(hotkey2Seed, []byte(strings.Repeat("34", 32)), 0600)

	coldkey1Seed := filepath.Join(stateDir, "coldkey1.seed")
	_ = os.WriteFile(coldkey1Seed, []byte(strings.Repeat("56", 32)), 0600)
	coldkey2Seed := filepath.Join(stateDir, "coldkey2.seed")
	_ = os.WriteFile(coldkey2Seed, []byte(strings.Repeat("78", 32)), 0600)

	coldkey1Raw, _ := crv4.LoadSeedFile(coldkey1Seed)
	coldkey1Kp, _ := crv4.KeypairFromSeed(coldkey1Raw)
	coldkey2Raw, _ := crv4.LoadSeedFile(coldkey2Seed)
	coldkey2Kp, _ := crv4.KeypairFromSeed(coldkey2Raw)

	ctx := context.Background()

	setupPending := func(from, through string) string {
		var out bytes.Buffer
		opts := docopt.Opts{
			"hotkey":             true,
			"challenge":          true,
			"<coldkey_ss58>":     coldkey1Kp.Address(),
			"--hotkey_seed_file": hotkey1Seed,
			"--api_url":          op.server.URL,
		}
		if from != "" && through != "" {
			opts["--wallet-from-epoch"] = from
			opts["--wallet-through-epoch"] = through
		}
		if err := hotkeyWalletChallenge(ctx, opts, &out); err != nil {
			t.Fatalf("challenge setup failed: %v", err)
		}
		store := hotkeyWalletStore(stateDir)
		pending, err := store.Pending()
		if err != nil || pending == nil {
			t.Fatalf("pending not found: %v", err)
		}
		msg, _ := pending.Message()
		return msg
	}

	dummySig := "0x" + strings.Repeat("aa", 64)

	t.Run("mismatched_message", func(t *testing.T) {
		setupPending("", "")
		var out bytes.Buffer
		err := hotkeyWalletSet(ctx, docopt.Opts{
			"hotkey":             true,
			"set":                true,
			"<coldkey_ss58>":     coldkey1Kp.Address(),
			"--hotkey_seed_file": hotkey1Seed,
			"--message":          "tampered message that does not match pending",
			"--signature":        dummySig,
			"--api_url":          op.server.URL,
		}, &out)
		if err == nil || !strings.Contains(err.Error(), "--message is not the pending statement") {
			t.Fatalf("expected error mentioning '--message is not the pending statement', got: %v", err)
		}
	})

	t.Run("mismatched_coldkey", func(t *testing.T) {
		msg := setupPending("", "")
		var out bytes.Buffer
		err := hotkeyWalletSet(ctx, docopt.Opts{
			"hotkey":             true,
			"set":                true,
			"<coldkey_ss58>":     coldkey2Kp.Address(),
			"--hotkey_seed_file": hotkey1Seed,
			"--message":          msg,
			"--signature":        dummySig,
			"--api_url":          op.server.URL,
		}, &out)
		if err == nil || !strings.Contains(err.Error(), "the pending statement names coldkey") {
			t.Fatalf("expected error mentioning 'the pending statement names coldkey', got: %v", err)
		}
	})

	t.Run("mismatched_hotkey", func(t *testing.T) {
		msg := setupPending("", "")
		var out bytes.Buffer
		err := hotkeyWalletSet(ctx, docopt.Opts{
			"hotkey":             true,
			"set":                true,
			"<coldkey_ss58>":     coldkey1Kp.Address(),
			"--hotkey_seed_file": hotkey2Seed,
			"--message":          msg,
			"--signature":        dummySig,
			"--api_url":          op.server.URL,
		}, &out)
		if err == nil || !strings.Contains(err.Error(), "the pending statement names another hotkey than --hotkey_seed_file") {
			t.Fatalf("expected error mentioning 'the pending statement names another hotkey', got: %v", err)
		}
	})

	t.Run("mismatched_epochs", func(t *testing.T) {
		msg := setupPending("100", "200")
		var out bytes.Buffer
		err := hotkeyWalletSet(ctx, docopt.Opts{
			"hotkey":                true,
			"set":                   true,
			"<coldkey_ss58>":        coldkey1Kp.Address(),
			"--hotkey_seed_file":    hotkey1Seed,
			"--wallet-from-epoch":   "150",
			"--wallet-through-epoch": "250",
			"--message":             msg,
			"--signature":           dummySig,
			"--api_url":             op.server.URL,
		}, &out)
		if err == nil || !strings.Contains(err.Error(), "the pending statement earns epochs") {
			t.Fatalf("expected error mentioning 'the pending statement earns epochs', got: %v", err)
		}
	})
}

// 4. hotkeyWalletSubmit failure (~589): when operator rejects consent, set must return an error.
func TestHotkeyWalletSet_OperatorSubmitFailure(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	op := newFakeOperator(t)
	if err := os.WriteFile(filepath.Join(stateDir, "jwt"), []byte(op.byJwt), 0600); err != nil {
		t.Fatal(err)
	}

	hotkeySeed := filepath.Join(stateDir, "hotkey.seed")
	_ = os.WriteFile(hotkeySeed, []byte(strings.Repeat("12", 32)), 0600)
	coldkeySeed := filepath.Join(stateDir, "coldkey.seed")
	_ = os.WriteFile(coldkeySeed, []byte(strings.Repeat("34", 32)), 0600)

	coldkeyRaw, _ := crv4.LoadSeedFile(coldkeySeed)
	coldkeyKp, _ := crv4.KeypairFromSeed(coldkeyRaw)

	ctx := context.Background()
	var out bytes.Buffer
	challengeOpts := docopt.Opts{
		"hotkey":             true,
		"challenge":          true,
		"<coldkey_ss58>":     coldkeyKp.Address(),
		"--hotkey_seed_file": hotkeySeed,
		"--api_url":          op.server.URL,
	}
	if err := hotkeyWalletChallenge(ctx, challengeOpts, &out); err != nil {
		t.Fatalf("challenge failed: %v", err)
	}

	store := hotkeyWalletStore(stateDir)
	pending, err := store.Pending()
	if err != nil || pending == nil {
		t.Fatalf("expected pending, got %v", err)
	}
	msg, _ := pending.Message()
	sig, err := hotkeywallet.Sign(coldkeyKp, msg)
	if err != nil {
		t.Fatal(err)
	}

	// Trigger operator failure during submit
	op.mu.Lock()
	op.return500 = true
	op.mu.Unlock()

	setOpts := docopt.Opts{
		"hotkey":             true,
		"set":                true,
		"<coldkey_ss58>":     coldkeyKp.Address(),
		"--hotkey_seed_file": hotkeySeed,
		"--message":          msg,
		"--signature":        "0x" + hex.EncodeToString(sig[:]),
		"--api_url":          op.server.URL,
	}
	err = hotkeyWalletSet(ctx, setOpts, &out)
	if err == nil {
		t.Fatal("expected hotkeyWalletSet to fail when operator fails submit, got nil")
	}
	if !strings.Contains(err.Error(), "do not adopt the head yet") {
		t.Fatalf("expected error mentioning 'do not adopt the head yet', got: %v", err)
	}
}

// 5. adopts() (~188-192): table test for each condition.
func TestHotkeyWalletEntry_AdoptsTable(t *testing.T) {
	hotkey := [32]byte{1, 2, 3, 4, 5}
	hotkeySs58, err := ss58.Encode(hotkey, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}

	differentHotkey := [32]byte{9, 9, 9}
	diffHotkeySs58, err := ss58.Encode(differentHotkey, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}

	headHash := [32]byte{10, 11, 12, 13}
	headHashHex := "0x" + hex.EncodeToString(headHash[:])

	diffHeadHash := [32]byte{20, 21, 22}
	diffHeadHashHex := "0x" + hex.EncodeToString(diffHeadHash[:])

	tests := []struct {
		name       string
		entry      *hotkeyWalletEntry
		queryKey   [32]byte
		queryHash  [32]byte
		queryGen   uint64
		wantAdopts bool
	}{
		{
			name: "all_matching_with_scope",
			entry: &hotkeyWalletEntry{
				HotkeySs58:        hotkeySs58,
				ConsentHeadHash:   headHashHex,
				ConsentGeneration: 3,
				ConsentScope:      protocol.EarningWalletModeHotkey,
			},
			queryKey:   hotkey,
			queryHash:  headHash,
			queryGen:   3,
			wantAdopts: true,
		},
		{
			name: "all_matching_without_scope",
			entry: &hotkeyWalletEntry{
				HotkeySs58:        hotkeySs58,
				ConsentHeadHash:   headHashHex,
				ConsentGeneration: 3,
			},
			queryKey:   hotkey,
			queryHash:  headHash,
			queryGen:   3,
			wantAdopts: true,
		},
		{
			name: "wrong_scope",
			entry: &hotkeyWalletEntry{
				HotkeySs58:        hotkeySs58,
				ConsentHeadHash:   headHashHex,
				ConsentGeneration: 3,
				ConsentScope:      "network",
			},
			queryKey:   hotkey,
			queryHash:  headHash,
			queryGen:   3,
			wantAdopts: false,
		},
		{
			name: "wrong_hotkey",
			entry: &hotkeyWalletEntry{
				HotkeySs58:        diffHotkeySs58,
				ConsentHeadHash:   headHashHex,
				ConsentGeneration: 3,
			},
			queryKey:   hotkey,
			queryHash:  headHash,
			queryGen:   3,
			wantAdopts: false,
		},
		{
			name: "wrong_generation",
			entry: &hotkeyWalletEntry{
				HotkeySs58:        hotkeySs58,
				ConsentHeadHash:   headHashHex,
				ConsentGeneration: 4,
			},
			queryKey:   hotkey,
			queryHash:  headHash,
			queryGen:   3,
			wantAdopts: false,
		},
		{
			name: "wrong_head_hash",
			entry: &hotkeyWalletEntry{
				HotkeySs58:        hotkeySs58,
				ConsentHeadHash:   diffHeadHashHex,
				ConsentGeneration: 3,
			},
			queryKey:   hotkey,
			queryHash:  headHash,
			queryGen:   3,
			wantAdopts: false,
		},
		{
			name:       "nil_entry",
			entry:      nil,
			queryKey:   hotkey,
			queryHash:  headHash,
			queryGen:   3,
			wantAdopts: false,
		},
		{
			name: "invalid_ss58",
			entry: &hotkeyWalletEntry{
				HotkeySs58:        "not-a-valid-ss58-address",
				ConsentHeadHash:   headHashHex,
				ConsentGeneration: 3,
			},
			queryKey:   hotkey,
			queryHash:  headHash,
			queryGen:   3,
			wantAdopts: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.entry.adopts(tc.queryKey, tc.queryHash, tc.queryGen)
			if got != tc.wantAdopts {
				t.Fatalf("adopts() = %v, want %v", got, tc.wantAdopts)
			}
		})
	}
}

// 6. snNormalizeSignatureHex length check (~805) and snUnescapeMessage (~822): table tests.
func TestSnNormalizeSignatureHex_Table(t *testing.T) {
	valid64Bytes := bytes.Repeat([]byte{0xab}, 64)
	valid64Hex := hex.EncodeToString(valid64Bytes)

	tests := []struct {
		name      string
		input     string
		want      string
		errSubstr string
	}{
		{
			name:  "valid_with_0x",
			input: "0x" + valid64Hex,
			want:  "0x" + valid64Hex,
		},
		{
			name:  "valid_without_0x",
			input: valid64Hex,
			want:  "0x" + valid64Hex,
		},
		{
			name:  "valid_uppercase_0X",
			input: "0X" + strings.ToUpper(valid64Hex),
			want:  "0x" + valid64Hex,
		},
		{
			name:      "refused_63_bytes",
			input:     hex.EncodeToString(bytes.Repeat([]byte{0xab}, 63)),
			errSubstr: "must be a 64-byte sr25519 signature, got 63 bytes",
		},
		{
			name:      "refused_65_bytes",
			input:     hex.EncodeToString(bytes.Repeat([]byte{0xab}, 65)),
			errSubstr: "must be a 64-byte sr25519 signature, got 65 bytes",
		},
		{
			name:      "refused_32_bytes",
			input:     hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32)),
			errSubstr: "must be a 64-byte sr25519 signature, got 32 bytes",
		},
		{
			name:      "invalid_hex",
			input:     "0xnothexchars!",
			errSubstr: "--signature must be hex",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := snNormalizeSignatureHex(tc.input)
			if tc.errSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("expected error mentioning %q, got: %v", tc.errSubstr, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.want {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			}
		})
	}
}

func TestSnUnescapeMessage_Table(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "escaped_newlines",
			input: "line1\\nline2\\nline3",
			want:  "line1\nline2\nline3",
		},
		{
			name:  "already_contains_real_newlines",
			input: "line1\nline2\\nstill_real",
			want:  "line1\nline2\\nstill_real",
		},
		{
			name:  "no_newlines",
			input: "single line without escapes",
			want:  "single line without escapes",
		},
		{
			name:  "empty_string",
			input: "",
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := snUnescapeMessage(tc.input)
			if got != tc.want {
				t.Fatalf("snUnescapeMessage(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// 7. Epoch flag validation (~287, ~298): only one of from/through refused;
// through < from refused; range > hotkeyWalletIntervalEpochs refused;
// and hotkeyWalletIntervalEpochs == 65535.
func TestHotkeyWallet_IntervalEpochsConstant(t *testing.T) {
	if hotkeyWalletIntervalEpochs != 65535 {
		t.Fatalf("hotkeyWalletIntervalEpochs = %d, want 65535", hotkeyWalletIntervalEpochs)
	}

	// Range of exactly 65535 must be accepted
	opts := docopt.Opts{
		"--wallet-from-epoch":    "10",
		"--wallet-through-epoch": "65545", // 65545 - 10 = 65535
	}
	epochs, err := hotkeyWalletEpochsFromOpts(opts)
	if err != nil {
		t.Fatalf("expected exact interval 65535 to be accepted, got: %v", err)
	}
	if epochs == nil || epochs.from != 10 || epochs.through != 65545 {
		t.Fatalf("unexpected epochs result: %+v", epochs)
	}

	// Range of 65536 must be refused
	optsExcess := docopt.Opts{
		"--wallet-from-epoch":    "10",
		"--wallet-through-epoch": "65546", // 65546 - 10 = 65536
	}
	_, err = hotkeyWalletEpochsFromOpts(optsExcess)
	if err == nil || !strings.Contains(err.Error(), "must be at most 65535 epochs after") {
		t.Fatalf("expected error for interval exceeding 65535, got: %v", err)
	}
}

func TestHotkeyWalletEpochsFromOpts_Validation(t *testing.T) {
	tests := []struct {
		name      string
		opts      docopt.Opts
		errSubstr string
		wantNil   bool
	}{
		{
			name:    "neither_specified",
			opts:    docopt.Opts{},
			wantNil: true,
		},
		{
			name:      "only_from_specified",
			opts:      docopt.Opts{"--wallet-from-epoch": "100"},
			errSubstr: "--wallet-from-epoch and --wallet-through-epoch go together",
		},
		{
			name:      "only_through_specified",
			opts:      docopt.Opts{"--wallet-through-epoch": "100"},
			errSubstr: "--wallet-from-epoch and --wallet-through-epoch go together",
		},
		{
			name:      "through_smaller_than_from",
			opts:      docopt.Opts{"--wallet-from-epoch": "100", "--wallet-through-epoch": "50"},
			errSubstr: "must be at most 65535 epochs after",
		},
		{
			name:      "range_exceeds_interval",
			opts:      docopt.Opts{"--wallet-from-epoch": "0", "--wallet-through-epoch": "70000"},
			errSubstr: "must be at most 65535 epochs after",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hotkeyWalletEpochsFromOpts(tc.opts)
			if tc.errSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("expected error mentioning %q, got: %v", tc.errSubstr, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tc.wantNil && got != nil {
					t.Fatalf("expected nil epochs, got %+v", got)
				}
			}
		})
	}
}

// 8. Operator answer hardening against mock operator:
// redirect refused (~94), oversized answer refused (~105), implausible epoch refused (~135),
// malformed genesis_hash refused (~204), netuid > 65535 refused (~207), operators disagree refused (~251).
func TestHotkeyWallet_OperatorAnswerHardening(t *testing.T) {
	ctx := context.Background()

	t.Run("redirect_refused", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/redirect" {
				http.Redirect(w, r, "/destination", http.StatusFound)
				return
			}
			if r.URL.Path == "/destination" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok": true}`))
				return
			}
			http.NotFound(w, r)
		}))
		defer ts.Close()

		target := hotkeyWalletTarget{apiUrl: ts.URL, byJwt: "dummy", domain: "test"}
		var dest map[string]any
		err := hotkeyWalletCall(ctx, target, http.MethodGet, "/redirect", nil, &dest)
		if err == nil {
			t.Fatal("expected redirect to be refused with error, got nil")
		}
		if !strings.Contains(err.Error(), "302 Found") {
			t.Fatalf("expected error mentioning 302 Found, got: %v", err)
		}
	})

	t.Run("oversized_answer_refused", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			chunk := bytes.Repeat([]byte("a"), 64*1024)
			written := 0
			for written < hotkeyWalletAnswerBytes+1 {
				n, _ := w.Write(chunk)
				written += n
			}
		}))
		defer ts.Close()

		target := hotkeyWalletTarget{apiUrl: ts.URL, byJwt: "dummy", domain: "test"}
		var dest map[string]any
		err := hotkeyWalletCall(ctx, target, http.MethodGet, "/large", nil, &dest)
		if err == nil || !strings.Contains(err.Error(), "answer exceeds") {
			t.Fatalf("expected answer exceeds error, got: %v", err)
		}
	})

	t.Run("implausible_epoch_refused", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"epoch":        uint64(math.MaxUint64),
				"chain_id":     1,
				"genesis_hash": "0x" + strings.Repeat("11", 32),
				"netuid":       1,
			})
		}))
		defer ts.Close()

		target := hotkeyWalletTarget{apiUrl: ts.URL, byJwt: "dummy", domain: "test"}
		_, err := hotkeyWalletEpoch(ctx, target)
		if err == nil || !strings.Contains(err.Error(), "is implausible") {
			t.Fatalf("expected error mentioning 'is implausible', got: %v", err)
		}
	})

	t.Run("malformed_genesis_hash_refused", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"epoch":        10,
				"chain_id":     1,
				"genesis_hash": "0x1234", // malformed: not 64 hex digits
				"netuid":       1,
			})
		}))
		defer ts.Close()

		target := hotkeyWalletTarget{apiUrl: ts.URL, byJwt: "dummy", domain: "test"}
		_, err := hotkeyWalletOperatorSubnet(ctx, target, "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY")
		if err == nil || !strings.Contains(err.Error(), "not 0x and 64 hex digits") {
			t.Fatalf("expected error mentioning 'not 0x and 64 hex digits', got: %v", err)
		}
	})

	t.Run("netuid_overflow_refused", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"epoch":        10,
				"chain_id":     1,
				"genesis_hash": "0x" + strings.Repeat("22", 32),
				"netuid":       70000, // > math.MaxUint16 (65535)
			})
		}))
		defer ts.Close()

		target := hotkeyWalletTarget{apiUrl: ts.URL, byJwt: "dummy", domain: "test"}
		_, err := hotkeyWalletOperatorSubnet(ctx, target, "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY")
		if err == nil || !strings.Contains(err.Error(), "beyond any subnet") {
			t.Fatalf("expected error mentioning 'beyond any subnet', got: %v", err)
		}
	})

	t.Run("operators_disagree_on_subnet", func(t *testing.T) {
		ts1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"epoch":        10,
				"chain_id":     1,
				"genesis_hash": "0x" + strings.Repeat("33", 32),
				"netuid":       1,
			})
		}))
		defer ts1.Close()

		ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"epoch":        10,
				"chain_id":     1,
				"genesis_hash": "0x" + strings.Repeat("33", 32),
				"netuid":       2, // differs from operator 1
			})
		}))
		defer ts2.Close()

		target1 := hotkeyWalletTarget{apiUrl: ts1.URL, byJwt: "dummy", domain: "op1"}
		target2 := hotkeyWalletTarget{apiUrl: ts2.URL, byJwt: "dummy", domain: "op2"}

		var out bytes.Buffer
		_, err := hotkeyWalletSubnet(ctx, []hotkeyWalletTarget{target1, target2}, "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY", &out)
		if err == nil || !strings.Contains(err.Error(), "operators disagree on the subnet") {
			t.Fatalf("expected error mentioning 'operators disagree on the subnet', got: %v", err)
		}
	})
}

// 9. hotkeyWalletNextStatement from-epoch clamping (~329) and upkeep clamping (~373);
// runHotkeyWalletUpkeepStep with an empty chain does nothing (~700);
// hotkeyWalletSet without seed file or message refused (~488).
func TestHotkeyWalletNextStatement_FromEpochClamping(t *testing.T) {
	stateDir := t.TempDir()
	store := hotkeyWalletStore(stateDir)

	op := newFakeOperator(t)
	op.mu.Lock()
	op.epoch = 100 // current epoch is 100
	op.mu.Unlock()

	hotkeySeed := filepath.Join(stateDir, "hotkey.seed")
	_ = os.WriteFile(hotkeySeed, []byte(strings.Repeat("12", 32)), 0600)
	coldkeySeed := filepath.Join(stateDir, "coldkey.seed")
	_ = os.WriteFile(coldkeySeed, []byte(strings.Repeat("34", 32)), 0600)

	hotkeyKp, _ := loadOperatorHotkey(hotkeySeed)
	coldkeyRaw, _ := crv4.LoadSeedFile(coldkeySeed)
	coldkeyKp, _ := crv4.KeypairFromSeed(coldkeyRaw)

	// Create and append a chain entry with FromEpoch = 200 (well ahead of current epoch 100)
	stmt, err := hotkeywallet.NextStatement(nil, fakeTestDomain.HotkeySubnet(), hotkeyKp.PublicKey(), coldkeyKp.PublicKey(), 200, 250, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	msg, err := stmt.Message()
	if err != nil {
		t.Fatal(err)
	}
	coldkeySig, _ := hotkeywallet.Sign(coldkeyKp, msg)
	hotkeySig, _ := hotkeywallet.Sign(hotkeyKp, msg)
	original := protocol.HotkeyWalletMappingConsent{
		Message:          msg,
		ColdkeySignature: coldkeySig,
		HotkeySignature:  hotkeySig,
	}
	if err := store.Append(original); err != nil {
		t.Fatal(err)
	}

	target := hotkeyWalletTarget{apiUrl: op.server.URL, byJwt: op.byJwt, domain: "test"}
	var out bytes.Buffer
	nextStmt, err := hotkeyWalletNextStatement(context.Background(), store, []hotkeyWalletTarget{target}, hotkeyKp, coldkeyKp.PublicKey(), coldkeyKp.Address(), nil, &out)
	if err != nil {
		t.Fatalf("hotkeyWalletNextStatement failed: %v", err)
	}

	// Must be clamped to max(current+1, head.FromEpoch+1) = max(101, 201) = 201
	if nextStmt.FromEpoch != 201 {
		t.Fatalf("nextStmt.FromEpoch = %d, want clamped to 201", nextStmt.FromEpoch)
	}
}

func TestEnsureOperatorHotkeyWallet_AdvancePastExistingEntry(t *testing.T) {
	stateDir := t.TempDir()
	store := hotkeyWalletStore(stateDir)

	op := newFakeOperator(t)
	op.mu.Lock()
	op.epoch = 10 // epochResult.Epoch + 2 = 12
	op.mu.Unlock()

	hotkeySeed := filepath.Join(stateDir, "hotkey.seed")
	_ = os.WriteFile(hotkeySeed, []byte(strings.Repeat("12", 32)), 0600)
	coldkeySeed := filepath.Join(stateDir, "coldkey.seed")
	_ = os.WriteFile(coldkeySeed, []byte(strings.Repeat("34", 32)), 0600)

	hotkeyKp, _ := loadOperatorHotkey(hotkeySeed)
	coldkeyRaw, _ := crv4.LoadSeedFile(coldkeySeed)
	coldkeyKp, _ := crv4.KeypairFromSeed(coldkeyRaw)

	// Chain has 1 entry
	stmt, err := hotkeywallet.NextStatement(nil, fakeTestDomain.HotkeySubnet(), hotkeyKp.PublicKey(), coldkeyKp.PublicKey(), 1, 100, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := stmt.Message()
	coldkeySig, _ := hotkeywallet.Sign(coldkeyKp, msg)
	hotkeySig, _ := hotkeywallet.Sign(hotkeyKp, msg)
	_ = store.Append(protocol.HotkeyWalletMappingConsent{
		Message:          msg,
		ColdkeySignature: coldkeySig,
		HotkeySignature:  hotkeySig,
	})
	chain, _ := store.Chain()

	// Operator reports an existing entry with FromEpoch = 20 (> 12)
	// (not adopting because consent_head_hash is different)
	op.mu.Lock()
	op.extraWallets = []map[string]any{
		{
			"coldkey_ss58":       coldkeyKp.Address(),
			"consent_scope":      "hotkey",
			"hotkey_ss58":        hotkeyKp.Address(),
			"from_epoch":         uint64(20),
			"through_epoch":      uint64(100),
			"consent_head_hash":  "0x" + strings.Repeat("ff", 32), // mismatched head
			"consent_generation": uint64(1),
		},
	}
	op.mu.Unlock()

	target := hotkeyWalletTarget{apiUrl: op.server.URL, byJwt: op.byJwt, domain: "test"}
	outcome, err := ensureOperatorHotkeyWallet(context.Background(), target, hotkeyKp, chain)
	if err != nil {
		t.Fatalf("ensureOperatorHotkeyWallet failed: %v", err)
	}

	// Must advance past existing entry: entry.FromEpoch + 1 = 20 + 1 = 21
	if outcome.fromEpoch != 21 {
		t.Fatalf("outcome.fromEpoch = %d, want advanced past existing entry to 21", outcome.fromEpoch)
	}
}

func TestRunHotkeyWalletUpkeepStep_EmptyChain(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	op := newFakeOperator(t)
	hotkeySeed := filepath.Join(stateDir, "hotkey.seed")
	_ = os.WriteFile(hotkeySeed, []byte(strings.Repeat("12", 32)), 0600)
	hotkeyKp, _ := loadOperatorHotkey(hotkeySeed)

	// Chain is empty, and stateDir does not even have a JWT
	var lastGen uint64
	outcome, err := runHotkeyWalletUpkeepStep(context.Background(), op.server.URL, hotkeyKp, &lastGen)
	if err != nil {
		t.Fatalf("expected empty chain upkeep step to return cleanly, got err: %v", err)
	}
	if outcome.delegated || outcome.generation != 0 {
		t.Fatalf("expected zero outcome on empty chain, got: %+v", outcome)
	}

	// Must have made zero calls to operator
	if count := op.callCount("GET /sn/wallet"); count != 0 {
		t.Fatalf("expected 0 calls to operator on empty chain, got %d", count)
	}
	if count := op.callCount("GET /sn/epoch"); count != 0 {
		t.Fatalf("expected 0 calls to operator on empty chain, got %d", count)
	}
}

func TestHotkeyWalletSet_MissingSignatureSource(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", stateDir)

	hotkeySeed := filepath.Join(stateDir, "hotkey.seed")
	_ = os.WriteFile(hotkeySeed, []byte(strings.Repeat("12", 32)), 0600)

	var out bytes.Buffer
	err := hotkeyWalletSet(context.Background(), docopt.Opts{
		"hotkey":             true,
		"set":                true,
		"<coldkey_ss58>":     "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY",
		"--hotkey_seed_file": hotkeySeed,
		"--api_url":          "https://dummy.example",
	}, &out)

	if err == nil || !strings.Contains(err.Error(), "the coldkey's signature is required") {
		t.Fatalf("expected error mentioning 'the coldkey's signature is required', got: %v", err)
	}
}


