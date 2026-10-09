package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/docopt/docopt-go"
	"github.com/urfoundation/sn/crv4"
	"github.com/urfoundation/sn/hotkeywallet"
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
